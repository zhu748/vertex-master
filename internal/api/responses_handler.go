package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/cli"
	"github.com/bsfdsagfadg/vertex/internal/transform"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

// ResponsesHandler implements the commonly used, stateless subset of the
// OpenAI Responses API on top of the existing Gemini request pipeline.
type ResponsesHandler struct {
	handler
	reqConv  transform.RequestConverter
	respConv transform.ResponseConverter
}

func (h *ResponsesHandler) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	body, err := decodeJSONObject(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		if isRequestBodyTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
		}
		oaiError(w, status, "invalid JSON request body", "invalid_request_error")
		return
	}

	rawModel := strings.TrimSpace(stringValue(body["model"]))
	if rawModel == "" {
		oaiError(w, http.StatusBadRequest, "missing required field 'model'", "invalid_request_error")
		return
	}
	actualModel, useFake := stripFakePrefix(rawModel, h.cfg.FakePrefixes())
	cli.UpdateReqModel(vertex.RequestIDFromContext(r.Context()), actualModel)

	chatBody, err := responsesToChatRequest(body)
	if err != nil {
		oaiError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	chatBody["model"] = actualModel
	model, payload, err := h.reqConv.Convert(chatBody, h.cfg)
	if err != nil {
		oaiError(w, http.StatusBadRequest, "invalid request: "+err.Error(), "invalid_request_error")
		return
	}
	transform.ApplyImageConfig(payload, chatBody)

	if protocolBoolValue(body["stream"]) {
		h.streamResponses(r.Context(), w, rawModel, model, payload, body, useFake || h.cfg.AggregateStream())
		return
	}

	geminiResp, vErr := h.vc.CompleteChat(r.Context(), model, payload)
	if vErr != nil {
		ve := toVertexError(vErr)
		writeJSON(w, ve.Code, vertexErrorToOAI(ve))
		return
	}
	oaiResp := h.respConv.ToOAI(geminiResp, model)
	out := outputFromOAI(oaiResp)
	writeJSON(w, http.StatusOK, buildResponsesResponse(body, rawModel, "resp_"+reqID24(), out))
}

func responsesToChatRequest(body map[string]any) (map[string]any, error) {
	chat := map[string]any{}
	for _, pair := range [][2]string{
		{"temperature", "temperature"}, {"top_p", "top_p"},
		{"max_output_tokens", "max_completion_tokens"}, {"parallel_tool_calls", "parallel_tool_calls"},
	} {
		if v, ok := body[pair[0]]; ok {
			chat[pair[1]] = v
		}
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort := stringValue(reasoning["effort"]); effort != "" {
			chat["reasoning_effort"] = effort
		}
	}
	if text, ok := body["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			typ := stringValue(format["type"])
			switch typ {
			case "json_schema":
				chat["response_format"] = map[string]any{
					"type": "json_schema",
					"json_schema": map[string]any{
						"name":   format["name"],
						"schema": format["schema"],
						"strict": format["strict"],
					},
				}
			case "json_object":
				chat["response_format"] = map[string]any{"type": "json_object"}
			}
		}
	}

	messages := []any{}
	if instructions := responseInstructions(body["instructions"]); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	input, ok := body["input"]
	if !ok || input == nil {
		return nil, fmt.Errorf("missing required field 'input'")
	}
	switch value := input.(type) {
	case string:
		if value == "" {
			return nil, fmt.Errorf("'input' must not be empty")
		}
		messages = append(messages, map[string]any{"role": "user", "content": value})
	case []any:
		if len(value) == 0 {
			return nil, fmt.Errorf("'input' must not be empty")
		}
		for _, raw := range value {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ := stringValue(item["type"])
			switch typ {
			case "function_call":
				id := stringValue(item["call_id"])
				if id == "" {
					id = stringValue(item["id"])
				}
				messages = append(messages, map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id": id, "type": "function",
						"function": map[string]any{
							"name": item["name"], "arguments": jsonString(item["arguments"]),
						},
					}},
				})
			case "function_call_output":
				messages = append(messages, map[string]any{
					"role": "tool", "tool_call_id": item["call_id"], "content": jsonString(item["output"]),
				})
			case "message", "":
				role := stringValue(item["role"])
				if role == "" {
					role = "user"
				}
				var content any
				if role == "assistant" {
					content = responseInstructions(item["content"])
				} else {
					var err error
					content, err = responseContentToChat(item["content"])
					if err != nil {
						return nil, err
					}
				}
				messages = append(messages, map[string]any{"role": role, "content": content})
			}
		}
	default:
		return nil, fmt.Errorf("'input' must be a string or array")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("'input' did not contain any supported items")
	}
	chat["messages"] = messages

	if rawTools, ok := body["tools"].([]any); ok {
		tools := make([]any, 0, len(rawTools))
		for _, raw := range rawTools {
			t, _ := raw.(map[string]any)
			if t == nil {
				continue
			}
			if stringValue(t["type"]) != "function" {
				return nil, fmt.Errorf("unsupported Responses tool type %q; only function tools are available", stringValue(t["type"]))
			}
			if stringValue(t["name"]) == "" {
				return nil, fmt.Errorf("function tool is missing required field 'name'")
			}
			fn := map[string]any{"name": t["name"]}
			for _, key := range []string{"description", "parameters", "strict"} {
				if v, exists := t[key]; exists {
					fn[key] = v
				}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if len(tools) > 0 {
			chat["tools"] = tools
		}
	}
	if choice, ok := body["tool_choice"]; ok {
		if m, isMap := choice.(map[string]any); isMap && stringValue(m["type"]) == "function" {
			chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": m["name"]}}
		} else {
			chat["tool_choice"] = choice
		}
	}
	return chat, nil
}

func responseInstructions(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var parts []string
	for _, raw := range anySlice(v) {
		if item, ok := raw.(map[string]any); ok {
			if text := stringValue(item["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func responseContentToChat(v any) (any, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	var content []any
	for _, raw := range anySlice(v) {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		switch stringValue(item["type"]) {
		case "input_text", "output_text", "text":
			content = append(content, map[string]any{"type": "text", "text": item["text"]})
		case "input_image":
			url := stringValue(item["image_url"])
			if url == "" {
				return nil, fmt.Errorf("input_image currently requires 'image_url'")
			}
			content = append(content, map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": url, "detail": item["detail"]},
			})
		default:
			return nil, fmt.Errorf("unsupported Responses content type %q", stringValue(item["type"]))
		}
	}
	return content, nil
}

func buildResponsesResponse(request map[string]any, model, id string, out protocolOutput) map[string]any {
	status := "completed"
	var incomplete any
	if out.Finish == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	output := responseOutputItems(out)
	textCfg := request["text"]
	if textCfg == nil {
		textCfg = map[string]any{"format": map[string]any{"type": "text"}}
	}
	reasoning := request["reasoning"]
	if reasoning == nil {
		reasoning = map[string]any{"effort": nil, "summary": nil}
	}
	return map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(),
		"completed_at": time.Now().Unix(), "status": status, "error": nil,
		"incomplete_details": incomplete, "instructions": request["instructions"],
		"max_output_tokens": request["max_output_tokens"], "model": model, "output": output,
		"parallel_tool_calls":  defaultBool(request["parallel_tool_calls"], true),
		"previous_response_id": request["previous_response_id"], "reasoning": reasoning,
		"store": defaultBool(request["store"], false), "temperature": request["temperature"],
		"text": textCfg, "tool_choice": defaultAny(request["tool_choice"], "auto"),
		"tools": defaultAny(request["tools"], []any{}), "top_p": request["top_p"],
		"truncation": defaultAny(request["truncation"], "disabled"), "metadata": defaultAny(request["metadata"], map[string]any{}),
		"usage": map[string]any{
			"input_tokens": out.Input, "input_tokens_details": map[string]any{"cached_tokens": 0},
			"output_tokens": out.Output, "output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens": out.Total,
		},
	}
}

func responseOutputItems(out protocolOutput) []any {
	items := []any{}
	if out.Text != "" || len(out.ToolCalls) == 0 {
		items = append(items, map[string]any{
			"id": "msg_" + reqID24(), "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text", "text": out.Text, "annotations": []any{}, "logprobs": []any{},
			}},
		})
	}
	for _, tc := range out.ToolCalls {
		items = append(items, map[string]any{
			"id": "fc_" + reqID24(), "type": "function_call", "status": "completed",
			"call_id": tc.ID, "name": tc.Name, "arguments": tc.Arguments,
		})
	}
	return items
}

func defaultAny(v, fallback any) any {
	if v == nil {
		return fallback
	}
	return v
}

func defaultBool(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

func (h *ResponsesHandler) streamResponses(
	ctx context.Context,
	w http.ResponseWriter,
	displayModel, model string,
	payload, request map[string]any,
	aggregate bool,
) {
	sw := newSSEWriter(w, "text/event-stream")
	id := "resp_" + reqID24()
	state := &responsesStreamState{
		sw: sw, id: id, model: displayModel, request: request,
	}
	state.emit("response.created", map[string]any{
		"response": state.responseObject("in_progress", protocolOutput{}),
	})
	state.emit("response.in_progress", map[string]any{
		"response": state.responseObject("in_progress", protocolOutput{}),
	})

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

type responsesStreamState struct {
	sw          *sseWriter
	id          string
	model       string
	request     map[string]any
	sequence    int
	outputIndex int
	textID      string
	text        string
	textOpen    bool
	items       []any
	out         protocolOutput
}

func (s *responsesStreamState) emit(event string, fields map[string]any) {
	s.sequence++
	fields["type"] = event
	fields["sequence_number"] = s.sequence
	_ = s.sw.write(namedSSE(event, fields))
}

func (s *responsesStreamState) responseObject(status string, out protocolOutput) map[string]any {
	resp := buildResponsesResponse(s.request, s.model, s.id, out)
	resp["status"] = status
	if status == "in_progress" {
		resp["completed_at"] = nil
		resp["output"] = []any{}
		resp["usage"] = nil
	}
	return resp
}

func (s *responsesStreamState) consume(chunk protocolOutput) {
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
		switch strings.ToLower(chunk.Finish) {
		case "stop", "length", "tool_calls", "content_filter":
			s.out.Finish = strings.ToLower(chunk.Finish)
		default:
			s.out.Finish = transform.MapFinishReason(chunk.Finish, len(chunk.ToolCalls) > 0)
		}
	}
	if chunk.Text != "" {
		if !s.textOpen {
			s.textOpen = true
			s.textID = "msg_" + reqID24()
			s.text = ""
			s.emit("response.output_item.added", map[string]any{
				"output_index": s.outputIndex,
				"item": map[string]any{
					"id": s.textID, "type": "message", "status": "in_progress",
					"role": "assistant", "content": []any{},
				},
			})
			s.emit("response.content_part.added", map[string]any{
				"item_id": s.textID, "output_index": s.outputIndex, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}},
			})
		}
		s.text += chunk.Text
		s.out.Text += chunk.Text
		s.emit("response.output_text.delta", map[string]any{
			"item_id": s.textID, "output_index": s.outputIndex, "content_index": 0,
			"delta": chunk.Text, "logprobs": []any{},
		})
	}
	for _, tc := range chunk.ToolCalls {
		s.closeText()
		itemID := "fc_" + reqID24()
		item := map[string]any{
			"id": itemID, "type": "function_call", "status": "in_progress",
			"call_id": tc.ID, "name": tc.Name, "arguments": "",
		}
		s.emit("response.output_item.added", map[string]any{"output_index": s.outputIndex, "item": item})
		s.emit("response.function_call_arguments.delta", map[string]any{
			"item_id": itemID, "output_index": s.outputIndex, "delta": tc.Arguments,
		})
		s.emit("response.function_call_arguments.done", map[string]any{
			"item_id": itemID, "output_index": s.outputIndex, "arguments": tc.Arguments,
		})
		item["status"] = "completed"
		item["arguments"] = tc.Arguments
		s.emit("response.output_item.done", map[string]any{"output_index": s.outputIndex, "item": item})
		s.items = append(s.items, item)
		s.out.ToolCalls = append(s.out.ToolCalls, tc)
		s.outputIndex++
	}
}

func (s *responsesStreamState) closeText() {
	if !s.textOpen {
		return
	}
	part := map[string]any{"type": "output_text", "text": s.text, "annotations": []any{}, "logprobs": []any{}}
	s.emit("response.output_text.done", map[string]any{
		"item_id": s.textID, "output_index": s.outputIndex, "content_index": 0,
		"text": s.text, "logprobs": []any{},
	})
	s.emit("response.content_part.done", map[string]any{
		"item_id": s.textID, "output_index": s.outputIndex, "content_index": 0, "part": part,
	})
	item := map[string]any{
		"id": s.textID, "type": "message", "status": "completed", "role": "assistant",
		"content": []any{part},
	}
	s.emit("response.output_item.done", map[string]any{"output_index": s.outputIndex, "item": item})
	s.items = append(s.items, item)
	s.outputIndex++
	s.textOpen = false
}

func (s *responsesStreamState) finish() {
	s.closeText()
	if len(s.items) == 0 {
		s.out.Text = ""
		s.items = responseOutputItems(s.out)
	}
	if s.out.Total == 0 {
		s.out.Total = s.out.Input + s.out.Output
	}
	resp := buildResponsesResponse(s.request, s.model, s.id, s.out)
	resp["output"] = s.items
	event := "response.completed"
	if resp["status"] == "incomplete" {
		event = "response.incomplete"
	}
	s.emit(event, map[string]any{"response": resp})
}

func (s *responsesStreamState) fail(err *vertex.VertexError) {
	resp := s.responseObject("failed", s.out)
	resp["error"] = map[string]any{"code": err.Kind, "message": vertex.FriendlyErrorMessage(err)}
	s.emit("response.failed", map[string]any{"response": resp})
}
