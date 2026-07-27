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

const responsesNamespaceToolsKey = "__responses_namespace_tools"

type responsesNamespacedTool struct {
	Namespace string
	Name      string
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
	namespaceTools, _ := chatBody[responsesNamespaceToolsKey].(map[string]responsesNamespacedTool)

	if protocolBoolValue(body["stream"]) {
		h.streamResponses(
			r.Context(), w, rawModel, model, payload, body, namespaceTools,
			useFake || h.cfg.AggregateStream(),
		)
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
	out = completeProtocolUsage(r.Context(), h.vc, model, payload, out)
	restoreResponsesToolNamespaces(&out, namespaceTools)
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

	convertedTools, namespaceTools, err := convertResponsesTools(body["tools"])
	if err != nil {
		return nil, err
	}
	if len(convertedTools) > 0 {
		chat["tools"] = convertedTools
	}
	if len(namespaceTools) > 0 {
		chat[responsesNamespaceToolsKey] = namespaceTools
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
		pendingToolCalls := []any{}
		flushToolCalls := func() {
			if len(pendingToolCalls) == 0 {
				return
			}
			messages = append(messages, map[string]any{
				"role": "assistant", "content": "", "tool_calls": pendingToolCalls,
			})
			pendingToolCalls = nil
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
				name := stringValue(item["name"])
				if namespace := stringValue(item["namespace"]); namespace != "" {
					name = flattenResponsesToolName(namespace, name)
				}
				pendingToolCalls = append(pendingToolCalls, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{
						"name": name, "arguments": jsonString(item["arguments"]),
					},
				})
			case "function_call_output":
				flushToolCalls()
				messages = append(messages, map[string]any{
					"role": "tool", "tool_call_id": item["call_id"], "content": jsonString(item["output"]),
				})
			case "message", "":
				flushToolCalls()
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
			case "reasoning":
				// Codex 会把上一轮 reasoning item 放回 input。Gemini 不接受该
				// Responses 专用项，但它不应打断相邻的并行 function_call 分组。
			}
		}
		flushToolCalls()
	default:
		return nil, fmt.Errorf("'input' must be a string or array")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("'input' did not contain any supported items")
	}
	chat["messages"] = messages

	if choice, ok := body["tool_choice"]; ok {
		if m, isMap := choice.(map[string]any); isMap && stringValue(m["type"]) == "function" {
			chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": m["name"]}}
		} else {
			chat["tool_choice"] = choice
		}
	}
	return chat, nil
}

func convertResponsesTools(raw any) ([]any, map[string]responsesNamespacedTool, error) {
	converted := []any{}
	namespaced := map[string]responsesNamespacedTool{}
	seenNames := map[string]bool{}
	appendFunction := func(tool map[string]any, name string) error {
		if name == "" {
			return fmt.Errorf("function tool is missing required field 'name'")
		}
		if seenNames[name] {
			return fmt.Errorf("duplicate Responses function tool name %q", name)
		}
		seenNames[name] = true
		fn := map[string]any{"name": name}
		for _, key := range []string{"description", "parameters", "strict"} {
			if value, exists := tool[key]; exists {
				fn[key] = value
			}
		}
		converted = append(converted, map[string]any{"type": "function", "function": fn})
		return nil
	}

	for _, rawTool := range anySlice(raw) {
		tool, _ := rawTool.(map[string]any)
		if tool == nil {
			continue
		}
		switch typ := stringValue(tool["type"]); typ {
		case "function":
			if err := appendFunction(tool, stringValue(tool["name"])); err != nil {
				return nil, nil, err
			}
		case "namespace":
			namespace := stringValue(tool["name"])
			if namespace == "" {
				return nil, nil, fmt.Errorf("namespace tool is missing required field 'name'")
			}
			// Codex 可能只发送 namespace 声明而不附带子工具（例如内置
			// collaboration）。这种声明无法由 Gemini 调用，安全忽略即可。
			for _, rawNested := range anySlice(tool["tools"]) {
				nested, _ := rawNested.(map[string]any)
				if nested == nil {
					continue
				}
				name := stringValue(nested["name"])
				flatName := flattenResponsesToolName(namespace, name)
				if err := appendFunction(nested, flatName); err != nil {
					return nil, nil, err
				}
				namespaced[flatName] = responsesNamespacedTool{Namespace: namespace, Name: name}
			}
		case "web_search", "web_search_preview", "image_generation", "file_search", "computer", "code_interpreter":
			// 这些工具由 OpenAI 服务端执行，当前 Gemini 匿名端点无法代为
			// 执行。Codex 会在自定义 provider 请求中自动附带其中一部分；
			// 忽略声明可保留本地 function tools，避免整个编码会话 400。
			continue
		default:
			return nil, nil, fmt.Errorf(
				"unsupported Responses tool type %q; function and namespace tools are available", typ,
			)
		}
	}
	return converted, namespaced, nil
}

func flattenResponsesToolName(namespace, name string) string {
	return namespace + "__" + name
}

func restoreResponsesToolNamespaces(out *protocolOutput, mappings map[string]responsesNamespacedTool) {
	if out == nil || len(mappings) == 0 {
		return
	}
	for i := range out.ToolCalls {
		if mapped, ok := mappings[out.ToolCalls[i].Name]; ok {
			out.ToolCalls[i].Name = mapped.Name
			out.ToolCalls[i].Namespace = mapped.Namespace
		}
	}
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
			"input_tokens": out.Input, "input_tokens_details": map[string]any{"cached_tokens": out.CachedInputTokens},
			"output_tokens": out.Output, "output_tokens_details": map[string]any{"reasoning_tokens": out.ReasoningTokens},
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
		item := map[string]any{
			"id": "fc_" + reqID24(), "type": "function_call", "status": "completed",
			"call_id": tc.ID, "name": tc.Name, "arguments": tc.Arguments,
		}
		if tc.Namespace != "" {
			item["namespace"] = tc.Namespace
		}
		items = append(items, item)
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
	namespaceTools map[string]responsesNamespacedTool,
	aggregate bool,
) {
	sw := newSSEWriter(w, "text/event-stream")
	id := "resp_" + reqID24()
	state := &responsesStreamState{
		sw: sw, id: id, model: displayModel, request: request, namespaceTools: namespaceTools,
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
		out := outputFromOAI(h.respConv.ToOAI(resp, model))
		out = completeProtocolUsage(ctx, h.vc, model, payload, out)
		restoreResponsesToolNamespaces(&out, namespaceTools)
		state.consume(out)
		state.finish()
		return
	}

	failed := false
	var lastCandidate map[string]any
	h.vc.StreamChat(ctx, model, payload, func(chunk vertex.StreamChunk) bool {
		if chunk.Err != nil {
			state.fail(chunk.Err)
			failed = true
			return false
		}
		data := cloneStringMap(chunk.Data)
		normalizeStreamingGeminiUsage(data, &lastCandidate)
		state.consume(outputFromGeminiChunk(data))
		return true
	})
	if !failed {
		state.out = completeProtocolUsage(ctx, h.vc, model, payload, state.out)
		state.finish()
	}
}

type responsesStreamState struct {
	sw             *sseWriter
	id             string
	model          string
	request        map[string]any
	namespaceTools map[string]responsesNamespacedTool
	sequence       int
	outputIndex    int
	textID         string
	text           string
	textOpen       bool
	items          []any
	out            protocolOutput
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
	restoreResponsesToolNamespaces(&chunk, s.namespaceTools)
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
		if tc.Namespace != "" {
			item["namespace"] = tc.Namespace
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
	// Codex CLI SDK 严格解析 output_text.done 事件，期望 logprobs 和 annotations 字段都存在。
	s.emit("response.output_text.done", map[string]any{
		"item_id": s.textID, "output_index": s.outputIndex, "content_index": 0,
		"text": s.text, "annotations": []any{}, "logprobs": []any{},
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
