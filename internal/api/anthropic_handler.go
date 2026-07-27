package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

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
	contents, _ := payload["contents"].([]any)
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": h.vc.CountTokens(r.Context(), model, contents)})
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
	chat := map[string]any{}
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

	messages := []any{}
	if system := anthropicText(body["system"]); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	rawMessages, ok := body["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, fmt.Errorf("messages must be a non-empty array")
	}
	for _, raw := range rawMessages {
		message, _ := raw.(map[string]any)
		if message == nil {
			continue
		}
		role := stringValue(message["role"])
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("message role must be 'user' or 'assistant'")
		}
		converted, err := anthropicMessageToChat(role, message["content"])
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("messages did not contain supported content")
	}
	chat["messages"] = messages

	if rawTools, ok := body["tools"].([]any); ok {
		tools := []any{}
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
	var values []string
	for _, raw := range anySlice(v) {
		block, _ := raw.(map[string]any)
		if stringValue(block["type"]) == "text" {
			values = append(values, stringValue(block["text"]))
		}
	}
	return strings.Join(values, "\n")
}

func anthropicMessageToChat(role string, content any) ([]any, error) {
	if text, ok := content.(string); ok {
		return []any{map[string]any{"role": role, "content": text}}, nil
	}
	if role == "assistant" {
		var text strings.Builder
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
		return []any{msg}, nil
	}

	result := []any{}
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
		content = append(content, map[string]any{"type": "thinking", "thinking": out.Reasoning, "signature": ""})
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
		"usage": map[string]any{
			"input_tokens": out.Input, "output_tokens": out.Output,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		},
	}
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
	if aggregate {
		resp, err := h.vc.CompleteChat(ctx, model, payload)
		if err != nil {
			state.fail(toVertexError(err))
			return
		}
		state.consume(outputFromOAI(h.respConv.ToOAI(resp, model)))
		state.finish()
		return
	}
	failed := false
	h.vc.StreamChat(ctx, model, payload, func(chunk vertex.StreamChunk) bool {
		if chunk.Err != nil {
			state.fail(chunk.Err)
			failed = true
			return false
		}
		state.consume(outputFromGeminiChunk(chunk.Data))
		return true
	})
	if !failed {
		state.finish()
	}
}

type anthropicStreamState struct {
	sw       *sseWriter
	id       string
	model    string
	index    int
	openType string
	text     string
	out      protocolOutput
}

func (s *anthropicStreamState) emit(event string, fields map[string]any) {
	fields["type"] = event
	_ = s.sw.write(namedSSE(event, fields))
}

func (s *anthropicStreamState) start() {
	s.emit("message_start", map[string]any{"message": map[string]any{
		"id": s.id, "type": "message", "role": "assistant", "model": s.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
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
	if chunk.Finish != "" {
		s.out.Finish = chunk.Finish
	}
	if chunk.Reasoning != "" {
		s.closeBlock()
		s.emit("content_block_start", map[string]any{
			"index": s.index, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
		})
		s.emit("content_block_delta", map[string]any{
			"index": s.index, "delta": map[string]any{"type": "thinking_delta", "thinking": chunk.Reasoning},
		})
		s.emit("content_block_stop", map[string]any{"index": s.index})
		s.index++
		s.out.Reasoning += chunk.Reasoning
	}
	if chunk.Text != "" {
		if s.openType != "text" {
			s.closeBlock()
			s.openType = "text"
			s.emit("content_block_start", map[string]any{
				"index": s.index, "content_block": map[string]any{"type": "text", "text": ""},
			})
		}
		s.text += chunk.Text
		s.out.Text += chunk.Text
		s.emit("content_block_delta", map[string]any{
			"index": s.index, "delta": map[string]any{"type": "text_delta", "text": chunk.Text},
		})
	}
	for _, tc := range chunk.ToolCalls {
		s.closeBlock()
		s.emit("content_block_start", map[string]any{
			"index": s.index,
			"content_block": map[string]any{
				"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": map[string]any{},
			},
		})
		s.emit("content_block_delta", map[string]any{
			"index": s.index, "delta": map[string]any{
				"type": "input_json_delta", "partial_json": tc.Arguments,
			},
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
	s.emit("content_block_stop", map[string]any{"index": s.index})
	s.index++
	s.openType = ""
}

func (s *anthropicStreamState) finish() {
	s.closeBlock()
	s.emit("message_delta", map[string]any{
		"delta": map[string]any{
			"stop_reason":   anthropicStopReason(s.out.Finish, len(s.out.ToolCalls) > 0),
			"stop_sequence": nil,
		},
		"usage": map[string]any{"output_tokens": s.out.Output},
	})
	s.emit("message_stop", map[string]any{})
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
