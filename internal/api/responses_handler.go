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
	out = completeProtocolUsageWithCountTokens(r.Context(), h.vc, model, payload, out)
	restoreResponsesToolNamespaces(&out, namespaceTools)
	writeJSON(w, http.StatusOK, buildResponsesResponse(body, rawModel, "resp_"+reqID24(), out))
}

func responsesToChatRequest(body map[string]any) (map[string]any, error) {
	chat := make(map[string]any, min(len(body), 12))
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

	input, ok := body["input"]
	if !ok || input == nil {
		return nil, fmt.Errorf("missing required field 'input'")
	}
	messageCapacity := 2
	if inputItems, isArray := input.([]any); isArray {
		messageCapacity = len(inputItems) + 1
	}
	messages := make([]any, 0, messageCapacity)
	if instructions := responseInstructions(body["instructions"]); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
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
		var pendingToolCalls []any
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
	rawTools := anySlice(raw)
	if len(rawTools) == 0 {
		return nil, nil, nil
	}
	converted := make([]any, 0, len(rawTools))
	var namespaced map[string]responsesNamespacedTool
	var seenNames map[string]struct{}
	appendFunction := func(tool map[string]any, name string) error {
		if name == "" {
			return fmt.Errorf("function tool is missing required field 'name'")
		}
		if _, exists := seenNames[name]; exists {
			return fmt.Errorf("duplicate Responses function tool name %q", name)
		}
		if seenNames == nil {
			seenNames = make(map[string]struct{}, len(rawTools))
		}
		seenNames[name] = struct{}{}
		fn := map[string]any{"name": name}
		for _, key := range []string{"description", "parameters", "strict"} {
			if value, exists := tool[key]; exists {
				fn[key] = value
			}
		}
		converted = append(converted, map[string]any{"type": "function", "function": fn})
		return nil
	}

	for _, rawTool := range rawTools {
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
			nestedTools := anySlice(tool["tools"])
			for _, rawNested := range nestedTools {
				nested, _ := rawNested.(map[string]any)
				if nested == nil {
					continue
				}
				name := stringValue(nested["name"])
				flatName := flattenResponsesToolName(namespace, name)
				if err := appendFunction(nested, flatName); err != nil {
					return nil, nil, err
				}
				if namespaced == nil {
					namespaced = make(map[string]responsesNamespacedTool, len(nestedTools))
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
	var inline [8]string
	parts := inline[:0]
	for _, raw := range anySlice(v) {
		if item, ok := raw.(map[string]any); ok {
			if value := stringValue(item["text"]); value != "" {
				parts = append(parts, value)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func responseContentToChat(v any) (any, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	rawContent := anySlice(v)
	if responsesTextContentCanPassThrough(rawContent) {
		return rawContent, nil
	}
	content := make([]any, 0, len(rawContent))
	for _, raw := range rawContent {
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

func responsesTextContentCanPassThrough(content []any) bool {
	if len(content) == 0 {
		return false
	}
	for _, raw := range content {
		item, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		switch stringValue(item["type"]) {
		case "input_text", "output_text", "text":
		default:
			return false
		}
	}
	return true
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
	if state.streamFailed() {
		return
	}
	state.emit("response.in_progress", map[string]any{
		"response": state.responseObject("in_progress", protocolOutput{}),
	})
	if state.streamFailed() {
		return
	}

	if aggregate {
		resp, err := h.vc.CompleteChat(ctx, model, payload)
		if err != nil {
			state.fail(toVertexError(err))
			return
		}
		if state.streamFailed() {
			return
		}
		out := outputFromOAI(h.respConv.ToOAI(resp, model))
		out = completeProtocolUsageWithCountTokens(ctx, h.vc, model, payload, out)
		if state.streamFailed() {
			return
		}
		restoreResponsesToolNamespaces(&out, namespaceTools)
		state.consume(out)
		state.finish()
		return
	}

	failed := false
	var lastCandidateTokenCount int
	h.vc.StreamChat(ctx, model, payload, func(chunk vertex.StreamChunk) bool {
		if state.streamFailed() {
			return false
		}
		if chunk.Err != nil {
			state.fail(chunk.Err)
			failed = true
			return false
		}
		data := chunk.Data
		normalizedUsage, hasUsage := normalizeStreamingGeminiUsage(data, &lastCandidateTokenCount)
		state.consume(outputFromGeminiChunkWithUsage(data, normalizedUsage, hasUsage))
		return !state.streamFailed()
	})
	if !failed && !state.streamFailed() {
		state.out = completeProtocolUsageWithCountTokens(ctx, h.vc, model, payload, state.output())
		if !state.streamFailed() {
			state.finish()
		}
	}
}

type responsesStreamState struct {
	sw                     *sseWriter
	id                     string
	model                  string
	request                map[string]any
	namespaceTools         map[string]responsesNamespacedTool
	sequence               int
	outputIndex            int
	textID                 string
	text                   transform.StringAccumulator
	textBlocks             []string
	textCache              string
	textCacheValid         bool
	textOpen               bool
	textDeltaEvent         responsesOutputTextDeltaEvent
	textItemEvent          responsesOutputTextItemEvent
	textContentPartEvent   responsesOutputTextContentPartEvent
	textDoneEvent          responsesOutputTextDoneEvent
	textItemContent        [1]responsesOutputTextPart
	functionItemEvent      responsesFunctionCallItemEvent
	functionArgumentsEvent responsesFunctionCallArgumentsEvent
	functionNamespace      string
	functionArguments      string
	items                  []any
	out                    protocolOutput
}

type responsesOutputTextDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Delta          string `json:"delta"`
	Logprobs       []any  `json:"logprobs"`
}

type responsesOutputTextPart struct {
	Annotations []any  `json:"annotations"`
	Logprobs    []any  `json:"logprobs"`
	Text        string `json:"text"`
	Type        string `json:"type"`
}

type responsesOutputTextMessageItem struct {
	Content []responsesOutputTextPart `json:"content"`
	ID      string                    `json:"id"`
	Role    string                    `json:"role"`
	Status  string                    `json:"status"`
	Type    string                    `json:"type"`
}

type responsesOutputTextItemEvent struct {
	Item           responsesOutputTextMessageItem `json:"item"`
	OutputIndex    int                            `json:"output_index"`
	SequenceNumber int                            `json:"sequence_number"`
	Type           string                         `json:"type"`
}

type responsesOutputTextContentPartEvent struct {
	ContentIndex   int                     `json:"content_index"`
	ItemID         string                  `json:"item_id"`
	OutputIndex    int                     `json:"output_index"`
	Part           responsesOutputTextPart `json:"part"`
	SequenceNumber int                     `json:"sequence_number"`
	Type           string                  `json:"type"`
}

type responsesOutputTextDoneEvent struct {
	Annotations    []any  `json:"annotations"`
	ContentIndex   int    `json:"content_index"`
	ItemID         string `json:"item_id"`
	Logprobs       []any  `json:"logprobs"`
	OutputIndex    int    `json:"output_index"`
	SequenceNumber int    `json:"sequence_number"`
	Text           string `json:"text"`
	Type           string `json:"type"`
}

type responsesFunctionCallItemEvent struct {
	Item           responsesFunctionCallItem `json:"item"`
	OutputIndex    int                       `json:"output_index"`
	SequenceNumber int                       `json:"sequence_number"`
	Type           string                    `json:"type"`
}

type responsesFunctionCallItem struct {
	Arguments string  `json:"arguments"`
	CallID    string  `json:"call_id"`
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Namespace *string `json:"namespace,omitempty"`
	Status    string  `json:"status"`
	Type      string  `json:"type"`
}

type responsesFunctionCallArgumentsEvent struct {
	Arguments      *string `json:"arguments,omitempty"`
	Delta          *string `json:"delta,omitempty"`
	ItemID         string  `json:"item_id"`
	OutputIndex    int     `json:"output_index"`
	SequenceNumber int     `json:"sequence_number"`
	Type           string  `json:"type"`
}

func (s *responsesStreamState) emit(event string, fields map[string]any) {
	if s.streamFailed() {
		return
	}
	s.sequence++
	fields["type"] = event
	fields["sequence_number"] = s.sequence
	_ = s.sw.writeNamed(event, fields)
}

func (s *responsesStreamState) streamFailed() bool {
	return s == nil || (s.sw != nil && s.sw.failed.Load())
}

func (s *responsesStreamState) emitTextDelta(delta string) {
	if s.streamFailed() {
		return
	}
	s.sequence++
	s.textDeltaEvent = responsesOutputTextDeltaEvent{
		Type:           "response.output_text.delta",
		SequenceNumber: s.sequence,
		ItemID:         s.textID,
		OutputIndex:    s.outputIndex,
		ContentIndex:   0,
		Delta:          delta,
		Logprobs:       []any{},
	}
	_ = s.sw.writeNamed("response.output_text.delta", &s.textDeltaEvent)
}

func (s *responsesStreamState) emitTextBlockStart() {
	if s.streamFailed() {
		return
	}
	s.sequence++
	s.textItemEvent = responsesOutputTextItemEvent{
		Item: responsesOutputTextMessageItem{
			Content: s.textItemContent[:0],
			ID:      s.textID,
			Role:    "assistant",
			Status:  "in_progress",
			Type:    "message",
		},
		OutputIndex:    s.outputIndex,
		SequenceNumber: s.sequence,
		Type:           "response.output_item.added",
	}
	if !s.sw.writeNamed("response.output_item.added", &s.textItemEvent) {
		return
	}

	s.sequence++
	s.textContentPartEvent = responsesOutputTextContentPartEvent{
		ContentIndex: 0,
		ItemID:       s.textID,
		OutputIndex:  s.outputIndex,
		Part: responsesOutputTextPart{
			Annotations: []any{}, Logprobs: []any{}, Text: "", Type: "output_text",
		},
		SequenceNumber: s.sequence,
		Type:           "response.content_part.added",
	}
	_ = s.sw.writeNamed("response.content_part.added", &s.textContentPartEvent)
}

func (s *responsesStreamState) emitTextBlockDone(text string) {
	if s.streamFailed() {
		return
	}
	// Codex CLI SDK 严格解析 output_text.done 事件，期望 logprobs 和
	// annotations 字段都存在，即使它们是空数组。
	s.sequence++
	s.textDoneEvent = responsesOutputTextDoneEvent{
		Annotations:    []any{},
		ContentIndex:   0,
		ItemID:         s.textID,
		Logprobs:       []any{},
		OutputIndex:    s.outputIndex,
		SequenceNumber: s.sequence,
		Text:           text,
		Type:           "response.output_text.done",
	}
	if !s.sw.writeNamed("response.output_text.done", &s.textDoneEvent) {
		return
	}

	part := responsesOutputTextPart{
		Annotations: []any{}, Logprobs: []any{}, Text: text, Type: "output_text",
	}
	s.sequence++
	s.textContentPartEvent = responsesOutputTextContentPartEvent{
		ContentIndex:   0,
		ItemID:         s.textID,
		OutputIndex:    s.outputIndex,
		Part:           part,
		SequenceNumber: s.sequence,
		Type:           "response.content_part.done",
	}
	if !s.sw.writeNamed("response.content_part.done", &s.textContentPartEvent) {
		return
	}

	s.textItemContent[0] = part
	s.sequence++
	s.textItemEvent = responsesOutputTextItemEvent{
		Item: responsesOutputTextMessageItem{
			Content: s.textItemContent[:],
			ID:      s.textID,
			Role:    "assistant",
			Status:  "completed",
			Type:    "message",
		},
		OutputIndex:    s.outputIndex,
		SequenceNumber: s.sequence,
		Type:           "response.output_item.done",
	}
	_ = s.sw.writeNamed("response.output_item.done", &s.textItemEvent)
}

func (s *responsesStreamState) emitFunctionCallItem(
	event, status, itemID string,
	tc protocolToolCall,
	arguments string,
) {
	if s.streamFailed() {
		return
	}
	s.sequence++
	s.functionNamespace = tc.Namespace
	var namespace *string
	if tc.Namespace != "" {
		namespace = &s.functionNamespace
	}
	s.functionItemEvent = responsesFunctionCallItemEvent{
		Item: responsesFunctionCallItem{
			Arguments: arguments,
			CallID:    tc.ID,
			ID:        itemID,
			Name:      tc.Name,
			Namespace: namespace,
			Status:    status,
			Type:      "function_call",
		},
		OutputIndex:    s.outputIndex,
		SequenceNumber: s.sequence,
		Type:           event,
	}
	_ = s.sw.writeNamed(event, &s.functionItemEvent)
}

func (s *responsesStreamState) emitFunctionCallArguments(event, itemID, arguments string) {
	if s.streamFailed() {
		return
	}
	s.sequence++
	s.functionArguments = arguments
	s.functionArgumentsEvent = responsesFunctionCallArgumentsEvent{
		ItemID:         itemID,
		OutputIndex:    s.outputIndex,
		SequenceNumber: s.sequence,
		Type:           event,
	}
	if event == "response.function_call_arguments.delta" {
		s.functionArgumentsEvent.Delta = &s.functionArguments
	} else {
		s.functionArgumentsEvent.Arguments = &s.functionArguments
	}
	_ = s.sw.writeNamed(event, &s.functionArgumentsEvent)
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
	if s.streamFailed() {
		return
	}
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
			s.text.Reset()
			s.emitTextBlockStart()
			if s.streamFailed() {
				return
			}
		}
		s.text.WriteString(chunk.Text)
		s.textCacheValid = false
		s.emitTextDelta(chunk.Text)
		if s.streamFailed() {
			return
		}
	}
	for _, tc := range chunk.ToolCalls {
		if s.streamFailed() {
			return
		}
		s.closeText()
		itemID := "fc_" + reqID24()
		s.emitFunctionCallItem("response.output_item.added", "in_progress", itemID, tc, "")
		s.emitFunctionCallArguments("response.function_call_arguments.delta", itemID, tc.Arguments)
		s.emitFunctionCallArguments("response.function_call_arguments.done", itemID, tc.Arguments)
		s.emitFunctionCallItem("response.output_item.done", "completed", itemID, tc, tc.Arguments)
		if s.streamFailed() {
			return
		}
		item := map[string]any{
			"id": itemID, "type": "function_call", "status": "completed",
			"call_id": tc.ID, "name": tc.Name, "arguments": tc.Arguments,
		}
		if tc.Namespace != "" {
			item["namespace"] = tc.Namespace
		}
		s.items = append(s.items, item)
		s.out.ToolCalls = append(s.out.ToolCalls, tc)
		s.outputIndex++
	}
}

func (s *responsesStreamState) closeText() {
	if !s.textOpen || s.streamFailed() {
		return
	}
	text := s.text.String()
	s.textBlocks = append(s.textBlocks, text)
	s.emitTextBlockDone(text)
	if s.streamFailed() {
		return
	}
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
	item := map[string]any{
		"id": s.textID, "type": "message", "status": "completed", "role": "assistant",
		"content": []any{part},
	}
	s.items = append(s.items, item)
	s.outputIndex++
	s.textOpen = false
	s.text.Reset()
}

func (s *responsesStreamState) finish() {
	if s.streamFailed() {
		return
	}
	s.closeText()
	if s.streamFailed() {
		return
	}
	s.out = s.output()
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
	if s.streamFailed() {
		return
	}
	resp := s.responseObject("failed", s.output())
	resp["error"] = map[string]any{"code": err.Kind, "message": vertex.FriendlyErrorMessage(err)}
	s.emit("response.failed", map[string]any{"response": resp})
}

func (s *responsesStreamState) output() protocolOutput {
	out := s.out
	if !s.textCacheValid {
		current := ""
		if s.textOpen {
			current = s.text.String()
		}
		s.textCache = joinProtocolTextBlocks(s.textBlocks, current)
		s.textCacheValid = true
	}
	out.Text = s.textCache
	return out
}
