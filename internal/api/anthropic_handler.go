package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/cli"
	"github.com/bsfdsagfadg/vertex/internal/transform"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

// AnthropicHandler exposes the Anthropic Messages wire format while using the
// same Gemini/Vertex execution path as the OpenAI-compatible endpoints.
type AnthropicHandler struct {
	handler
	reqConv  transform.RequestConverter
	respConv transform.ResponseConverter
}

func (h *AnthropicHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.anthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	body, ok := h.readAnthropicBody(w, r)
	if !ok {
		return
	}
	rawModel := strings.TrimSpace(stringValue(body["model"]))
	if rawModel == "" {
		h.anthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if _, present := body["max_tokens"]; !present {
		h.anthropicError(w, http.StatusBadRequest, "invalid_request_error", "max_tokens is required")
		return
	}
	actualModel, useFake := stripFakePrefix(rawModel, h.cfg.FakePrefixes())
	cli.UpdateReqModel(vertex.RequestIDFromContext(r.Context()), actualModel)

	chatBody, err := anthropicToChatRequest(body)
	if err != nil {
		h.anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	chatBody["model"] = actualModel
	model, payload, err := h.reqConv.Convert(chatBody, h.cfg)
	if err != nil {
		h.anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	transform.ApplyImageConfig(payload, chatBody)

	if protocolBoolValue(body["stream"]) {
		h.streamMessages(r.Context(), w, rawModel, model, payload, useFake || h.cfg.AggregateStream())
		return
	}

	resp, vErr := h.vc.CompleteChat(r.Context(), model, payload)
	if vErr != nil {
		h.writeAnthropicVertexError(w, toVertexError(vErr))
		return
	}
	out := outputFromOAI(h.respConv.ToOAI(resp, model))
	out = completeProtocolUsageWithCountTokens(r.Context(), h.vc, model, payload, out)
	writeJSON(w, http.StatusOK, anthropicMessage(rawModel, "msg_"+reqID24(), out))
}

func (h *AnthropicHandler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.anthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	body, ok := h.readAnthropicBody(w, r)
	if !ok {
		return
	}
	rawModel := strings.TrimSpace(stringValue(body["model"]))
	if rawModel == "" {
		h.anthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	actualModel, _ := stripFakePrefix(rawModel, h.cfg.FakePrefixes())
	chatBody, err := anthropicToChatRequest(body)
	if err != nil {
		h.anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	chatBody["model"] = actualModel
	model, payload, err := h.reqConv.Convert(chatBody, h.cfg)
	if err != nil {
		h.anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": h.vc.CountTokens(r.Context(), model, protocolInputContents(payload)),
	})
}

func (h *AnthropicHandler) readAnthropicBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body, err := decodeJSONObject(r.Body)
	if err == nil {
		return body, true
	}
	status := http.StatusBadRequest
	if isRequestBodyTooLarge(err) {
		status = http.StatusRequestEntityTooLarge
	}
	h.anthropicError(w, status, "invalid_request_error", "invalid JSON request body")
	return nil, false
}

func anthropicToChatRequest(body map[string]any) (map[string]any, error) {
	chat := make(map[string]any, 12)
	for _, pair := range [][2]string{
		{"max_tokens", "max_completion_tokens"}, {"temperature", "temperature"},
		{"top_p", "top_p"}, {"top_k", "top_k"}, {"stop_sequences", "stop"},
	} {
		if v, ok := body[pair[0]]; ok {
			chat[pair[1]] = v
		}
	}
	if thinking, ok := body["thinking"].(map[string]any); ok {
		chat["thinking"] = thinking
	}

	rawMessages, ok := body["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, fmt.Errorf("messages must be a non-empty array")
	}
	messages := make([]any, 0, len(rawMessages)+1)
	if system := anthropicText(body["system"]); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	for _, raw := range rawMessages {
		message, _ := raw.(map[string]any)
		if message == nil {
			continue
		}
		role := stringValue(message["role"])
		if role == "system" || role == "developer" {
			if content := anthropicText(message["content"]); content != "" {
				messages = append(messages, map[string]any{"role": "system", "content": content})
			}
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("message role %q must be 'user' or 'assistant'", role)
		}
		var err error
		messages, err = appendAnthropicMessageToChat(messages, role, message["content"])
		if err != nil {
			return nil, err
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("messages did not contain supported content")
	}
	chat["messages"] = messages

	if rawTools, ok := body["tools"].([]any); ok {
		tools := make([]any, 0, len(rawTools))
		for _, raw := range rawTools {
			tool, _ := raw.(map[string]any)
			if tool == nil || stringValue(tool["name"]) == "" {
				continue
			}
			fn := map[string]any{
				"name": tool["name"], "description": tool["description"],
				"parameters": defaultAny(tool["input_schema"], map[string]any{"type": "object", "properties": map[string]any{}}),
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if len(tools) > 0 {
			chat["tools"] = tools
		}
	}
	if choice, ok := body["tool_choice"].(map[string]any); ok {
		switch stringValue(choice["type"]) {
		case "auto":
			chat["tool_choice"] = "auto"
		case "any":
			chat["tool_choice"] = "required"
		case "none":
			chat["tool_choice"] = "none"
		case "tool":
			chat["tool_choice"] = map[string]any{
				"type": "function", "function": map[string]any{"name": choice["name"]},
			}
		}
	}
	return chat, nil
}

func anthropicText(v any) string {
	if text, ok := v.(string); ok {
		return text
	}
	var inline [8]string
	values := inline[:0]
	for _, raw := range anySlice(v) {
		block, _ := raw.(map[string]any)
		if stringValue(block["type"]) == "text" {
			values = append(values, stringValue(block["text"]))
		}
	}
	return strings.Join(values, "\n")
}

func appendAnthropicMessageToChat(result []any, role string, content any) ([]any, error) {
	if text, ok := content.(string); ok {
		return append(result, map[string]any{"role": role, "content": text}), nil
	}
	if role == "assistant" {
		var text transform.StringAccumulator
		toolCalls := []any{}
		for _, raw := range anySlice(content) {
			block, _ := raw.(map[string]any)
			switch stringValue(block["type"]) {
			case "text":
				text.WriteString(stringValue(block["text"]))
			case "tool_use":
				toolCalls = append(toolCalls, map[string]any{
					"id": block["id"], "type": "function",
					"function": map[string]any{"name": block["name"], "arguments": jsonString(block["input"])},
				})
			}
		}
		msg := map[string]any{"role": "assistant", "content": text.String()}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		return append(result, msg), nil
	}

	regular := []any{}
	flushRegular := func() {
		if len(regular) > 0 {
			result = append(result, map[string]any{"role": "user", "content": regular})
			regular = nil
		}
	}
	for _, raw := range anySlice(content) {
		block, _ := raw.(map[string]any)
		if block == nil {
			continue
		}
		switch stringValue(block["type"]) {
		case "text":
			regular = append(regular, map[string]any{"type": "text", "text": block["text"]})
		case "image":
			source, _ := block["source"].(map[string]any)
			switch stringValue(source["type"]) {
			case "base64":
				url := "data:" + stringValue(source["media_type"]) + ";base64," + stringValue(source["data"])
				regular = append(regular, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			case "url":
				regular = append(regular, map[string]any{"type": "image_url", "image_url": map[string]any{"url": source["url"]}})
			}
		case "tool_result":
			flushRegular()
			result = append(result, map[string]any{
				"role": "tool", "tool_call_id": block["tool_use_id"], "content": anthropicToolResult(block["content"]),
			})
		}
	}
	flushRegular()
	return result, nil
}

func anthropicToolResult(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if text := anthropicText(v); text != "" {
		return text
	}
	return jsonString(v)
}

func anthropicMessage(model, id string, out protocolOutput) map[string]any {
	content := []any{}
	if out.Reasoning != "" {
		// 非流式响应同样需要非空 signature，否则 Claude Code SDK 在下一轮对话历史中会拒绝该 thinking 块。
		content = append(content, map[string]any{"type": "thinking", "thinking": out.Reasoning, "signature": thinkingSignature(out.Reasoning)})
	}
	if out.Text != "" || len(out.ToolCalls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": out.Text})
	}
	for _, tc := range out.ToolCalls {
		content = append(content, map[string]any{
			"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": jsonValue(tc.Arguments),
		})
	}
	return map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": anthropicStopReason(out.Finish, len(out.ToolCalls) > 0),
		"stop_sequence": nil,
		"usage":         anthropicUsage(out),
	}
}

func anthropicUsage(out protocolOutput) map[string]any {
	totalInput := max(out.Input, 0)
	cacheRead := min(max(out.CachedInputTokens, 0), totalInput)
	usage := map[string]any{
		"input_tokens":                totalInput - cacheRead,
		"output_tokens":               max(out.Output, 0),
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     cacheRead,
	}
	if thinking := min(max(out.ReasoningTokens, 0), max(out.Output, 0)); thinking > 0 {
		usage["output_tokens_details"] = map[string]any{"thinking_tokens": thinking}
	}
	return usage
}

func anthropicStopReason(finish string, hasTools bool) string {
	if hasTools || strings.EqualFold(finish, "tool_calls") {
		return "tool_use"
	}
	switch strings.ToLower(finish) {
	case "length", "max_tokens":
		return "max_tokens"
	case "content_filter", "safety", "recitation", "prohibited_content":
		return "refusal"
	default:
		return "end_turn"
	}
}

func (h *AnthropicHandler) streamMessages(
	ctx context.Context,
	w http.ResponseWriter,
	displayModel, model string,
	payload map[string]any,
	aggregate bool,
) {
	state := &anthropicStreamState{
		sw: newSSEWriter(w, "text/event-stream"), id: "msg_" + reqID24(), model: displayModel,
	}
	state.start()

	// Claude Code SDK 期望流式连接中每 ~15 秒收到一个 ping 事件保活。
	// 长思考场景下 Gemini 可能 10-30 秒不发内容，没有 ping 会导致 SDK 误判超时断连。
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	var pingWg sync.WaitGroup
	pingWg.Add(1)
	go func() {
		defer pingWg.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				if !state.emitPing() {
					return
				}
			}
		}
	}()

	if aggregate {
		resp, err := h.vc.CompleteChat(ctx, model, payload)
		if err != nil {
			pingCancel()
			pingWg.Wait()
			state.fail(toVertexError(err))
			return
		}
		out := completeProtocolUsageWithCountTokens(ctx, h.vc, model, payload, outputFromOAI(h.respConv.ToOAI(resp, model)))
		state.consume(out)
		pingCancel()
		pingWg.Wait()
		state.finish()
		return
	}
	failed := false
	var lastCandidateTokenCount int
	h.vc.StreamChat(ctx, model, payload, func(chunk vertex.StreamChunk) bool {
		if chunk.Err != nil {
			state.fail(chunk.Err)
			failed = true
			return false
		}
		data := chunk.Data
		normalizedUsage, hasUsage := normalizeStreamingGeminiUsage(data, &lastCandidateTokenCount)
		state.consume(outputFromGeminiChunkWithUsage(data, normalizedUsage, hasUsage))
		return true
	})
	pingCancel()
	pingWg.Wait()
	if !failed {
		state.out = completeProtocolUsageWithCountTokens(ctx, h.vc, model, payload, state.output())
		state.finish()
	}
}

type anthropicStreamState struct {
	sw                  *sseWriter
	id                  string
	model               string
	index               int
	openType            string
	text                transform.StringAccumulator
	blockThinking       transform.StringAccumulator
	reasoningBlocks     []string
	reasoningCache      string
	reasoningCacheValid bool
	deltaEvent          anthropicContentBlockDeltaEvent
	out                 protocolOutput
	mu                  sync.Mutex // 保护 sw 的并发写（ping goroutine 与主回调可能竞争）
}

type anthropicContentBlockDeltaEvent struct {
	Type  string                     `json:"type"`
	Index int                        `json:"index"`
	Delta anthropicContentBlockDelta `json:"delta"`
}

type anthropicContentBlockDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

func (s *anthropicStreamState) emit(event string, fields map[string]any) {
	fields["type"] = event
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.sw.writeNamed(event, fields)
}

func (s *anthropicStreamState) emitContentBlockDelta(delta anthropicContentBlockDelta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deltaEvent = anthropicContentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: s.index,
		Delta: delta,
	}
	_ = s.sw.writeNamed("content_block_delta", &s.deltaEvent)
}

// emitPing 发送一个 Anthropic 协议的 ping 保活事件。
func (s *anthropicStreamState) emitPing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sw.write("event: ping\ndata: {}\n\n")
}

func (s *anthropicStreamState) start() {
	s.emit("message_start", map[string]any{"message": map[string]any{
		"id": s.id, "type": "message", "role": "assistant", "model": s.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": anthropicUsage(protocolOutput{}),
	}})
}

func (s *anthropicStreamState) consume(chunk protocolOutput) {
	if chunk.Input > 0 {
		s.out.Input = chunk.Input
	}
	if chunk.Output > 0 {
		s.out.Output = chunk.Output
	}
	if chunk.Total > 0 {
		s.out.Total = chunk.Total
	}
	if chunk.CachedInputTokens > 0 {
		s.out.CachedInputTokens = chunk.CachedInputTokens
	}
	if chunk.ReasoningTokens > 0 {
		s.out.ReasoningTokens = chunk.ReasoningTokens
	}
	if chunk.Finish != "" {
		s.out.Finish = chunk.Finish
	}
	if chunk.Reasoning != "" {
		if s.openType != "thinking" {
			s.closeBlock()
			s.openType = "thinking"
			s.blockThinking.Reset()
			s.emit("content_block_start", map[string]any{
				"index": s.index, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
			})
		}
		s.emitContentBlockDelta(anthropicContentBlockDelta{
			Type: "thinking_delta", Thinking: chunk.Reasoning,
		})
		s.blockThinking.WriteString(chunk.Reasoning)
		s.reasoningCacheValid = false
	}
	if chunk.Text != "" {
		if s.openType != "text" {
			s.closeBlock()
			s.openType = "text"
			s.emit("content_block_start", map[string]any{
				"index": s.index, "content_block": map[string]any{"type": "text", "text": ""},
			})
		}
		s.text.WriteString(chunk.Text)
		s.emitContentBlockDelta(anthropicContentBlockDelta{Type: "text_delta", Text: chunk.Text})
	}
	for _, tc := range chunk.ToolCalls {
		s.closeBlock()
		s.emit("content_block_start", map[string]any{
			"index": s.index,
			"content_block": map[string]any{
				"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": map[string]any{},
			},
		})
		s.emitContentBlockDelta(anthropicContentBlockDelta{
			Type: "input_json_delta", PartialJSON: tc.Arguments,
		})
		s.emit("content_block_stop", map[string]any{"index": s.index})
		s.index++
		s.out.ToolCalls = append(s.out.ToolCalls, tc)
	}
}

func (s *anthropicStreamState) closeBlock() {
	if s.openType == "" {
		return
	}
	if s.openType == "thinking" {
		// Claude Code SDK 要求 thinking 块在 content_block_stop 前必须有非空
		// signature_delta。连续思考增量必须共享同一个块，并对完整块签名。
		thinking := s.blockThinking.String()
		s.emitContentBlockDelta(anthropicContentBlockDelta{
			Type: "signature_delta", Signature: thinkingSignature(thinking),
		})
		s.reasoningBlocks = append(s.reasoningBlocks, thinking)
		s.blockThinking.Reset()
	}
	s.emit("content_block_stop", map[string]any{"index": s.index})
	s.index++
	s.openType = ""
}

func (s *anthropicStreamState) finish() {
	s.closeBlock()
	s.out = s.output()
	s.emit("message_delta", map[string]any{
		"delta": map[string]any{
			"stop_reason":   anthropicStopReason(s.out.Finish, len(s.out.ToolCalls) > 0),
			"stop_sequence": nil,
		},
		"usage": anthropicUsage(s.out),
	})
	s.emit("message_stop", map[string]any{})
}

func (s *anthropicStreamState) output() protocolOutput {
	out := s.out
	out.Text = s.text.String()
	if !s.reasoningCacheValid {
		current := ""
		if s.openType == "thinking" {
			current = s.blockThinking.String()
		}
		s.reasoningCache = joinProtocolTextBlocks(s.reasoningBlocks, current)
		s.reasoningCacheValid = true
	}
	out.Reasoning = s.reasoningCache
	return out
}

// thinkingSignature 生成一个伪签名供 Claude Code SDK 校验。
// Anthropic 原生签名是服务端加密的，这里取 thinking 内容的 SHA-256 前 32 字节
// 做 base16 编码，保证：1) 非空 2) 同内容同签名 3) 不同内容不同签名。
func thinkingSignature(thinking string) string {
	sum := sha256.Sum256([]byte(thinking))
	return hex.EncodeToString(sum[:32])
}

func (s *anthropicStreamState) fail(err *vertex.VertexError) {
	s.emit("error", map[string]any{"error": map[string]any{
		"type": anthropicErrorType(err), "message": vertex.FriendlyErrorMessage(err),
	}})
}

func (h *AnthropicHandler) anthropicError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error", "error": map[string]any{"type": typ, "message": message},
		"request_id": "req_" + reqID24(),
	})
}

func (h *AnthropicHandler) writeAnthropicVertexError(w http.ResponseWriter, err *vertex.VertexError) {
	h.anthropicError(w, err.Code, anthropicErrorType(err), vertex.FriendlyErrorMessage(err))
}

func anthropicErrorType(err *vertex.VertexError) string {
	switch err.Kind {
	case "invalid":
		return "invalid_request_error"
	case "auth":
		return "authentication_error"
	case "permission":
		return "permission_error"
	case "notfound":
		return "not_found_error"
	case "ratelimit":
		return "rate_limit_error"
	default:
		if err.Code == http.StatusServiceUnavailable {
			return "overloaded_error"
		}
		return "api_error"
	}
}
