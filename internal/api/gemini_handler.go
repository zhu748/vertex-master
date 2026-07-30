package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/transform"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

type GeminiHandler struct {
	handler
}

func (g *GeminiHandler) handleModelsSubtree(w http.ResponseWriter, r *http.Request) {
	var rest string
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1beta/models/"):
		rest = strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
	case strings.HasPrefix(r.URL.Path, "/v1/models/"):
		rest = strings.TrimPrefix(r.URL.Path, "/v1/models/")
	default:
		oaiError(w, http.StatusNotFound, "not found", "invalid_request_error")
		return
	}
	if rest == "" {
		oaiError(w, http.StatusNotFound, "not found", "invalid_request_error")
		return
	}

	model := rest
	method := ""
	if idx := strings.LastIndex(rest, ":"); idx != -1 {
		model = rest[:idx]
		method = rest[idx+1:]
	}

	switch method {
	case "":
		if r.Method != http.MethodGet {
			oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		g.handleModelInfo(w, model)
	case "generateContent":
		g.requirePost(w, r, func() { g.handleGeminiGenerate(w, r, model) })
	case "streamGenerateContent":
		g.requirePost(w, r, func() { g.handleGeminiStreamGenerate(w, r, model) })
	case "countTokens":
		g.requirePost(w, r, func() { g.handleCountTokens(w, r, model) })
	default:
		oaiError(w, http.StatusNotFound, "未知方法 "+method+" (unknown method)", "invalid_request_error")
	}
}

func (g *GeminiHandler) requirePost(w http.ResponseWriter, r *http.Request, fn func()) {
	if r.Method != http.MethodPost {
		oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	fn()
}

func (g *GeminiHandler) readGeminiBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body, err := decodeJSONObject(r.Body)
	if err != nil {
		if isRequestBodyTooLarge(err) {
			g.geminiError(w, http.StatusRequestEntityTooLarge, "请求体过大 (request body too large)", "RESOURCE_EXHAUSTED")
			return nil, false
		}
		if strings.Contains(err.Error(), "invalid UTF-8") {
			g.geminiError(w, http.StatusBadRequest, "请求体编码错误，需为 UTF-8 (request body must be UTF-8 encoded)", "INVALID_ARGUMENT")
			return nil, false
		}
		g.geminiError(w, http.StatusBadRequest, "请求格式错误，JSON 解析失败 (invalid JSON)", "INVALID_ARGUMENT")
		return nil, false
	}
	body, err = normalizeGeminiRequestBody(body)
	if err != nil {
		g.geminiError(
			w,
			http.StatusBadRequest,
			"请求内容格式错误 (invalid request content): "+err.Error(),
			"INVALID_ARGUMENT",
		)
		return nil, false
	}
	return body, true
}

func (g *GeminiHandler) handleGeminiGenerate(w http.ResponseWriter, r *http.Request, model string) {
	actualModel, _ := resolveRequestedModel(model, g.cfg)
	body, ok := g.readGeminiBody(w, r)
	if !ok {
		return
	}
	prefill := transform.AdaptGemini36Prefill(g.cfg.ResolveModelName(actualModel), body)
	log.Printf("[Server] [GeminiGenerate] 收到请求: 模型=%s, 真模型=%s", model, actualModel)

	resp, vErr := g.vc.CompleteChat(r.Context(), actualModel, body)
	if vErr != nil {
		ve := toVertexError(vErr)
		if isSafetyBlock(ve) {
			writeJSON(w, http.StatusOK, geminiSafetyResponse(ve))
			return
		}
		writeJSON(w, ve.Code, vertexErrorToGemini(ve))
		return
	}
	transform.StripAssistantPrefillFromGemini(resp, prefill)
	cleanGeminiFinishReason(resp)
	cleanGeminiPromptFeedback(resp)
	applyGeminiUsage(
		completeProtocolUsageWithCountTokens(r.Context(), g.vc, actualModel, body, outputFromGeminiChunk(resp)),
		resp,
	)
	rewriteGeminiIDs(resp, generateVPSuffix())
	writeJSON(w, http.StatusOK, resp)
}

func (g *GeminiHandler) handleGeminiStreamGenerate(w http.ResponseWriter, r *http.Request, model string) {
	actualModel, useFake := resolveRequestedModel(model, g.cfg)
	body, ok := g.readGeminiBody(w, r)
	if !ok {
		return
	}
	transform.AdaptGemini36Prefill(g.cfg.ResolveModelName(actualModel), body)
	log.Printf("[Server] [GeminiStreamGenerate] 收到请求: 模型=%s, 真模型=%s, 假流式=%v", model, actualModel, useFake)

	sw := newSSEWriter(w, "text/event-stream")

	if useFake {
		g.geminiFakeStream(r.Context(), sw, actualModel, body)
		return
	}

	gotChunk := false
	hasFinish := false
	streamErrWritten := false
	suffix := generateVPSuffix()
	var lastCandidateTokenCount int
	var streamOutput protocolOutputAccumulator
	prefillFilter := transform.NewAssistantPrefillStreamFilter(
		transform.AssistantPrefillFromPayload(body),
	)
	var textStreamEncoder geminiTextStreamEncoder
	textStreamEncoder.init()
	g.vc.StreamChat(r.Context(), actualModel, body, func(ch vertex.StreamChunk) bool {
		if sw.failed.Load() {
			return false
		}
		if ch.Err != nil {
			streamErrWritten = true
			if isSafetyBlock(ch.Err) {
				_ = sw.writeData(geminiSafetyChunk(ch.Err))
			} else {
				_ = sw.writeData(map[string]any{"error": map[string]any{
					"code": ch.Err.Code, "message": vertex.FriendlyErrorMessage(ch.Err), "status": geminiStatusOf(ch.Err),
				}})
			}
			return false
		}
		gotChunk = true
		// StreamChunk 把胜出节点 chunk 的所有权交给当前单一消费者，可直接
		// 就地清理和补齐字段，避免每帧递归复制整棵响应对象。
		data := ch.Data
		prefillFilter.FilterGeminiChunk(data)
		normalizedUsage, hasUsage := normalizeStreamingGeminiUsage(data, &lastCandidateTokenCount)
		streamOutput.Add(outputFromGeminiChunkWithUsage(data, normalizedUsage, hasUsage))
		if fr := cleanGeminiFinishReason(data); fr != "" {
			hasFinish = true
		}
		cleanGeminiPromptFeedback(data)
		rewriteGeminiIDs(data, suffix)
		return textStreamEncoder.writeData(sw, data)
	})
	if sw.failed.Load() {
		return
	}

	if streamErrWritten {
		return
	}
	if tails := prefillFilter.FinalizeGemini(); len(tails) > 0 {
		candidates := make([]any, 0, len(tails))
		for _, tail := range tails {
			candidates = append(candidates, map[string]any{
				"index": tail.Index,
				"content": map[string]any{
					"role": "model", "parts": []any{map[string]any{"text": tail.Text}},
				},
			})
		}
		tailChunk := map[string]any{"candidates": candidates}
		if !textStreamEncoder.writeData(sw, tailChunk) {
			return
		}
		streamOutput.Add(outputFromGeminiChunk(tailChunk))
	}
	if !gotChunk {
		_ = sw.writeData(map[string]any{
			"error": map[string]any{
				"code": 500, "message": "Upstream returned empty response (no content)", "status": "INTERNAL",
			},
		})
		return
	}
	completedOutput := completeProtocolUsageWithCountTokens(
		r.Context(),
		g.vc,
		actualModel,
		body,
		streamOutput.Output(),
	)
	if hasProtocolUsage(completedOutput) {
		usageChunk := map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{}, "role": "model"},
				"index":   0,
			}},
		}
		if !hasFinish {
			usageChunk["candidates"].([]any)[0].(map[string]any)["finishReason"] = "STOP"
		}
		applyGeminiUsage(completedOutput, usageChunk)
		_ = sw.writeData(usageChunk)
	} else if !hasFinish {
		_ = sw.writeData(map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"parts": []any{}, "role": "model"},
				"finishReason": "STOP",
				"index":        0,
			}},
		})
	}
}

func normalizeStreamingGeminiUsage(data map[string]any, lastCandidateTokenCount *int) (transform.NormalizedUsage, bool) {
	if candidate := firstGeminiCandidate(data); candidate != nil {
		for _, key := range []string{"tokenCount", "token_count"} {
			if count := protocolIntValue(candidate[key]); count > 0 {
				*lastCandidateTokenCount = count
				break
			}
		}
	}
	return ensureGeminiUsageCandidate(data, *lastCandidateTokenCount)
}

// ensureGeminiUsageCandidate 兼容会忽略 metadata-only SSE 帧的 Gemini 客户端。
// RikkaHub 的 GoogleProvider 只有在 candidates 非空时才继续解析 usageMetadata，
// 因此对有真实 token 统计但无 candidates 的末帧补一个不含正文的空 candidate。
func ensureGeminiUsageCandidate(data map[string]any, fallbackTokenCount int) (transform.NormalizedUsage, bool) {
	usage, ok := data["usageMetadata"].(map[string]any)
	if !ok || len(usage) == 0 {
		return transform.NormalizedUsage{}, false
	}
	candidate := firstGeminiCandidate(data)
	usageCandidate := candidate
	if usageCandidate == nil && fallbackTokenCount > 0 {
		// 仅 usage-only 末帧临时构造最小候选，不跨 chunk 保留可能很大的正文。
		usageCandidate = map[string]any{"tokenCount": fallbackTokenCount}
	}
	normalized := transform.NormalizeUsageForCandidate(usage, usageCandidate)
	input := normalized.PromptTokens
	output := normalized.CompletionTokens
	total := normalized.TotalTokens
	if input == 0 && output == 0 && total == 0 {
		return normalized, true
	}

	// 补 canonical Gemini 汇总字段，让只读取顶层计数、不读取 modality details
	// 的客户端也能获得输入/输出用量。
	if protocolIntValue(usage["promptTokenCount"]) == 0 && input > 0 {
		usage["promptTokenCount"] = input
	}
	if protocolIntValue(usage["candidatesTokenCount"])+protocolIntValue(usage["thoughtsTokenCount"]) == 0 && output > 0 {
		usage["candidatesTokenCount"] = output
	}
	if protocolIntValue(usage["totalTokenCount"]) == 0 && total > 0 {
		usage["totalTokenCount"] = total
	}

	if firstGeminiCandidate(data) == nil {
		data["candidates"] = []any{map[string]any{
			"index": 0,
			"content": map[string]any{
				"role":  "model",
				"parts": []any{},
			},
		}}
	}
	return normalized, true
}

func firstGeminiCandidate(data map[string]any) map[string]any {
	candidates, ok := data["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return nil
	}
	candidate, _ := candidates[0].(map[string]any)
	return candidate
}

func (g *GeminiHandler) geminiFakeStream(ctx context.Context, sw *sseWriter, model string, body map[string]any) {
	resp, vErr := g.vc.CompleteChat(ctx, model, body)
	if vErr != nil {
		ve := toVertexError(vErr)
		if isSafetyBlock(ve) {
			_ = sw.writeData(geminiSafetyChunk(ve))
			return
		}
		_ = sw.writeData(map[string]any{"error": map[string]any{
			"code": ve.Code, "message": vertex.FriendlyErrorMessage(ve), "status": geminiStatusOf(ve),
		}})
		return
	}

	transform.StripAssistantPrefillFromGemini(
		resp,
		transform.AssistantPrefillFromPayload(body),
	)
	cleanGeminiFinishReason(resp)
	cleanGeminiPromptFeedback(resp)
	rewriteGeminiIDs(resp, generateVPSuffix())
	out := completeProtocolUsageWithCountTokens(ctx, g.vc, model, body, outputFromGeminiChunk(resp))
	for _, chunk := range geminiFakeStreamFrames(resp) {
		if !sw.writeData(chunk) {
			return
		}
	}
	if hasProtocolUsage(out) {
		usageChunk := map[string]any{"candidates": []any{map[string]any{
			"index": 0, "content": map[string]any{"role": "model", "parts": []any{}},
		}}}
		applyGeminiUsage(out, usageChunk)
		_ = sw.writeData(usageChunk)
	}
}

func geminiFakeStreamFrames(resp map[string]any) []map[string]any {
	candidates, _ := resp["candidates"].([]any)
	frames := make([]map[string]any, 0, fakeStreamTargetChunks)
	for candidateIndex, rawCandidate := range candidates {
		candidate, ok := rawCandidate.(map[string]any)
		if !ok {
			continue
		}
		content, _ := candidate["content"].(map[string]any)
		role := stringValue(content["role"])
		if role == "" {
			role = "model"
		}
		start := len(frames)
		for _, rawPart := range anySlice(content["parts"]) {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			text, hasText := part["text"].(string)
			textChunks := splitIntoRuneChunks(text)
			if !hasText || len(textChunks) == 0 {
				frames = append(frames, geminiFakePartFrame(candidateIndex, role, part))
				continue
			}
			for _, piece := range textChunks {
				partChunk := make(map[string]any, len(part))
				for key, value := range part {
					partChunk[key] = value
				}
				partChunk["text"] = piece
				frames = append(frames, geminiFakePartFrame(candidateIndex, role, partChunk))
			}
		}
		if len(frames) == start {
			frames = append(frames, map[string]any{"candidates": []any{map[string]any{
				"index": candidateIndex,
				"content": map[string]any{
					"role": role, "parts": []any{},
				},
			}}})
		}

		finalCandidate := firstGeminiCandidate(frames[len(frames)-1])
		for key, value := range candidate {
			if key != "content" && key != "index" {
				finalCandidate[key] = value
			}
		}
		if originalIndex, exists := candidate["index"]; exists {
			finalCandidate["index"] = originalIndex
		}
		for key, value := range content {
			if key != "parts" && key != "role" {
				finalCandidate["content"].(map[string]any)[key] = value
			}
		}
	}

	topMetadata := make(map[string]any, 5)
	for _, key := range []string{
		"createTime", "modelVersion", "responseId", "promptFeedback", "modelStatus",
	} {
		if value, exists := resp[key]; exists {
			topMetadata[key] = value
		}
	}
	if len(topMetadata) > 0 {
		if len(frames) == 0 {
			frames = append(frames, topMetadata)
		} else {
			for key, value := range topMetadata {
				frames[len(frames)-1][key] = value
			}
		}
	}
	return frames
}

func geminiFakePartFrame(candidateIndex int, role string, part map[string]any) map[string]any {
	return map[string]any{"candidates": []any{map[string]any{
		"index": candidateIndex,
		"content": map[string]any{
			"role": role, "parts": []any{part},
		},
	}}}
}

func (g *GeminiHandler) handleCountTokens(w http.ResponseWriter, r *http.Request, model string) {
	actualModel, _ := resolveRequestedModel(model, g.cfg)
	body, ok := g.readGeminiBody(w, r)
	if !ok {
		return
	}
	log.Printf("[Server] [CountTokens] 收到请求: 模型=%s, 真模型=%s", model, actualModel)

	total, countErr := g.vc.CountTokensExact(r.Context(), actualModel, protocolInputContents(body))
	if countErr != nil {
		ve := toVertexError(countErr)
		writeJSON(w, ve.Code, vertexErrorToGemini(ve))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"totalTokens": total})
}

func (g *GeminiHandler) handleModelInfo(w http.ResponseWriter, modelName string) {
	name := strings.TrimPrefix(modelName, "models/")
	known := false
	for _, m := range g.cfg.ModelsWithFakeVariants() {
		if m == name {
			known = true
			break
		}
	}
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"code": 404, "message": "Model '" + modelName + "' not found.", "status": "NOT_FOUND",
		}})
		return
	}
	writeJSON(w, http.StatusOK, geminiModelInfo(name))
}

func (g *GeminiHandler) geminiSSE(obj map[string]any) string {
	return sseEvent(obj)
}

func (g *GeminiHandler) geminiError(w http.ResponseWriter, status int, msg, geminiStatus string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": status, "message": msg, "status": geminiStatus,
	}})
}

func cleanGeminiFinishReason(data map[string]any) string {
	cands, ok := data["candidates"].([]any)
	if !ok || len(cands) == 0 {
		return ""
	}
	var realFR string
	for _, candRaw := range cands {
		cand, ok := candRaw.(map[string]any)
		if !ok {
			continue
		}
		fr, _ := cand["finishReason"].(string)
		if fr == "FINISH_REASON_UNSPECIFIED" {
			delete(cand, "finishReason")
		} else if fr != "" {
			if realFR == "" {
				realFR = fr
			}
		}
	}
	return realFR
}

// cleanGeminiPromptFeedback 清理 promptFeedback 中的 protobuf 默认值占位符。
//
// 匿名 Gemini 上游经常在正常响应里附带
//
//	promptFeedback: { blockReason: "BLOCKED_REASON_UNSPECIFIED" }
//
// 这个值是 protobuf 枚举的默认值（0），并不是真拦截 —— candidates 里通常有实际内容。
// 但很多 Gemini SDK 客户端（含 Google 官方 SDK）只要看到 blockReason 字段非空就判定为
// 拦截，会丢弃 candidates 里的内容或报错。
//
// 这里在透传前删除这个无害的占位符；只有真正的拦截原因（SAFETY / RECITATION 等）才保留。
// 真正被拦截时 vertex 层会走 isSafetyBlock 分支返回 geminiSafetyResponse，不会走到这里。
//
// StreamChunk.Data 的所有权已转移给当前消费者，因此这里可以安全地就地清理。
func cleanGeminiPromptFeedback(data map[string]any) {
	feedback, ok := data["promptFeedback"].(map[string]any)
	if !ok {
		return
	}
	reason, _ := feedback["blockReason"].(string)
	if strings.EqualFold(reason, "BLOCKED_REASON_UNSPECIFIED") ||
		strings.EqualFold(reason, "BLOCK_REASON_UNSPECIFIED") ||
		reason == "" {
		delete(feedback, "blockReason")
		delete(feedback, "blockReasonMessage")
	}
	if len(feedback) == 0 {
		delete(data, "promptFeedback")
	}
}

func vertexErrorToGemini(e *vertex.VertexError) map[string]any {
	msg := withUpstreamDetail(vertex.FriendlyErrorMessage(e), e)
	return map[string]any{"error": map[string]any{
		"code": e.Code, "message": msg, "status": geminiStatusOf(e),
	}}
}

func geminiStatusOf(e *vertex.VertexError) string {
	if e != nil && e.Status != "" {
		return e.Status
	}
	return "INTERNAL"
}

func geminiSafetyResponse(e *vertex.VertexError) map[string]any {
	blockReason := e.Status
	if blockReason == "" {
		blockReason = "SAFETY"
	}
	return map[string]any{
		"candidates": []any{},
		"promptFeedback": map[string]any{
			"blockReason":        blockReason,
			"safetyRatings":      []any{},
			"blockReasonMessage": e.Message,
		},
	}
}

func geminiSafetyChunk(e *vertex.VertexError) map[string]any {
	blockReason := e.Status
	if blockReason == "" {
		blockReason = "SAFETY"
	}
	return map[string]any{
		"candidates": []any{map[string]any{
			"content":       map[string]any{"parts": []any{}, "role": "model"},
			"finishReason":  "SAFETY",
			"safetyRatings": []any{},
			"index":         0,
		}},
		"promptFeedback": map[string]any{
			"blockReason":        blockReason,
			"safetyRatings":      []any{},
			"blockReasonMessage": e.Message,
		},
	}
}

func geminiResponseText(resp map[string]any) string {
	textLength := 0
	maximumInt := int(^uint(0) >> 1)
	visitGeminiResponseText(resp, func(text string) {
		if textLength < 0 {
			return
		}
		if len(text) > maximumInt-textLength {
			textLength = -1
			return
		}
		textLength += len(text)
	})

	var sb strings.Builder
	if textLength > 0 {
		sb.Grow(textLength)
	}
	visitGeminiResponseText(resp, func(text string) {
		sb.WriteString(text)
	})
	return sb.String()
}

func visitGeminiResponseText(resp map[string]any, visit func(string)) {
	cands, _ := resp["candidates"].([]any)
	for _, cRaw := range cands {
		c, ok := cRaw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := c["content"].(map[string]any)
		if !ok {
			continue
		}
		parts, _ := content["parts"].([]any)
		for _, pRaw := range parts {
			p, ok2 := pRaw.(map[string]any)
			if !ok2 {
				continue
			}
			if isTruthyAny(p["thought"]) {
				continue
			}
			if t, ok3 := p["text"].(string); ok3 {
				visit(t)
			}
		}
	}
}

func isTruthyAny(v any) bool { return jsonx.Truthy(v) }
func generateVPSuffix() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("-vp%08x", buf)
}

func rewriteGeminiIDs(val any, suffix string) {
	switch v := val.(type) {
	case map[string]any:
		for k, mv := range v {
			if s, ok := mv.(string); ok && strings.HasPrefix(s, "gemini-tool-call-") {
				v[k] = s + suffix
			} else {
				rewriteGeminiIDs(mv, suffix)
			}
		}
	case []any:
		for _, item := range v {
			rewriteGeminiIDs(item, suffix)
		}
	}
}
