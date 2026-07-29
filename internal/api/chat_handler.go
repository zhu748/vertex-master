package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/cli"
	"github.com/bsfdsagfadg/vertex/internal/transform"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

type ChatHandler struct {
	handler
	reqConv  transform.RequestConverter
	respConv transform.ResponseConverter
}

func (c *ChatHandler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	var body map[string]any
	body, err := decodeJSONObject(r.Body)
	if err != nil {
		if isRequestBodyTooLarge(err) {
			oaiError(w, http.StatusRequestEntityTooLarge, "请求体过大 (request body too large)", "invalid_request_error")
			return
		}
		if strings.Contains(err.Error(), "invalid UTF-8") {
			oaiError(w, http.StatusBadRequest, "请求体编码错误，需为 UTF-8 (request body must be UTF-8 encoded)", "invalid_request_error")
			return
		}
		oaiError(w, http.StatusBadRequest, "请求格式错误，JSON 解析失败 (invalid JSON)", "invalid_request_error")
		return
	}
	rawModel, _ := body["model"].(string)
	if strings.TrimSpace(rawModel) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": "请求参数有误: 缺少必需字段 model (missing required field 'model')",
			"type":    "invalid_request_error", "code": 400, "param": "model",
		}})
		return
	}

	actualModel, useFake := stripFakePrefix(rawModel, c.cfg.FakePrefixes())
	body["model"] = actualModel
	cli.UpdateReqModel(vertex.RequestIDFromContext(r.Context()), actualModel)

	stream, _ := body["stream"].(bool)
	aggregateStream := stream && c.cfg.AggregateStream()

	model, geminiPayload, convErr := c.reqConv.Convert(body, c.cfg)
	if convErr != nil {
		oaiError(w, http.StatusBadRequest, "请求参数有误: "+convErr.Error()+" (invalid argument)", "invalid_request_error")
		return
	}

	n, nErr := resolveN(body["n"], c.cfg.MaxN())
	if nErr != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": nErr, "type": "invalid_request_error", "code": 400, "param": "n",
		}})
		return
	}
	if stream && n > 1 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": "流式不支持 n>1，请设 stream=false 或 n=1 (streaming supports only n=1; set stream=false or n=1 for multiple choices)",
			"type":    "invalid_request_error", "code": 400, "param": "n",
		}})
		return
	}

	log.Printf("[Server] [ChatCompletions] 收到请求: 模型=%s, 真模型=%s, 流式=%v, n=%d", rawModel, actualModel, stream, n)

	transform.ApplyImageConfig(geminiPayload, body)
	if strings.Contains(strings.ToLower(model), "image") {
		gc, ok := geminiPayload["generationConfig"].(map[string]any)
		if !ok {
			gc = map[string]any{}
			geminiPayload["generationConfig"] = gc
		}
		ic, ok := gc["imageConfig"].(map[string]any)
		if !ok {
			ic = map[string]any{}
			gc["imageConfig"] = ic
		}
		if _, has := ic["imageSize"]; !has {
			ic["imageSize"] = "1K"
		}
	}

	if aggregateStream {
		c.oaiAggregateStream(r.Context(), w, model, geminiPayload)
		return
	}
	if stream && useFake {
		c.oaiFakeStream(r.Context(), w, model, geminiPayload)
		return
	}

	if stream {
		c.streamChatCompletions(r.Context(), w, model, geminiPayload)
		return
	}

	if n > 1 {
		responses, vErr := c.vc.CompleteChatN(r.Context(), model, geminiPayload, n)
		if vErr != nil {
			ve := toVertexError(vErr)
			if isSafetyBlock(ve) {
				log.Printf("[Vertex] 请求被 Google 安全审查拦截, 请求ID=%s, 原因: %s", vertex.RequestIDFromContext(r.Context()), ve.Status)
				writeJSON(w, http.StatusOK, oaiSafetyResponse(model))
				return
			}
			writeJSON(w, ve.Code, vertexErrorToOAI(ve))
			return
		}
		oaiResp := c.respConv.AggregateN(responses, model)
		transform.StripAssistantPrefillFromOAI(
			oaiResp,
			transform.AssistantPrefillFromPayload(geminiPayload),
		)
		writeJSON(w, http.StatusOK, oaiResp)
		return
	}

	geminiResp, vErr := c.vc.CompleteChat(r.Context(), model, geminiPayload)
	if vErr != nil {
		ve := toVertexError(vErr)
		if isSafetyBlock(ve) {
			log.Printf("[Vertex] 请求被 Google 安全审查拦截, 请求ID=%s, 原因: %s", vertex.RequestIDFromContext(r.Context()), ve.Status)
			writeJSON(w, http.StatusOK, oaiSafetyResponse(model))
			return
		}
		writeJSON(w, ve.Code, vertexErrorToOAI(ve))
		return
	}

	oaiResp := c.respConv.ToOAI(geminiResp, model)
	transform.StripAssistantPrefillFromOAI(
		oaiResp,
		transform.AssistantPrefillFromPayload(geminiPayload),
	)
	applyOAIUsage(
		completeProtocolUsageWithCountTokens(r.Context(), c.vc, model, geminiPayload, outputFromOAI(oaiResp)),
		oaiResp,
	)
	writeJSON(w, http.StatusOK, oaiResp)
}

func (c *ChatHandler) streamChatCompletions(ctx context.Context, w http.ResponseWriter, model string, geminiPayload map[string]any) {
	requestID := reqID24()

	flusher, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := &sseWriter{w: w}
	if canFlush {
		sw.flush = flusher.Flush
	}
	clientDisconnected := false

	write := func(line string) bool {
		if _, err := io.WriteString(w, line); err != nil {
			clientDisconnected = true
			log.Printf("[Server] [Stream] 请求ID=%s 客户端已主动断开连接", requestID)
			return false
		}
		if canFlush {
			flusher.Flush()
		}
		return true
	}

	isFirst := true
	hasFinish := false
	gotContent := false
	streamErrWritten := false
	startTime := time.Now()
	prefillFilter := transform.NewAssistantPrefillStreamFilter(
		transform.AssistantPrefillFromPayload(geminiPayload),
	)
	var lastCandidateTokenCount int
	var streamOutput protocolOutputAccumulator
	var streamEncoder transform.StreamEventEncoder
	if converter, ok := c.respConv.(transform.StreamingResponseConverter); ok {
		streamEncoder = converter.NewStreamEventEncoder(model, requestID)
	}
	writeEvent := func(payload any) bool {
		if sw.writeData(payload) {
			return true
		}
		clientDisconnected = true
		log.Printf("[Server] [Stream] 请求ID=%s 客户端已主动断开连接", requestID)
		return false
	}

	c.vc.StreamChat(ctx, model, geminiPayload, func(ch vertex.StreamChunk) bool {
		if clientDisconnected || sw.failed.Load() {
			return false
		}
		if isFirst && ch.Err == nil {
			log.Printf("[Server] [Stream] 请求ID=%s 首字响应耗时: %.2fs", requestID, time.Since(startTime).Seconds())
			cli.UpdateReqState(requestID, "💬 流式打字", "\033[36m", "正在输出...")
		}
		if ch.Err != nil {
			c.writeStreamError(write, ch.Err, requestID, model)
			streamErrWritten = true
			return false
		}
		data := ch.Data
		prefillFilter.FilterGeminiChunk(data)
		normalizedUsage, hasUsage := normalizeStreamingGeminiUsage(data, &lastCandidateTokenCount)
		streamOutput.Add(outputFromGeminiChunkWithUsage(data, normalizedUsage, hasUsage))
		first := isFirst
		isFirst = false
		if streamEncoder != nil {
			result, ok := streamEncoder.Emit(data, first, writeEvent)
			hasFinish = hasFinish || result.HasFinish
			gotContent = gotContent || result.HasContent
			return ok
		}

		events := c.respConv.StreamToSSE(data, model, requestID, first)
		for _, ev := range events {
			if strings.Contains(ev, `"finish_reason"`) && !strings.Contains(ev, `"finish_reason":null`) {
				hasFinish = true
			}
			if strings.Contains(ev, `"content":`) || strings.Contains(ev, `"tool_calls":`) || strings.Contains(ev, `"reasoning_content":`) {
				gotContent = true
			}
			if !write(ev) {
				return false
			}
		}
		return true
	})
	if clientDisconnected || sw.failed.Load() {
		return
	}

	writeSilent := func(line string) bool {
		if _, err := io.WriteString(w, line); err != nil {
			return false
		}
		if canFlush {
			flusher.Flush()
		}
		return true
	}

	if streamErrWritten {
		return
	}
	if tail := prefillFilter.Finalize(); tail != "" {
		base := streamChunkBase(model, requestID)
		base["choices"] = []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": tail},
			"finish_reason": nil,
		}}
		if !writeSilent(sseEvent(base)) {
			return
		}
		gotContent = true
		streamOutput.AddText(tail)
	}
	if !gotContent && !prefillFilter.SawText() {
		ee := vertex.NewEmptyResponseError("Upstream returned empty response (no content)")
		c.writeStreamError(write, ee, requestID, model)
		return
	}
	if !hasFinish {
		base := streamChunkBase(model, requestID)
		base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "length"}}
		if !writeSilent(sseEvent(base)) {
			return
		}
	}
	completedOutput := completeProtocolUsageWithCountTokens(
		ctx,
		c.vc,
		model,
		geminiPayload,
		streamOutput.Output(),
	)
	if !writeOAIStreamUsageValues(sw, completedOutput, model, requestID, time.Now().Unix(), true) {
		return
	}
	writeSilent("data: [DONE]\n\n")
}

func (c *ChatHandler) writeStreamError(write func(string) bool, e *vertex.VertexError, requestID, model string) {
	if isSafetyBlock(e) {
		base := streamChunkBase(model, requestID)
		base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "content_filter"}}
		_ = write(sseEvent(base))
	} else {
		_ = write(sseEvent(vertexErrorToOAI(e)))
	}
	_ = write("data: [DONE]\n\n")
}

func (c *ChatHandler) oaiFakeStream(ctx context.Context, w http.ResponseWriter, model string, geminiPayload map[string]any) {
	requestID := reqID24()
	sw := newSSEWriter(w, "text/event-stream")

	resp, vErr := c.vc.CompleteChat(ctx, model, geminiPayload)
	if vErr != nil {
		ve := toVertexError(vErr)
		c.writeStreamError(sw.write, ve, requestID, model)
		return
	}

	oai := c.respConv.ToOAI(resp, model)
	contentText := transform.StripAssistantPrefillEcho(
		firstChoiceContent(oai),
		transform.AssistantPrefillFromPayload(geminiPayload),
	)
	out := outputFromOAI(oai)
	out.Text = contentText
	applyOAIUsage(completeProtocolUsageWithCountTokens(ctx, c.vc, model, geminiPayload, out), oai)

	createdTS := time.Now().Unix()
	chunks := splitIntoRuneChunks(contentText)
	if len(chunks) == 0 {
		base := streamChunkBase(model, requestID)
		base["created"] = createdTS
		base["choices"] = []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": ""},
			"finish_reason": "stop",
		}}
		if !sw.write(sseEvent(base)) {
			return
		}
		if !writeOAIStreamUsage(sw, oai, model, requestID, createdTS) {
			return
		}
		_ = sw.write("data: [DONE]\n\n")
		return
	}
	for i, piece := range chunks {
		base := streamChunkBase(model, requestID)
		base["created"] = createdTS
		var delta map[string]any
		if i == 0 {
			delta = map[string]any{"role": "assistant", "content": piece}
		} else {
			delta = map[string]any{"content": piece}
		}
		choice := map[string]any{"index": 0, "delta": delta}
		if i == len(chunks)-1 {
			choice["finish_reason"] = "stop"
		}
		base["choices"] = []any{choice}
		if !sw.write(sseEvent(base)) {
			return
		}
	}
	if !writeOAIStreamUsage(sw, oai, model, requestID, createdTS) {
		return
	}
	_ = sw.write("data: [DONE]\n\n")
}

func (c *ChatHandler) oaiAggregateStream(ctx context.Context, w http.ResponseWriter, model string, geminiPayload map[string]any) {
	requestID := reqID24()
	sw := newSSEWriter(w, "text/event-stream")

	resp, vErr := c.vc.CompleteChat(ctx, model, geminiPayload)
	if vErr != nil {
		ve := toVertexError(vErr)
		c.writeStreamError(sw.write, ve, requestID, model)
		return
	}

	oai := c.respConv.ToOAI(resp, model)
	contentText := transform.StripAssistantPrefillEcho(
		firstChoiceContent(oai),
		transform.AssistantPrefillFromPayload(geminiPayload),
	)
	out := outputFromOAI(oai)
	out.Text = contentText
	applyOAIUsage(completeProtocolUsageWithCountTokens(ctx, c.vc, model, geminiPayload, out), oai)

	createdTS := time.Now().Unix()
	base := streamChunkBase(model, requestID)
	base["created"] = createdTS

	choice := map[string]any{
		"index": 0,
		"delta": map[string]any{"role": "assistant", "content": contentText},
	}
	base["choices"] = []any{choice}
	if !sw.write(sseEvent(base)) {
		return
	}

	// Stream end
	baseEnd := streamChunkBase(model, requestID)
	baseEnd["created"] = createdTS
	choiceEnd := map[string]any{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": "stop",
	}
	baseEnd["choices"] = []any{choiceEnd}
	if !sw.write(sseEvent(baseEnd)) {
		return
	}
	if !writeOAIStreamUsage(sw, oai, model, requestID, createdTS) {
		return
	}
	_ = sw.write("data: [DONE]\n\n")
}

// writeOAIStreamUsage 按 OpenAI 流式协议写出独立 usage 块。choices 必须为空，
// 这样 ChatBox、SillyTavern 等客户端能把它识别为统计帧而不是普通内容帧。
type openAIUsageStreamChunk struct {
	Choices []openAIUsageStreamChoice `json:"choices"`
	Created int64                     `json:"created"`
	ID      string                    `json:"id"`
	Model   string                    `json:"model"`
	Object  string                    `json:"object"`
	Usage   transform.OpenAIUsage     `json:"usage"`
}

type openAIUsageStreamChoice struct {
	Delta        struct{} `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
	Index        int      `json:"index"`
}

func writeOAIStreamUsage(
	sw *sseWriter,
	oai map[string]any,
	model, requestID string,
	created int64,
) bool {
	usage, ok := oai["usage"].(map[string]any)
	if !ok || len(usage) == 0 {
		return true
	}
	out := outputFromOAI(map[string]any{"usage": usage})
	return writeOAIStreamUsageValues(sw, out, model, requestID, created, false)
}

func writeOAIStreamUsageValues(
	sw *sseWriter,
	out protocolOutput,
	model, requestID string,
	created int64,
	compatChoice bool,
) bool {
	if out.Input == 0 && out.Output == 0 && out.Total == 0 {
		return true
	}
	if sw == nil {
		return false
	}
	chunk := openAIUsageStreamChunk{
		Choices: []openAIUsageStreamChoice{},
		Created: created,
		ID:      "chatcmpl-" + requestID,
		Model:   model,
		Object:  "chat.completion.chunk",
	}
	if compatChoice {
		// RikkaHub 等客户端在 choices 非空时一定会进入消息处理流程，再合并 usage。
		// 空 delta 不会生成或重复任何正文。
		chunk.Choices = []openAIUsageStreamChoice{{}}
	}
	transform.NormalizedUsage{
		PromptTokens: out.Input, CompletionTokens: out.Output, TotalTokens: out.Total,
		CachedInputTokens: out.CachedInputTokens, ReasoningTokens: out.ReasoningTokens,
	}.FillOpenAIUsage(&chunk.Usage)
	return sw.writeData(&chunk)
}

func firstChoiceContent(oai map[string]any) string {
	choices, ok := oai["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		return ""
	}
	if c, ok2 := msg["content"].(string); ok2 {
		return c
	}
	return ""
}
