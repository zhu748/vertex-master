package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"slices"
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
	reqConv       transform.RequestConverter
	respConv      transform.ResponseConverter
	claudePrompts *claudePromptStore
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
	actualModel, useFake := resolveRequestedModel(rawModel, h.cfg)
	cli.UpdateReqModel(vertex.RequestIDFromContext(r.Context()), actualModel)

	model, payload, err := h.convertAnthropicRequest(body, rawModel, actualModel, "messages")
	if err != nil {
		h.writeAnthropicConversionError(w, err)
		return
	}

	if protocolBoolValue(body["stream"]) {
		h.streamMessages(r.Context(), w, rawModel, model, payload, useFake || h.cfg.AggregateStream())
		return
	}

	resp, vErr := h.vc.CompleteChat(r.Context(), model, payload)
	if vErr != nil {
		h.writeAnthropicVertexError(w, toVertexError(vErr))
		return
	}
	out := outputFromResponseConverterWithRawArguments(h.respConv, resp, model)
	out.Text = transform.StripAssistantPrefillEcho(
		out.Text,
		transform.AssistantPrefillFromPayload(payload),
	)
	out = completeProtocolUsageWithCountTokens(r.Context(), h.vc, model, payload, out)
	writeJSON(w, http.StatusOK, anthropicMessage(rawModel, reqID24WithPrefix("msg_"), out))
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
	actualModel, _ := resolveRequestedModel(rawModel, h.cfg)
	model, payload, err := h.convertAnthropicRequest(
		body,
		rawModel,
		actualModel,
		"count_tokens",
	)
	if err != nil {
		h.writeAnthropicConversionError(w, err)
		return
	}
	total, countErr := h.vc.CountTokensExact(r.Context(), model, protocolInputContents(payload))
	if countErr != nil {
		h.writeAnthropicVertexError(w, toVertexError(countErr))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": total})
}

func (h *AnthropicHandler) convertAnthropicRequest(
	body map[string]any,
	rawModel string,
	actualModel string,
	endpoint string,
) (string, map[string]any, error) {
	chatBody, err := anthropicToChatRequest(body)
	if err != nil {
		return "", nil, err
	}
	promptResult, err := applyClaudePromptPolicy(chatBody, h.cfg, rawModel, actualModel)
	if err != nil {
		return "", nil, err
	}
	chatBody["model"] = actualModel
	model, payload, err := h.reqConv.Convert(chatBody, h.cfg)
	if err != nil {
		return "", nil, err
	}
	transform.ApplyImageConfig(payload, chatBody)
	h.claudePrompts.Record(rawModel, endpoint, promptResult)
	return model, payload, nil
}

func (h *AnthropicHandler) writeAnthropicConversionError(w http.ResponseWriter, err error) {
	var policyErr *claudePromptPolicyError
	if !errors.As(err, &policyErr) {
		h.anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if policyErr.status >= http.StatusInternalServerError {
		log.Printf("[ClaudePrompt] invalid prompt policy: %v", policyErr)
	}
	h.anthropicError(w, policyErr.status, policyErr.typ, policyErr.message)
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
	// At most ten Anthropic fields map into the internal Chat request. Bound
	// the hint so unknown client extensions cannot cause a large duplicate map.
	const maxChatFields = 10
	chat := make(map[string]any, min(len(body), maxChatFields))
	for _, pair := range [][2]string{
		{"max_tokens", "max_completion_tokens"}, {"temperature", "temperature"},
		{"top_p", "top_p"}, {"top_k", "top_k"}, {"stop_sequences", "stop"},
	} {
		if v, ok := body[pair[0]]; ok {
			chat[pair[1]] = v
		}
	}
	if rawThinking, exists := body["thinking"]; exists && rawThinking != nil {
		thinking, ok := rawThinking.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("thinking must be an object")
		}
		thinkingType := strings.ToLower(strings.TrimSpace(stringValue(thinking["type"])))
		switch thinkingType {
		case "enabled":
			budget, ok := anthropicThinkingBudget(thinking["budget_tokens"])
			if !ok || budget < 1024 {
				return nil, fmt.Errorf("thinking.budget_tokens must be an integer >= 1024")
			}
			if err := validateAnthropicThinkingDisplay(thinking); err != nil {
				return nil, err
			}
			chat["thinking"] = thinking
		case "adaptive":
			if err := validateAnthropicThinkingDisplay(thinking); err != nil {
				return nil, err
			}
			chat["thinking"] = thinking
		case "disabled":
			chat["thinking"] = thinking
		default:
			return nil, fmt.Errorf("unsupported thinking.type %q", thinkingType)
		}
	}
	if rawOutputConfig, exists := body["output_config"]; exists && rawOutputConfig != nil {
		outputConfig, ok := rawOutputConfig.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("output_config must be an object")
		}
		if rawEffort, exists := outputConfig["effort"]; exists && rawEffort != nil {
			effort, ok := rawEffort.(string)
			if !ok || strings.TrimSpace(effort) == "" {
				return nil, fmt.Errorf("output_config.effort must be a non-empty string")
			}
			effort = strings.ToLower(strings.TrimSpace(effort))
			switch effort {
			case "low", "medium", "high", "xhigh", "max":
				chat["reasoning_effort"] = effort
			default:
				return nil, fmt.Errorf("unsupported output_config.effort %q", effort)
			}
		}
	}

	rawMessages, ok := body["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, fmt.Errorf("messages must be a non-empty array")
	}
	messages := make([]any, 0, len(rawMessages)+1)
	if rawSystem, exists := body["system"]; exists && rawSystem != nil {
		system, err := anthropicInstructionText(rawSystem)
		if err != nil {
			return nil, fmt.Errorf("system: %w", err)
		}
		if system != "" {
			messages = append(messages, map[string]any{"role": "system", "content": system})
		}
	}
	for messageIndex, raw := range rawMessages {
		message, _ := raw.(map[string]any)
		if message == nil {
			return nil, fmt.Errorf("messages[%d] must be an object", messageIndex)
		}
		role := stringValue(message["role"])
		if role == "system" || role == "developer" {
			content, err := anthropicInstructionText(message["content"])
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", messageIndex, err)
			}
			if content != "" {
				messages = append(messages, map[string]any{"role": "system", "content": content})
			}
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("message role %q must be 'user' or 'assistant'", role)
		}
		if anthropicMessageCanPassThrough(message, role) {
			messages = append(messages, message)
			continue
		}
		var err error
		messages, err = appendAnthropicMessageToChat(messages, role, message["content"])
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", messageIndex, err)
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("messages did not contain supported content")
	}
	chat["messages"] = messages

	if rawToolsValue, exists := body["tools"]; exists && rawToolsValue != nil {
		rawTools, ok := rawToolsValue.([]any)
		if !ok {
			return nil, fmt.Errorf("tools must be an array")
		}
		tools := make([]any, 0, len(rawTools))
		for toolIndex, raw := range rawTools {
			tool, _ := raw.(map[string]any)
			if tool == nil {
				return nil, fmt.Errorf("tools[%d] must be an object", toolIndex)
			}
			if stringValue(tool["name"]) == "" {
				return nil, fmt.Errorf("tools[%d] is missing name", toolIndex)
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
	if rawChoice, exists := body["tool_choice"]; exists && rawChoice != nil {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool_choice must be an object")
		}
		switch stringValue(choice["type"]) {
		case "auto":
			chat["tool_choice"] = "auto"
		case "any":
			chat["tool_choice"] = "required"
		case "none":
			chat["tool_choice"] = "none"
		case "tool":
			if stringValue(choice["name"]) == "" {
				return nil, fmt.Errorf("tool_choice type 'tool' requires name")
			}
			chat["tool_choice"] = map[string]any{
				"type": "function", "function": map[string]any{"name": choice["name"]},
			}
		default:
			return nil, fmt.Errorf(
				"unsupported tool_choice type %q",
				stringValue(choice["type"]),
			)
		}
	}
	return chat, nil
}

func anthropicThinkingBudget(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number ||
			number < math.MinInt64 || number >= math.MaxInt64 {
			return 0, false
		}
		return int64(number), true
	default:
		return 0, false
	}
}

func validateAnthropicThinkingDisplay(thinking map[string]any) error {
	rawDisplay, exists := thinking["display"]
	if !exists || rawDisplay == nil {
		return nil
	}
	display, ok := rawDisplay.(string)
	if !ok {
		return fmt.Errorf("thinking.display must be a string")
	}
	switch strings.ToLower(strings.TrimSpace(display)) {
	case "summarized", "omitted":
		return nil
	default:
		return fmt.Errorf("unsupported thinking.display %q", display)
	}
}

func anthropicMessageCanPassThrough(message map[string]any, role string) bool {
	if len(message) != 2 {
		return false
	}
	if _, ok := message["role"].(string); !ok {
		return false
	}
	content := message["content"]
	if _, ok := content.(string); ok {
		return true
	}
	textContent, ok := anthropicUserTextContentCanPassThrough(content)
	if !ok {
		return false
	}
	if role == "user" {
		return true
	}
	if role != "assistant" || len(textContent) != 1 {
		return false
	}
	block, _ := textContent[0].(map[string]any)
	text, _ := block["text"].(string)
	return text != ""
}

func anthropicInstructionText(v any) (string, error) {
	if text, ok := v.(string); ok {
		return text, nil
	}
	blocks, ok := v.([]any)
	if !ok {
		return "", fmt.Errorf("content must be a string or text block array")
	}
	var inline [8]string
	values := inline[:0]
	for blockIndex, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("content[%d] must be an object", blockIndex)
		}
		if typ := stringValue(block["type"]); typ != "text" {
			return "", fmt.Errorf("content[%d] has unsupported type %q", blockIndex, typ)
		}
		text, ok := block["text"].(string)
		if !ok {
			return "", fmt.Errorf("content[%d].text must be a string", blockIndex)
		}
		values = append(values, text)
	}
	return strings.Join(values, "\n"), nil
}

func anthropicTextBlocks(v any) (string, bool) {
	blocks, ok := v.([]any)
	if !ok {
		return "", false
	}
	var inline [8]string
	values := inline[:0]
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || stringValue(block["type"]) != "text" {
			return "", false
		}
		text, ok := block["text"].(string)
		if !ok {
			return "", false
		}
		values = append(values, text)
	}
	return strings.Join(values, "\n"), true
}

func appendAnthropicMessageToChat(result []any, role string, content any) ([]any, error) {
	if text, ok := content.(string); ok {
		return append(result, transform.CanonicalChatMessage{Role: role, Content: text}), nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("content must be a string or array")
	}
	if role == "assistant" {
		var text transform.StringAccumulator
		toolCalls := []any{}
		for blockIndex, raw := range blocks {
			block, _ := raw.(map[string]any)
			if block == nil {
				return nil, fmt.Errorf("content[%d] must be an object", blockIndex)
			}
			switch stringValue(block["type"]) {
			case "text":
				value, ok := block["text"].(string)
				if !ok {
					return nil, fmt.Errorf("content[%d].text must be a string", blockIndex)
				}
				text.WriteString(value)
			case "tool_use":
				if stringValue(block["id"]) == "" || stringValue(block["name"]) == "" {
					return nil, fmt.Errorf(
						"content[%d] tool_use requires id and name",
						blockIndex,
					)
				}
				toolCalls = append(toolCalls, transform.CanonicalOAIToolCall{
					ID:   stringValue(block["id"]),
					Type: "function",
					Function: transform.CanonicalOAIFunctionCallData{
						Name:      stringValue(block["name"]),
						Arguments: normalizedIntermediateJSONValue(block["input"]),
					},
				})
			case "thinking", "redacted_thinking":
				// Extended-thinking history is provider-private context. Gemini
				// cannot consume Anthropic signatures, so intentionally omit
				// these known blocks while preserving adjacent visible/tool data.
			default:
				return nil, fmt.Errorf(
					"content[%d] has unsupported assistant block type %q",
					blockIndex,
					stringValue(block["type"]),
				)
			}
		}
		msg := transform.CanonicalChatMessage{Role: "assistant", Content: text.String()}
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
		return append(result, msg), nil
	}
	if textContent, ok := anthropicUserTextContentCanPassThrough(content); ok {
		return append(result, transform.CanonicalChatMessage{Role: "user", Content: textContent}), nil
	}

	regular := []any{}
	flushRegular := func() {
		if len(regular) > 0 {
			result = append(result, transform.CanonicalChatMessage{Role: "user", Content: regular})
			regular = nil
		}
	}
	for blockIndex, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block == nil {
			return nil, fmt.Errorf("content[%d] must be an object", blockIndex)
		}
		switch stringValue(block["type"]) {
		case "text":
			text, ok := block["text"].(string)
			if !ok {
				return nil, fmt.Errorf("content[%d].text must be a string", blockIndex)
			}
			regular = append(regular, map[string]any{"type": "text", "text": text})
		case "image":
			source, _ := block["source"].(map[string]any)
			if source == nil {
				return nil, fmt.Errorf("content[%d].source must be an object", blockIndex)
			}
			switch stringValue(source["type"]) {
			case "base64":
				mediaType := stringValue(source["media_type"])
				data := stringValue(source["data"])
				if mediaType == "" || data == "" {
					return nil, fmt.Errorf(
						"content[%d] base64 image requires media_type and data",
						blockIndex,
					)
				}
				url := "data:" + mediaType + ";base64," + data
				regular = append(regular, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			case "url":
				url := stringValue(source["url"])
				if url == "" {
					return nil, fmt.Errorf("content[%d] URL image requires url", blockIndex)
				}
				regular = append(regular, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			default:
				return nil, fmt.Errorf(
					"content[%d] has unsupported image source type %q",
					blockIndex,
					stringValue(source["type"]),
				)
			}
		case "tool_result":
			toolUseID := stringValue(block["tool_use_id"])
			if toolUseID == "" {
				return nil, fmt.Errorf("content[%d] tool_result requires tool_use_id", blockIndex)
			}
			flushRegular()
			result = append(result, transform.CanonicalChatMessage{
				Role: "tool", ToolCallID: toolUseID, Content: anthropicToolResult(block["content"]),
			})
		default:
			return nil, fmt.Errorf(
				"content[%d] has unsupported user block type %q",
				blockIndex,
				stringValue(block["type"]),
			)
		}
	}
	flushRegular()
	return result, nil
}

func anthropicUserTextContentCanPassThrough(content any) ([]any, bool) {
	blocks, ok := content.([]any)
	if !ok || len(blocks) == 0 {
		return nil, false
	}
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || stringValue(block["type"]) != "text" {
			return nil, false
		}
		if _, ok := block["text"].(string); !ok {
			return nil, false
		}
	}
	return blocks, true
}

func anthropicToolResult(v any) any {
	if s, ok := v.(string); ok {
		return s
	}
	if text, ok := anthropicTextBlocks(v); ok && text != "" {
		return text
	}
	return normalizedIntermediateJSONValue(v)
}

type anthropicMessageResponse struct {
	Content      []any              `json:"content"`
	ID           string             `json:"id"`
	Model        string             `json:"model"`
	Role         string             `json:"role"`
	StopReason   string             `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Type         string             `json:"type"`
	Usage        anthropicUsageData `json:"usage"`
}

type anthropicThinkingContent struct {
	Signature string `json:"signature"`
	Thinking  string `json:"thinking"`
	Type      string `json:"type"`
}

type anthropicTextContent struct {
	Text string `json:"text"`
	Type string `json:"type"`
}

type anthropicToolUseContent struct {
	ID    string `json:"id"`
	Input any    `json:"input"`
	Name  string `json:"name"`
	Type  string `json:"type"`
}

type anthropicUsageData struct {
	CacheCreationInputTokens int                          `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                          `json:"cache_read_input_tokens"`
	InputTokens              int                          `json:"input_tokens"`
	OutputTokens             int                          `json:"output_tokens"`
	OutputTokensDetails      *anthropicOutputTokenDetails `json:"output_tokens_details,omitempty"`

	outputTokensDetails anthropicOutputTokenDetails `json:"-"`
}

type anthropicOutputTokenDetails struct {
	ThinkingTokens int `json:"thinking_tokens"`
}

func anthropicMessage(model, id string, out protocolOutput) *anthropicMessageResponse {
	capacity := len(out.ToolCalls)
	if out.Reasoning != "" {
		capacity++
	}
	if out.Text != "" || len(out.ToolCalls) == 0 {
		capacity++
	}
	message := &anthropicMessageResponse{
		Content: make([]any, 0, capacity),
		ID:      id,
		Model:   model,
		Role:    "assistant",
		Type:    "message",
	}
	if out.Reasoning != "" {
		// 非流式响应同样需要非空 signature，否则 Claude Code SDK 在下一轮对话历史中会拒绝该 thinking 块。
		message.Content = append(message.Content, &anthropicThinkingContent{
			Signature: thinkingSignature(out.Reasoning), Thinking: out.Reasoning, Type: "thinking",
		})
	}
	if out.Text != "" || len(out.ToolCalls) == 0 {
		message.Content = append(message.Content, &anthropicTextContent{Text: out.Text, Type: "text"})
	}
	toolContent := make([]anthropicToolUseContent, len(out.ToolCalls))
	packedInputRaw := make([]byte, 0, anthropicToolInputRawCapacity(out.ToolCalls))
	for index, tc := range out.ToolCalls {
		var input any
		input, packedInputRaw = anthropicToolInputPacked(tc, packedInputRaw)
		toolContent[index] = anthropicToolUseContent{
			ID: tc.ID, Input: input, Name: tc.Name, Type: "tool_use",
		}
		message.Content = append(message.Content, &toolContent[index])
	}
	message.StopReason = anthropicStopReason(out.Finish, len(out.ToolCalls) > 0)
	fillAnthropicUsage(&message.Usage, out)
	return message
}

// Reserve one shared backing buffer for string-backed tool inputs. Validation is
// deliberately deferred to anthropicToolInputPacked so each argument is scanned
// only once on the response hot path.
func anthropicToolInputRawCapacity(toolCalls []protocolToolCall) int {
	capacity := 0
	for _, toolCall := range toolCalls {
		if toolCall.argumentsValue != nil || len(toolCall.argumentsRaw) > 0 {
			continue
		}
		if len(toolCall.Arguments) > int(^uint(0)>>1)-capacity {
			return 0
		}
		capacity += len(toolCall.Arguments)
	}
	return capacity
}

func anthropicToolInputPacked(toolCall protocolToolCall, packedRaw []byte) (any, []byte) {
	if toolCall.argumentsValue != nil {
		return toolCall.argumentsValue, packedRaw
	}
	if len(toolCall.argumentsRaw) > 0 {
		return toolCall.argumentsRaw, packedRaw
	}
	raw := json.RawMessage(toolCall.Arguments)
	if json.Valid(raw) {
		start := len(packedRaw)
		packedRaw = append(packedRaw, raw...)
		end := len(packedRaw)
		return json.RawMessage(packedRaw[start:end:end]), packedRaw
	}
	return jsonValue(toolCall.Arguments), packedRaw
}

func anthropicUsage(out protocolOutput) *anthropicUsageData {
	usage := &anthropicUsageData{}
	fillAnthropicUsage(usage, out)
	return usage
}

func fillAnthropicUsage(usage *anthropicUsageData, out protocolOutput) {
	totalInput := max(out.Input, 0)
	cacheRead := min(max(out.CachedInputTokens, 0), totalInput)
	*usage = anthropicUsageData{
		CacheReadInputTokens: cacheRead,
		InputTokens:          totalInput - cacheRead,
		OutputTokens:         max(out.Output, 0),
	}
	if thinking := min(max(out.ReasoningTokens, 0), max(out.Output, 0)); thinking > 0 {
		usage.outputTokensDetails.ThinkingTokens = thinking
		usage.OutputTokensDetails = &usage.outputTokensDetails
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
		sw: newSSEWriter(w, "text/event-stream"), id: reqID24WithPrefix("msg_"), model: displayModel,
	}
	state.start()
	if !state.connected() {
		return
	}

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
		if !state.connected() {
			pingCancel()
			pingWg.Wait()
			return
		}
		out := outputFromResponseConverter(h.respConv, resp, model)
		out.Text = transform.StripAssistantPrefillEcho(
			out.Text,
			transform.AssistantPrefillFromPayload(payload),
		)
		out = completeProtocolUsageWithCountTokens(ctx, h.vc, model, payload, out)
		if !state.connected() {
			pingCancel()
			pingWg.Wait()
			return
		}
		state.consume(out)
		pingCancel()
		pingWg.Wait()
		state.finish()
		return
	}
	failed := false
	var lastCandidateTokenCount int
	prefillFilter := transform.NewAssistantPrefillStreamFilter(
		transform.AssistantPrefillFromPayload(payload),
	)
	h.vc.StreamChat(ctx, model, payload, func(chunk vertex.StreamChunk) bool {
		if !state.connected() {
			return false
		}
		if chunk.Err != nil {
			state.fail(chunk.Err)
			failed = true
			return false
		}
		if chunk.HasCanonicalText {
			state.consume(outputFromCanonicalTextStreamData(chunk.CanonicalText, prefillFilter))
			return state.connected()
		}
		data := chunk.GeminiData()
		prefillFilter.FilterGeminiChunk(data)
		normalizedUsage, hasUsage := normalizeStreamingGeminiUsage(data, &lastCandidateTokenCount)
		state.consume(outputFromGeminiChunkWithUsage(data, normalizedUsage, hasUsage))
		return state.connected()
	})
	pingCancel()
	pingWg.Wait()
	if !failed && state.connected() {
		if tail := prefillFilter.Finalize(); tail != "" {
			state.consume(protocolOutput{Text: tail})
		}
		state.out = completeProtocolUsageWithCountTokens(ctx, h.vc, model, payload, state.output())
		if state.connected() {
			state.finish()
		}
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
	blockStartEvent     anthropicContentBlockStartEvent
	blockStopEvent      anthropicContentBlockStopEvent
	emptyString         string
	emptyObject         struct{}
	toolID              string
	toolName            string
	lifecycleEvents     *anthropicLifecycleEventState
	out                 protocolOutput
	mu                  sync.Mutex // 保护 sw 的并发写（ping goroutine 与主回调可能竞争）
}

type anthropicLifecycleEventState struct {
	messageStart anthropicMessageStartEvent
	messageDelta anthropicMessageDeltaEvent
	messageStop  anthropicMessageStopEvent
	failure      anthropicErrorEvent
}

type anthropicMessageStartEvent struct {
	Message anthropicMessageStart `json:"message"`
	Type    string                `json:"type"`
}

type anthropicMessageStart struct {
	Content      [0]any             `json:"content"`
	ID           string             `json:"id"`
	Model        string             `json:"model"`
	Role         string             `json:"role"`
	StopReason   *string            `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Type         string             `json:"type"`
	Usage        anthropicUsageData `json:"usage"`
}

type anthropicMessageDeltaEvent struct {
	Delta anthropicMessageDelta `json:"delta"`
	Type  string                `json:"type"`
	Usage anthropicUsageData    `json:"usage"`
}

type anthropicMessageDelta struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type anthropicMessageStopEvent struct {
	Type string `json:"type"`
}

type anthropicErrorEvent struct {
	Error anthropicErrorPayload `json:"error"`
	Type  string                `json:"type"`
}

type anthropicErrorPayload struct {
	Message string `json:"message"`
	Type    string `json:"type"`
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

type anthropicContentBlockStartEvent struct {
	ContentBlock anthropicContentBlockStart `json:"content_block"`
	Index        int                        `json:"index"`
	Type         string                     `json:"type"`
}

// Field order follows encoding/json's sorted map-key order so this typed path
// remains byte-for-byte compatible with the previous map representation.
type anthropicContentBlockStart struct {
	ID        *string   `json:"id,omitempty"`
	Input     *struct{} `json:"input,omitempty"`
	Name      *string   `json:"name,omitempty"`
	Signature *string   `json:"signature,omitempty"`
	Text      *string   `json:"text,omitempty"`
	Thinking  *string   `json:"thinking,omitempty"`
	Type      string    `json:"type"`
}

type anthropicContentBlockStopEvent struct {
	Index int    `json:"index"`
	Type  string `json:"type"`
}

func (s *anthropicStreamState) emitContentBlockDelta(delta anthropicContentBlockDelta) {
	if !s.connected() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deltaEvent = anthropicContentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: s.index,
		Delta: delta,
	}
	s.writeNamedLocked("content_block_delta", &s.deltaEvent)
}

func (s *anthropicStreamState) emitContentBlockStart(block anthropicContentBlockStart) {
	if !s.connected() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockStartEvent = anthropicContentBlockStartEvent{
		ContentBlock: block,
		Index:        s.index,
		Type:         "content_block_start",
	}
	s.writeNamedLocked("content_block_start", &s.blockStartEvent)
}

func (s *anthropicStreamState) emitContentBlockStop() {
	if !s.connected() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockStopEvent = anthropicContentBlockStopEvent{Index: s.index, Type: "content_block_stop"}
	s.writeNamedLocked("content_block_stop", &s.blockStopEvent)
}

func (s *anthropicStreamState) lifecycleEventState() *anthropicLifecycleEventState {
	if s.lifecycleEvents == nil {
		s.lifecycleEvents = &anthropicLifecycleEventState{}
	}
	return s.lifecycleEvents
}

func (s *anthropicStreamState) emitMessageStart() {
	if !s.connected() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events := s.lifecycleEventState()
	events.messageStart = anthropicMessageStartEvent{
		Message: anthropicMessageStart{
			ID: s.id, Model: s.model, Role: "assistant", Type: "message",
		},
		Type: "message_start",
	}
	fillAnthropicUsage(&events.messageStart.Message.Usage, protocolOutput{})
	s.writeNamedLocked("message_start", &events.messageStart)
}

func (s *anthropicStreamState) emitMessageDelta() {
	if !s.connected() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events := s.lifecycleEventState()
	events.messageDelta = anthropicMessageDeltaEvent{
		Delta: anthropicMessageDelta{
			StopReason: anthropicStopReason(s.out.Finish, len(s.out.ToolCalls) > 0),
		},
		Type: "message_delta",
	}
	fillAnthropicUsage(&events.messageDelta.Usage, s.out)
	s.writeNamedLocked("message_delta", &events.messageDelta)
}

func (s *anthropicStreamState) emitMessageStop() {
	if !s.connected() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events := s.lifecycleEventState()
	events.messageStop = anthropicMessageStopEvent{Type: "message_stop"}
	s.writeNamedLocked("message_stop", &events.messageStop)
}

func (s *anthropicStreamState) emitError(err *vertex.VertexError) {
	if !s.connected() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events := s.lifecycleEventState()
	events.failure = anthropicErrorEvent{
		Error: anthropicErrorPayload{
			Message: vertex.FriendlyErrorMessage(err), Type: anthropicErrorType(err),
		},
		Type: "error",
	}
	s.writeNamedLocked("error", &events.failure)
}

func (s *anthropicStreamState) connected() bool {
	return s != nil && (s.sw == nil || !s.sw.failed.Load())
}

// writeNamedLocked writes while the caller holds mu and remembers a broken
// client connection so the upstream streaming callback can stop immediately.
func (s *anthropicStreamState) writeNamedLocked(event string, payload any) bool {
	if !s.connected() {
		return false
	}
	if !s.sw.writeNamed(event, payload) {
		return false
	}
	return true
}

// emitPing 发送一个 Anthropic 协议的 ping 保活事件。
func (s *anthropicStreamState) emitPing() bool {
	if !s.connected() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected() {
		return false
	}
	if !s.sw.write("event: ping\ndata: {}\n\n") {
		return false
	}
	return true
}

func (s *anthropicStreamState) start() {
	s.emitMessageStart()
}

func (s *anthropicStreamState) consume(chunk protocolOutput) {
	if !s.connected() {
		return
	}
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
			if !s.connected() {
				return
			}
			s.openType = "thinking"
			s.blockThinking.Reset()
			s.emitContentBlockStart(anthropicContentBlockStart{
				Signature: &s.emptyString, Thinking: &s.emptyString, Type: "thinking",
			})
			if !s.connected() {
				return
			}
		}
		s.emitContentBlockDelta(anthropicContentBlockDelta{
			Type: "thinking_delta", Thinking: chunk.Reasoning,
		})
		if !s.connected() {
			return
		}
		s.blockThinking.WriteString(chunk.Reasoning)
		s.reasoningCacheValid = false
	}
	if chunk.Text != "" {
		if s.openType != "text" {
			s.closeBlock()
			if !s.connected() {
				return
			}
			s.openType = "text"
			s.emitContentBlockStart(anthropicContentBlockStart{Text: &s.emptyString, Type: "text"})
			if !s.connected() {
				return
			}
		}
		s.text.WriteString(chunk.Text)
		s.emitContentBlockDelta(anthropicContentBlockDelta{Type: "text_delta", Text: chunk.Text})
		if !s.connected() {
			return
		}
	}
	if len(chunk.ToolCalls) > 0 {
		s.out.ToolCalls = slices.Grow(s.out.ToolCalls, len(chunk.ToolCalls))
	}
	for _, tc := range chunk.ToolCalls {
		if !s.connected() {
			return
		}
		s.closeBlock()
		if !s.connected() {
			return
		}
		s.toolID = tc.ID
		s.toolName = tc.Name
		s.emitContentBlockStart(anthropicContentBlockStart{
			ID: &s.toolID, Input: &s.emptyObject, Name: &s.toolName, Type: "tool_use",
		})
		if !s.connected() {
			return
		}
		s.emitContentBlockDelta(anthropicContentBlockDelta{
			Type: "input_json_delta", PartialJSON: tc.Arguments,
		})
		if !s.connected() {
			return
		}
		s.emitContentBlockStop()
		if !s.connected() {
			return
		}
		s.index++
		s.out.ToolCalls = append(s.out.ToolCalls, tc)
	}
}

func (s *anthropicStreamState) closeBlock() {
	if s.openType == "" || !s.connected() {
		return
	}
	if s.openType == "thinking" {
		// Claude Code SDK 要求 thinking 块在 content_block_stop 前必须有非空
		// signature_delta。连续思考增量必须共享同一个块，并对完整块签名。
		thinking := s.blockThinking.String()
		s.emitContentBlockDelta(anthropicContentBlockDelta{
			Type: "signature_delta", Signature: thinkingSignature(thinking),
		})
		if !s.connected() {
			return
		}
		s.reasoningBlocks = append(s.reasoningBlocks, thinking)
		s.blockThinking.Reset()
	}
	s.emitContentBlockStop()
	s.index++
	s.openType = ""
}

func (s *anthropicStreamState) finish() {
	if !s.connected() {
		return
	}
	s.closeBlock()
	if !s.connected() {
		return
	}
	s.out = s.output()
	s.emitMessageDelta()
	s.emitMessageStop()
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
	s.emitError(err)
}

func (h *AnthropicHandler) anthropicError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error", "error": map[string]any{"type": typ, "message": message},
		"request_id": reqID24WithPrefix("req_"),
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
