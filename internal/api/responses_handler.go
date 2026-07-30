package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/cli"
	"github.com/bsfdsagfadg/vertex/internal/jsonx"
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
	actualModel, useFake := resolveRequestedModel(rawModel, h.cfg)
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
	out.Text = transform.StripAssistantPrefillEcho(
		out.Text,
		transform.AssistantPrefillFromPayload(payload),
	)
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
	instructions, err := responseInstructions(body["instructions"])
	if err != nil {
		return nil, fmt.Errorf("instructions: %w", err)
	}
	if instructions != "" {
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
		var allToolCalls []any
		pendingToolCallStart := 0
		flushToolCalls := func() {
			if pendingToolCallStart == len(allToolCalls) {
				return
			}
			pendingToolCalls := allToolCalls[pendingToolCallStart:len(allToolCalls):len(allToolCalls)]
			messages = append(messages, map[string]any{
				"role": "assistant", "content": "", "tool_calls": pendingToolCalls,
			})
			pendingToolCallStart = len(allToolCalls)
		}
		for itemIndex, raw := range value {
			item, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("input[%d] must be an object", itemIndex)
			}
			typ := stringValue(item["type"])
			switch typ {
			case "function_call":
				id := stringValue(item["call_id"])
				if id == "" {
					id = stringValue(item["id"])
				}
				name := stringValue(item["name"])
				if id == "" {
					return nil, fmt.Errorf("input[%d] function_call is missing call_id", itemIndex)
				}
				if name == "" {
					return nil, fmt.Errorf("input[%d] function_call is missing name", itemIndex)
				}
				if namespace := stringValue(item["namespace"]); namespace != "" {
					name = flattenResponsesToolName(namespace, name)
				}
				if allToolCalls == nil {
					allToolCalls = make([]any, 0, min(len(value), 8))
				}
				allToolCalls = append(allToolCalls, transform.CanonicalOAIToolCall{
					ID:   id,
					Type: "function",
					Function: transform.CanonicalOAIFunctionCallData{
						Name: name,
						Arguments: normalizedIntermediateJSONValue(
							item["arguments"],
						),
					},
				})
			case "function_call_output":
				if stringValue(item["call_id"]) == "" {
					return nil, fmt.Errorf(
						"input[%d] function_call_output is missing call_id",
						itemIndex,
					)
				}
				if _, exists := item["output"]; !exists {
					return nil, fmt.Errorf(
						"input[%d] function_call_output is missing output",
						itemIndex,
					)
				}
				flushToolCalls()
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": item["call_id"],
					"content":      normalizedIntermediateJSONValue(item["output"]),
				})
			case "message", "":
				flushToolCalls()
				explicitRole := stringValue(item["role"])
				role := explicitRole
				if role == "" {
					role = "user"
				}
				if _, exists := item["content"]; !exists {
					return nil, fmt.Errorf("input[%d] message is missing content", itemIndex)
				}
				if explicitRole == role && reusableResponsesMessage(item) {
					// The decoded request is read-only for the remainder of the
					// handler. Reuse canonical messages without first boxing an
					// unchanged []any content value.
					messages = append(messages, item)
					continue
				}
				content, err := responseContentToChat(item["content"])
				if err != nil {
					return nil, fmt.Errorf("input[%d]: %w", itemIndex, err)
				}
				messages = append(messages, map[string]any{"role": role, "content": content})
			case "reasoning":
				// Codex 会把上一轮 reasoning item 放回 input。Gemini 不接受该
				// Responses 专用项，但它不应打断相邻的并行 function_call 分组。
			default:
				return nil, fmt.Errorf(
					"input[%d] has unsupported item type %q",
					itemIndex,
					typ,
				)
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
			name := stringValue(m["name"])
			if namespace := stringValue(m["namespace"]); namespace != "" {
				name = flattenResponsesToolName(namespace, name)
			}
			chat["tool_choice"] = map[string]any{
				"type": "function", "function": map[string]any{"name": name},
			}
		} else {
			chat["tool_choice"] = choice
		}
	}
	return chat, nil
}

func convertResponsesTools(raw any) ([]any, map[string]responsesNamespacedTool, error) {
	if raw == nil {
		return nil, nil, nil
	}
	rawTools, ok := raw.([]any)
	if !ok {
		return nil, nil, fmt.Errorf("tools must be an array")
	}
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

	for toolIndex, rawTool := range rawTools {
		tool, _ := rawTool.(map[string]any)
		if tool == nil {
			return nil, nil, fmt.Errorf("tools[%d] must be an object", toolIndex)
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
			var nestedTools []any
			if rawNestedTools, exists := tool["tools"]; exists && rawNestedTools != nil {
				var ok bool
				nestedTools, ok = rawNestedTools.([]any)
				if !ok {
					return nil, nil, fmt.Errorf("tools[%d].tools must be an array", toolIndex)
				}
			}
			for nestedIndex, rawNested := range nestedTools {
				nested, _ := rawNested.(map[string]any)
				if nested == nil {
					return nil, nil, fmt.Errorf(
						"tools[%d].tools[%d] must be an object",
						toolIndex,
						nestedIndex,
					)
				}
				if typ := stringValue(nested["type"]); typ != "" && typ != "function" {
					return nil, nil, fmt.Errorf(
						"tools[%d].tools[%d] has unsupported type %q",
						toolIndex,
						nestedIndex,
						typ,
					)
				}
				name := stringValue(nested["name"])
				if name == "" {
					return nil, nil, fmt.Errorf(
						"tools[%d].tools[%d] function is missing required field 'name'",
						toolIndex,
						nestedIndex,
					)
				}
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

func responseInstructions(v any) (string, error) {
	switch value := v.(type) {
	case nil:
		return "", nil
	case string:
		return value, nil
	case []any:
		v = value
	default:
		return "", fmt.Errorf("must be a string or text block array")
	}

	var inline [8]string
	parts := inline[:0]
	for index, raw := range v.([]any) {
		item, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("content[%d] must be an object", index)
		}
		switch typ := stringValue(item["type"]); typ {
		case "", "text", "input_text", "output_text":
		default:
			return "", fmt.Errorf("content[%d] has unsupported type %q", index, typ)
		}
		text, ok := item["text"].(string)
		if !ok {
			return "", fmt.Errorf("content[%d].text must be a string", index)
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func responseContentToChat(v any) (any, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	rawContent, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("message content must be a string or array")
	}
	if responsesTextContentCanPassThrough(rawContent) {
		return rawContent, nil
	}
	content := make([]any, 0, len(rawContent))
	for contentIndex, raw := range rawContent {
		item, _ := raw.(map[string]any)
		if item == nil {
			return nil, fmt.Errorf("content[%d] must be an object", contentIndex)
		}
		switch stringValue(item["type"]) {
		case "input_text", "output_text", "text":
			text, ok := item["text"].(string)
			if !ok {
				return nil, fmt.Errorf("content[%d].text must be a string", contentIndex)
			}
			content = append(content, map[string]any{"type": "text", "text": text})
		case "input_image":
			url := stringValue(item["image_url"])
			if url == "" {
				return nil, fmt.Errorf("input_image currently requires 'image_url'")
			}
			content = append(content, map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": url, "detail": item["detail"]},
			})
		default:
			return nil, fmt.Errorf(
				"content[%d] has unsupported Responses type %q",
				contentIndex,
				stringValue(item["type"]),
			)
		}
	}
	return content, nil
}

func reusableResponsesMessage(item map[string]any) bool {
	if len(item) < 2 || len(item) > 3 {
		return false
	}
	if stringValue(item["role"]) == "" {
		return false
	}
	if typ := stringValue(item["type"]); typ != "" && typ != "message" {
		return false
	}
	for key := range item {
		if key != "type" && key != "role" && key != "content" {
			return false
		}
	}
	switch original := item["content"].(type) {
	case string:
		return true
	case []any:
		return responsesTextContentCanPassThrough(original)
	default:
		return false
	}
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
			if _, ok := item["text"].(string); !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

type responsesResponse struct {
	CompletedAt        *int64                     `json:"completed_at"`
	CreatedAt          int64                      `json:"created_at"`
	Error              *responsesResponseError    `json:"error"`
	ID                 string                     `json:"id"`
	IncompleteDetails  *responsesIncompleteDetail `json:"incomplete_details"`
	Instructions       any                        `json:"instructions"`
	MaxOutputTokens    any                        `json:"max_output_tokens"`
	Metadata           any                        `json:"metadata"`
	Model              string                     `json:"model"`
	Object             string                     `json:"object"`
	Output             []any                      `json:"output"`
	ParallelToolCalls  bool                       `json:"parallel_tool_calls"`
	PreviousResponseID any                        `json:"previous_response_id"`
	Reasoning          any                        `json:"reasoning"`
	Status             string                     `json:"status"`
	Store              bool                       `json:"store"`
	Temperature        any                        `json:"temperature"`
	Text               any                        `json:"text"`
	ToolChoice         any                        `json:"tool_choice"`
	Tools              any                        `json:"tools"`
	TopP               any                        `json:"top_p"`
	Truncation         any                        `json:"truncation"`
	Usage              *responsesResponseUsage    `json:"usage"`

	completedAt       int64                     `json:"-"`
	incompleteDetails responsesIncompleteDetail `json:"-"`
	usage             responsesResponseUsage    `json:"-"`
}

type responsesResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesIncompleteDetail struct {
	Reason string `json:"reason"`
}

type responsesResponseUsage struct {
	InputTokens         int                         `json:"input_tokens"`
	InputTokensDetails  responsesInputTokenDetails  `json:"input_tokens_details"`
	OutputTokens        int                         `json:"output_tokens"`
	OutputTokensDetails responsesOutputTokenDetails `json:"output_tokens_details"`
	TotalTokens         int                         `json:"total_tokens"`
}

type responsesInputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type responsesOutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type responsesDefaultTextConfig struct {
	Format responsesDefaultTextFormat `json:"format"`
}

type responsesDefaultTextFormat struct {
	Type string `json:"type"`
}

type responsesDefaultReasoningConfig struct {
	Effort  any `json:"effort"`
	Summary any `json:"summary"`
}

type responsesEmptyObject struct{}

var (
	defaultResponsesText = responsesDefaultTextConfig{ //nolint:gochecknoglobals
		Format: responsesDefaultTextFormat{Type: "text"},
	}
	defaultResponsesReasoning = responsesDefaultReasoningConfig{} //nolint:gochecknoglobals
	defaultResponsesMetadata  = responsesEmptyObject{}            //nolint:gochecknoglobals
	defaultResponsesTools     = []any{}                           //nolint:gochecknoglobals
)

func buildResponsesResponse(request map[string]any, model, id string, out protocolOutput) *responsesResponse {
	response := &responsesResponse{}
	fillResponsesResponse(response, request, model, id, out, nil)
	return response
}

func fillResponsesResponse(
	response *responsesResponse,
	request map[string]any,
	model, id string,
	out protocolOutput,
	output []any,
) {
	textCfg := request["text"]
	if textCfg == nil {
		textCfg = &defaultResponsesText
	}
	reasoning := request["reasoning"]
	if reasoning == nil {
		reasoning = &defaultResponsesReasoning
	}

	*response = responsesResponse{
		CreatedAt:          time.Now().Unix(),
		ID:                 id,
		Instructions:       request["instructions"],
		MaxOutputTokens:    request["max_output_tokens"],
		Metadata:           defaultAny(request["metadata"], &defaultResponsesMetadata),
		Model:              model,
		Object:             "response",
		ParallelToolCalls:  defaultBool(request["parallel_tool_calls"], true),
		PreviousResponseID: request["previous_response_id"],
		Reasoning:          reasoning,
		Store:              defaultBool(request["store"], false),
		Temperature:        request["temperature"],
		Text:               textCfg,
		ToolChoice:         defaultAny(request["tool_choice"], "auto"),
		Tools:              defaultAny(request["tools"], defaultResponsesTools),
		TopP:               request["top_p"],
		Truncation:         defaultAny(request["truncation"], "disabled"),
	}
	fillResponsesResult(response, out, output)
}

func fillResponsesResult(response *responsesResponse, out protocolOutput, output []any) {
	status := "completed"
	incompleteReason := responsesIncompleteReason(out.Finish)
	if incompleteReason != "" {
		status = "incomplete"
	}
	if output == nil {
		output = responseOutputItems(out)
	} else {
		setResponsesOutputItemStatus(output, responsesOutputItemStatus(out.Finish))
	}
	response.completedAt = time.Now().Unix()
	response.CompletedAt = &response.completedAt
	response.Error = nil
	response.IncompleteDetails = nil
	response.Output = output
	response.Status = status
	response.usage = responsesResponseUsage{
		InputTokens: out.Input,
		InputTokensDetails: responsesInputTokenDetails{
			CachedTokens: out.CachedInputTokens,
		},
		OutputTokens: out.Output,
		OutputTokensDetails: responsesOutputTokenDetails{
			ReasoningTokens: out.ReasoningTokens,
		},
		TotalTokens: out.Total,
	}
	response.Usage = &response.usage
	if status == "incomplete" {
		response.incompleteDetails.Reason = incompleteReason
		response.IncompleteDetails = &response.incompleteDetails
	}
}

func responsesIncompleteReason(finish string) string {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "length", "max_tokens", "max_output_tokens":
		return "max_output_tokens"
	case "content_filter", "safety", "recitation", "prohibited_content", "blocklist", "spii":
		return "content_filter"
	default:
		return ""
	}
}

func responsesOutputItemStatus(finish string) string {
	if responsesIncompleteReason(finish) != "" {
		return "incomplete"
	}
	return "completed"
}

func setResponsesOutputItemStatus(output []any, status string) {
	for _, rawItem := range output {
		switch item := rawItem.(type) {
		case *responsesCompletedTextMessageItem:
			item.Status = status
		case *responsesOutputTextMessageItem:
			item.Status = status
		case *responsesFunctionCallItem:
			item.Status = status
		case map[string]any:
			item["status"] = status
		}
	}
}

func cacheResponsesStreamStaticFields(response *responsesResponse) {
	response.Instructions = cacheResponsesStaticJSON(response.Instructions)
	response.Metadata = cacheResponsesStaticJSON(response.Metadata)
	response.Reasoning = cacheResponsesStaticJSON(response.Reasoning)
	response.Text = cacheResponsesStaticJSON(response.Text)
	response.ToolChoice = cacheResponsesStaticJSON(response.ToolChoice)
	response.Tools = cacheResponsesStaticJSON(response.Tools)
}

func cacheResponsesStaticJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 0 {
			return value
		}
	case []any:
		if len(typed) == 0 {
			return value
		}
	default:
		return value
	}
	encoded, err := jsonx.Marshal(value)
	if err == nil {
		return json.RawMessage(encoded)
	}
	return value
}

func responseOutputItems(out protocolOutput) []any {
	textOutput := out.Text != "" || len(out.ToolCalls) == 0
	status := responsesOutputItemStatus(out.Finish)
	capacity := len(out.ToolCalls)
	if textOutput {
		capacity++
	}
	items := make([]any, 0, capacity)
	if textOutput {
		items = append(items, &responsesCompletedTextMessageItem{
			Content: [1]responsesOutputTextPart{{
				Annotations: []any{}, Logprobs: []any{}, Text: out.Text, Type: "output_text",
			}},
			ID: "msg_" + reqID24(), Role: "assistant", Status: status, Type: "message",
		})
	}
	for _, tc := range out.ToolCalls {
		items = append(items, &responsesFunctionCallItem{
			Arguments: tc.Arguments,
			CallID:    tc.ID,
			ID:        "fc_" + reqID24(),
			Name:      tc.Name,
			Namespace: tc.Namespace,
			Status:    status,
			Type:      "function_call",
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
	namespaceTools map[string]responsesNamespacedTool,
	aggregate bool,
) {
	sw := newSSEWriter(w, "text/event-stream")
	id := "resp_" + reqID24()
	state := &responsesStreamState{
		sw: sw, id: id, model: displayModel, request: request, namespaceTools: namespaceTools,
	}
	state.start()
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
		out.Text = transform.StripAssistantPrefillEcho(
			out.Text,
			transform.AssistantPrefillFromPayload(payload),
		)
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
	prefillFilter := transform.NewAssistantPrefillStreamFilter(
		transform.AssistantPrefillFromPayload(payload),
	)
	h.vc.StreamChat(ctx, model, payload, func(chunk vertex.StreamChunk) bool {
		if state.streamFailed() {
			return false
		}
		if chunk.Err != nil {
			state.fail(chunk.Err)
			failed = true
			return false
		}
		if chunk.HasCanonicalText {
			state.consume(outputFromCanonicalTextStreamData(chunk.CanonicalText, prefillFilter))
			return !state.streamFailed()
		}
		data := chunk.GeminiData()
		prefillFilter.FilterGeminiChunk(data)
		normalizedUsage, hasUsage := normalizeStreamingGeminiUsage(data, &lastCandidateTokenCount)
		state.consume(outputFromGeminiChunkWithUsage(data, normalizedUsage, hasUsage))
		return !state.streamFailed()
	})
	if !failed && !state.streamFailed() {
		if tail := prefillFilter.Finalize(); tail != "" {
			state.consume(protocolOutput{Text: tail})
		}
		state.out = completeProtocolUsageWithCountTokens(ctx, h.vc, model, payload, state.output())
		if !state.streamFailed() {
			state.finish()
		}
	}
}

func (s *responsesStreamState) start() {
	response := s.responseObject("in_progress", protocolOutput{}, []any{})
	s.emitResponse("response.created", response)
	if s.streamFailed() {
		return
	}
	s.emitResponse("response.in_progress", response)
}

type responsesStreamState struct {
	sw              *sseWriter
	id              string
	model           string
	request         map[string]any
	namespaceTools  map[string]responsesNamespacedTool
	sequence        int
	outputIndex     int
	textID          string
	text            transform.StringAccumulator
	textBlocks      []string
	textCache       string
	textCacheValid  bool
	textOpen        bool
	textEvents      *responsesTextEventState
	functionEvents  *responsesFunctionEventState
	lifecycleEvents *responsesLifecycleEventState
	items           []any
	out             protocolOutput
}

// Responses text and function-call events are mutually exclusive for the
// common single-output case. Keep their reusable encoding state lazy so a
// text-only request does not retain function-call structs and vice versa.
type responsesTextEventState struct {
	deltaEvent       responsesOutputTextDeltaEvent
	itemEvent        responsesOutputTextItemEvent
	contentPartEvent responsesOutputTextContentPartEvent
	doneEvent        responsesOutputTextDoneEvent
	itemContent      [1]responsesOutputTextPart
}

type responsesFunctionEventState struct {
	itemEvent      responsesFunctionCallItemEvent
	argumentsEvent responsesFunctionCallArgumentsEvent
	arguments      string
}

type responsesLifecycleEventState struct {
	event       responsesLifecycleEvent
	response    responsesResponse
	err         responsesResponseError
	initialized bool
}

type responsesLifecycleEvent struct {
	Response       *responsesResponse `json:"response"`
	SequenceNumber int                `json:"sequence_number"`
	Type           string             `json:"type"`
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

type responsesCompletedTextMessageItem struct {
	Content [1]responsesOutputTextPart `json:"content"`
	ID      string                     `json:"id"`
	Role    string                     `json:"role"`
	Status  string                     `json:"status"`
	Type    string                     `json:"type"`
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
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Status    string `json:"status"`
	Type      string `json:"type"`
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

func (s *responsesStreamState) textEventState() *responsesTextEventState {
	if s.textEvents == nil {
		s.textEvents = &responsesTextEventState{}
	}
	return s.textEvents
}

func (s *responsesStreamState) functionEventState() *responsesFunctionEventState {
	if s.functionEvents == nil {
		s.functionEvents = &responsesFunctionEventState{}
	}
	return s.functionEvents
}

func (s *responsesStreamState) lifecycleEventState() *responsesLifecycleEventState {
	if s.lifecycleEvents == nil {
		s.lifecycleEvents = &responsesLifecycleEventState{}
	}
	return s.lifecycleEvents
}

func (s *responsesStreamState) emitResponse(event string, response *responsesResponse) {
	if s.streamFailed() {
		return
	}
	events := s.lifecycleEventState()
	s.sequence++
	events.event = responsesLifecycleEvent{
		Response: response, SequenceNumber: s.sequence, Type: event,
	}
	_ = s.sw.writeNamed(event, &events.event)
}

func (s *responsesStreamState) emitTextDelta(delta string) {
	if s.streamFailed() {
		return
	}
	events := s.textEventState()
	s.sequence++
	events.deltaEvent = responsesOutputTextDeltaEvent{
		Type:           "response.output_text.delta",
		SequenceNumber: s.sequence,
		ItemID:         s.textID,
		OutputIndex:    s.outputIndex,
		ContentIndex:   0,
		Delta:          delta,
		Logprobs:       []any{},
	}
	_ = s.sw.writeNamed("response.output_text.delta", &events.deltaEvent)
}

func (s *responsesStreamState) emitTextBlockStart() {
	if s.streamFailed() {
		return
	}
	events := s.textEventState()
	s.sequence++
	events.itemEvent = responsesOutputTextItemEvent{
		Item: responsesOutputTextMessageItem{
			Content: events.itemContent[:0],
			ID:      s.textID,
			Role:    "assistant",
			Status:  "in_progress",
			Type:    "message",
		},
		OutputIndex:    s.outputIndex,
		SequenceNumber: s.sequence,
		Type:           "response.output_item.added",
	}
	if !s.sw.writeNamed("response.output_item.added", &events.itemEvent) {
		return
	}

	s.sequence++
	events.contentPartEvent = responsesOutputTextContentPartEvent{
		ContentIndex: 0,
		ItemID:       s.textID,
		OutputIndex:  s.outputIndex,
		Part: responsesOutputTextPart{
			Annotations: []any{}, Logprobs: []any{}, Text: "", Type: "output_text",
		},
		SequenceNumber: s.sequence,
		Type:           "response.content_part.added",
	}
	_ = s.sw.writeNamed("response.content_part.added", &events.contentPartEvent)
}

func (s *responsesStreamState) emitTextBlockDone(text string) {
	if s.streamFailed() {
		return
	}
	events := s.textEventState()
	// Codex CLI SDK 严格解析 output_text.done 事件，期望 logprobs 和
	// annotations 字段都存在，即使它们是空数组。
	s.sequence++
	events.doneEvent = responsesOutputTextDoneEvent{
		Annotations:    []any{},
		ContentIndex:   0,
		ItemID:         s.textID,
		Logprobs:       []any{},
		OutputIndex:    s.outputIndex,
		SequenceNumber: s.sequence,
		Text:           text,
		Type:           "response.output_text.done",
	}
	if !s.sw.writeNamed("response.output_text.done", &events.doneEvent) {
		return
	}

	part := responsesOutputTextPart{
		Annotations: []any{}, Logprobs: []any{}, Text: text, Type: "output_text",
	}
	s.sequence++
	events.contentPartEvent = responsesOutputTextContentPartEvent{
		ContentIndex:   0,
		ItemID:         s.textID,
		OutputIndex:    s.outputIndex,
		Part:           part,
		SequenceNumber: s.sequence,
		Type:           "response.content_part.done",
	}
	if !s.sw.writeNamed("response.content_part.done", &events.contentPartEvent) {
		return
	}

	events.itemContent[0] = part
	s.sequence++
	events.itemEvent = responsesOutputTextItemEvent{
		Item: responsesOutputTextMessageItem{
			Content: events.itemContent[:],
			ID:      s.textID,
			Role:    "assistant",
			Status:  responsesOutputItemStatus(s.out.Finish),
			Type:    "message",
		},
		OutputIndex:    s.outputIndex,
		SequenceNumber: s.sequence,
		Type:           "response.output_item.done",
	}
	_ = s.sw.writeNamed("response.output_item.done", &events.itemEvent)
}

func (s *responsesStreamState) emitFunctionCallItem(
	event, status, itemID string,
	tc protocolToolCall,
	arguments string,
) {
	if s.streamFailed() {
		return
	}
	events := s.functionEventState()
	s.sequence++
	events.itemEvent = responsesFunctionCallItemEvent{
		Item: responsesFunctionCallItem{
			Arguments: arguments,
			CallID:    tc.ID,
			ID:        itemID,
			Name:      tc.Name,
			Namespace: tc.Namespace,
			Status:    status,
			Type:      "function_call",
		},
		OutputIndex:    s.outputIndex,
		SequenceNumber: s.sequence,
		Type:           event,
	}
	_ = s.sw.writeNamed(event, &events.itemEvent)
}

func (s *responsesStreamState) emitFunctionCallArguments(event, itemID, arguments string) {
	if s.streamFailed() {
		return
	}
	events := s.functionEventState()
	s.sequence++
	events.arguments = arguments
	events.argumentsEvent = responsesFunctionCallArgumentsEvent{
		ItemID:         itemID,
		OutputIndex:    s.outputIndex,
		SequenceNumber: s.sequence,
		Type:           event,
	}
	if event == "response.function_call_arguments.delta" {
		events.argumentsEvent.Delta = &events.arguments
	} else {
		events.argumentsEvent.Arguments = &events.arguments
	}
	_ = s.sw.writeNamed(event, &events.argumentsEvent)
}

func (s *responsesStreamState) responseObject(
	status string,
	out protocolOutput,
	output []any,
) *responsesResponse {
	events := s.lifecycleEventState()
	resp := &events.response
	if events.initialized {
		fillResponsesResult(resp, out, output)
	} else {
		fillResponsesResponse(resp, s.request, s.model, s.id, out, output)
		cacheResponsesStreamStaticFields(resp)
		events.initialized = true
	}
	if status != "" {
		resp.Status = status
	}
	if status == "in_progress" {
		resp.CompletedAt = nil
		resp.Usage = nil
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
	if len(chunk.ToolCalls) > 0 {
		additionalItems := len(chunk.ToolCalls)
		if s.textOpen {
			additionalItems++
		}
		s.items = slices.Grow(s.items, additionalItems)
		s.out.ToolCalls = slices.Grow(s.out.ToolCalls, len(chunk.ToolCalls))
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
		itemStatus := responsesOutputItemStatus(s.out.Finish)
		s.emitFunctionCallItem("response.output_item.done", itemStatus, itemID, tc, tc.Arguments)
		if s.streamFailed() {
			return
		}
		s.items = append(s.items, &responsesFunctionCallItem{
			Arguments: tc.Arguments,
			CallID:    tc.ID,
			ID:        itemID,
			Name:      tc.Name,
			Namespace: tc.Namespace,
			Status:    itemStatus,
			Type:      "function_call",
		})
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
	s.items = append(s.items, &responsesCompletedTextMessageItem{
		Content: [1]responsesOutputTextPart{{
			Annotations: []any{}, Logprobs: []any{}, Text: text, Type: "output_text",
		}},
		ID: s.textID, Role: "assistant", Status: responsesOutputItemStatus(s.out.Finish), Type: "message",
	})
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
	resp := s.responseObject("", s.out, s.items)
	event := "response.completed"
	if resp.Status == "incomplete" {
		event = "response.incomplete"
	}
	s.emitResponse(event, resp)
}

func (s *responsesStreamState) fail(err *vertex.VertexError) {
	if s.streamFailed() {
		return
	}
	resp := s.responseObject("failed", s.output(), nil)
	events := s.lifecycleEventState()
	events.err = responsesResponseError{Code: err.Kind, Message: vertex.FriendlyErrorMessage(err)}
	resp.Error = &events.err
	s.emitResponse("response.failed", resp)
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
