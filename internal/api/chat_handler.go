package api

import (
	"context"
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

	actualModel, useFake := resolveRequestedModel(rawModel, c.cfg)
	body["model"] = actualModel
	cli.UpdateReqModel(vertex.RequestIDFromContext(r.Context()), actualModel)

	stream, _ := body["stream"].(bool)
	aggregateStream := stream && c.cfg.AggregateStream()

	model, geminiPayload, convErr := c.reqConv.Convert(body, c.cfg)
	if convErr != nil {
		oaiError(w, http.StatusBadRequest, "请求参数有误: "+convErr.Error()+" (invalid argument)", "invalid_request_error")
		return
	}
	if prefill := transform.AssistantPrefillFromPayload(geminiPayload); prefill != "" {
		shape := summarizePrompt(geminiPayload)
		log.Printf(
			"[Server] [Prefill] 请求ID=%s, 文本提示摘要=%s, 轮次=%d (user=%d, model=%d, function=%d), "+
				"system=%dB, text=%dB, non_text=%d, prefill=%dB, 假流式=%v, 并发池=%v",
			vertex.RequestIDFromContext(r.Context()),
			shape.Fingerprint,
			shape.Turns,
			shape.UserTurns,
			shape.ModelTurns,
			shape.FunctionTurns,
			shape.SystemBytes,
			shape.TextBytes,
			shape.NonTextParts,
			len(prefill),
			useFake,
			c.cfg.ParallelPoolEnabled(),
		)
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

	sw := newSSEWriter(w, "text/event-stream")
	clientDisconnected := false

	write := func(line string) bool {
		if !sw.write(line) {
			clientDisconnected = true
			log.Printf("[Server] [Stream] 请求ID=%s 客户端已主动断开连接", requestID)
			return false
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
	var textStreamEncoder transform.TextStreamEventEncoder
	if streamEncoder != nil {
		textStreamEncoder, _ = streamEncoder.(transform.TextStreamEventEncoder)
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
		if ch.HasCanonicalText && textStreamEncoder != nil {
			output := outputFromCanonicalTextStreamData(ch.CanonicalText, prefillFilter)
			streamOutput.Add(output)
			first := isFirst
			isFirst = false
			result, ok := textStreamEncoder.EmitText(
				output.Text,
				output.Finish,
				first,
				writeEvent,
			)
			hasFinish = hasFinish || result.HasFinish
			gotContent = gotContent || result.HasContent
			return ok
		}

		data := ch.GeminiData()
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
		return sw.write(line)
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
	out = completeProtocolUsageWithCountTokens(ctx, c.vc, model, geminiPayload, out)
	applyOAIUsage(out, oai)
	finishReason := out.Finish
	if finishReason == "" {
		finishReason = "stop"
	}

	createdTS := time.Now().Unix()
	deltas := syntheticOAIStreamDeltas(out)
	for i, delta := range deltas {
		base := streamChunkBase(model, requestID)
		base["created"] = createdTS
		choice := map[string]any{"index": 0, "delta": delta}
		if i == len(deltas)-1 {
			choice["finish_reason"] = finishReason
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
	out = completeProtocolUsageWithCountTokens(ctx, c.vc, model, geminiPayload, out)
	applyOAIUsage(out, oai)
	finishReason := out.Finish
	if finishReason == "" {
		finishReason = "stop"
	}

	createdTS := time.Now().Unix()
	base := streamChunkBase(model, requestID)
	base["created"] = createdTS

	delta := map[string]any{"role": "assistant"}
	if out.Text != "" {
		delta["content"] = out.Text
	}
	if out.Reasoning != "" {
		delta["reasoning_content"] = out.Reasoning
	}
	if toolCalls := protocolToolCallsToStreamDelta(out.ToolCalls); len(toolCalls) > 0 {
		delta["tool_calls"] = toolCalls
	}
	if len(delta) == 1 {
		delta["content"] = ""
	}
	choice := map[string]any{
		"index": 0,
		"delta": delta,
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
		"finish_reason": finishReason,
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

func syntheticOAIStreamDeltas(out protocolOutput) []map[string]any {
	deltas := make([]map[string]any, 0, fakeStreamTargetChunks*2+1)
	for _, piece := range splitIntoRuneChunks(out.Reasoning) {
		deltas = append(deltas, map[string]any{"reasoning_content": piece})
	}
	for _, piece := range splitIntoRuneChunks(out.Text) {
		deltas = append(deltas, map[string]any{"content": piece})
	}
	if toolCalls := protocolToolCallsToStreamDelta(out.ToolCalls); len(toolCalls) > 0 {
		deltas = append(deltas, map[string]any{"tool_calls": toolCalls})
	}
	if len(deltas) == 0 {
		deltas = append(deltas, map[string]any{"content": ""})
	}
	deltas[0]["role"] = "assistant"
	return deltas
}

func protocolToolCallsToStreamDelta(toolCalls []protocolToolCall) []any {
	if len(toolCalls) == 0 {
		return nil
	}
	deltas := make([]any, 0, len(toolCalls))
	for index, toolCall := range toolCalls {
		id := toolCall.ID
		if id == "" {
			id = "call_" + reqID24()
		}
		deltas = append(deltas, map[string]any{
			"index": index,
			"id":    id,
			"type":  "function",
			"function": map[string]any{
				"name":      toolCall.Name,
				"arguments": toolCall.Arguments,
			},
		})
	}
	return deltas
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
