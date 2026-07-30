package api

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/transform"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

type protocolToolCall struct {
	ID        string
	Name      string
	Namespace string
	Arguments string
}

type protocolOutput struct {
	Text              string
	Reasoning         string
	ToolCalls         []protocolToolCall
	Finish            string
	Input             int
	Output            int
	Total             int
	CachedInputTokens int
	ReasoningTokens   int
}

type protocolOutputAccumulator struct {
	out       protocolOutput
	text      transform.StringAccumulator
	reasoning transform.StringAccumulator
}

func (a *protocolOutputAccumulator) Add(chunk protocolOutput) {
	if a == nil {
		return
	}
	a.text.WriteString(chunk.Text)
	a.reasoning.WriteString(chunk.Reasoning)
	a.out.ToolCalls = append(a.out.ToolCalls, chunk.ToolCalls...)
	if chunk.Finish != "" {
		a.out.Finish = chunk.Finish
	}
	if chunk.Input > 0 {
		a.out.Input = chunk.Input
	}
	if chunk.Output > 0 {
		a.out.Output = chunk.Output
	}
	if chunk.Total > 0 {
		a.out.Total = chunk.Total
	}
	if chunk.CachedInputTokens > 0 {
		a.out.CachedInputTokens = chunk.CachedInputTokens
	}
	if chunk.ReasoningTokens > 0 {
		a.out.ReasoningTokens = chunk.ReasoningTokens
	}
}

func (a *protocolOutputAccumulator) AddText(text string) {
	if a != nil {
		a.text.WriteString(text)
	}
}

func (a *protocolOutputAccumulator) Output() protocolOutput {
	if a == nil {
		return protocolOutput{}
	}
	out := a.out
	out.Text = a.text.String()
	out.Reasoning = a.reasoning.String()
	return out
}

func joinProtocolTextBlocks(blocks []string, current string) string {
	if len(blocks) == 0 {
		return current
	}
	if len(blocks) == 1 && current == "" {
		return blocks[0]
	}
	var accumulator transform.StringAccumulator
	for _, block := range blocks {
		accumulator.WriteString(block)
	}
	accumulator.WriteString(current)
	return accumulator.String()
}

// normalizeProtocolUsage 只根据上游已返回的字段补齐可确定的分项，不进行本地
// token 估算。上游没有 usage 时保持为零，调用方不会向客户端伪造统计。
func normalizeProtocolUsage(out protocolOutput) protocolOutput {
	if out.Input == 0 && out.Total > out.Output && out.Output > 0 {
		out.Input = out.Total - out.Output
	}
	if out.Output == 0 && out.Total > out.Input && out.Input > 0 {
		out.Output = out.Total - out.Input
	}
	if out.Total == 0 && out.Input > 0 && out.Output > 0 {
		out.Total = addProtocolCounts(out.Input, out.Output)
	}
	return out
}

// completeProtocolUsageWithCountTokens 只在生成响应缺少 usage 时调用匿名
// Vertex 的真实 CountTokens operation 补齐缺失分项。它不做本地估算，也不
// 覆盖生成接口已经返回的统计；查询失败时相应字段继续保持为零。
func completeProtocolUsageWithCountTokens(
	ctx context.Context,
	vc *vertex.VertexAIClient,
	model string,
	payload map[string]any,
	out protocolOutput,
) protocolOutput {
	out = normalizeProtocolUsage(out)
	needInput := out.Input == 0
	needOutput := out.Output == 0
	if (!needInput && !needOutput) || vc == nil {
		return out
	}

	inputContents := protocolInputContents(payload)
	outputContents := protocolOutputContents(out)
	if len(inputContents) == 0 {
		needInput = false
	}
	if len(outputContents) == 0 {
		needOutput = false
	}

	contentSets := make([][]any, 0, 2)
	inputIndex, outputIndex := -1, -1
	if needInput {
		inputIndex = len(contentSets)
		contentSets = append(contentSets, inputContents)
	}
	if needOutput {
		outputIndex = len(contentSets)
		contentSets = append(contentSets, outputContents)
	}
	counts := vc.CountTokenSets(ctx, model, contentSets...)

	if inputIndex >= 0 && inputIndex < len(counts) && counts[inputIndex] > 0 {
		out.Input = counts[inputIndex]
	}
	if outputIndex >= 0 && outputIndex < len(counts) && counts[outputIndex] > 0 {
		out.Output = counts[outputIndex]
	}
	return normalizeProtocolUsage(out)
}

func protocolInputContents(payload map[string]any) []any {
	contents := make([]any, 0, len(anySlice(payload["contents"]))+1)
	if system, ok := payload["systemInstruction"].(map[string]any); ok && len(system) > 0 {
		// CountTokens operation 仅接收 contents；用独立 user content 传入 system
		// parts，使系统提示也由模型 tokenizer 计入，而不是在本地估算。
		systemContent := make(map[string]any, len(system))
		for key, value := range system {
			systemContent[key] = value
		}
		if stringValue(systemContent["role"]) == "" {
			systemContent["role"] = "user"
		}
		contents = append(contents, systemContent)
	}
	contents = append(contents, anySlice(payload["contents"])...)
	return contents
}

func protocolOutputContents(out protocolOutput) []any {
	parts := make([]any, 0, 2+len(out.ToolCalls))
	if out.Reasoning != "" {
		parts = append(parts, map[string]any{"text": out.Reasoning})
	}
	if out.Text != "" {
		parts = append(parts, map[string]any{"text": out.Text})
	}
	for _, toolCall := range out.ToolCalls {
		functionCall := map[string]any{
			"name": toolCall.Name,
			"args": jsonValue(toolCall.Arguments),
		}
		if toolCall.ID != "" {
			functionCall["id"] = toolCall.ID
		}
		parts = append(parts, map[string]any{"functionCall": functionCall})
	}
	if len(parts) == 0 {
		return nil
	}
	return []any{map[string]any{
		"role":  "model",
		"parts": parts,
	}}
}

func hasProtocolUsage(out protocolOutput) bool {
	return out.Input > 0 || out.Output > 0 || out.Total > 0
}

func applyOAIUsage(out protocolOutput, response map[string]any) {
	if !hasProtocolUsage(out) {
		return
	}
	usage, _ := response["usage"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
		response["usage"] = usage
	}
	if protocolIntValue(usage["prompt_tokens"]) == 0 {
		usage["prompt_tokens"] = out.Input
	}
	if protocolIntValue(usage["completion_tokens"]) == 0 {
		usage["completion_tokens"] = out.Output
	}
	if protocolIntValue(usage["total_tokens"]) == 0 {
		usage["total_tokens"] = out.Total
	}
	if out.CachedInputTokens > 0 {
		details, _ := usage["prompt_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
			usage["prompt_tokens_details"] = details
		}
		details["cached_tokens"] = out.CachedInputTokens
	}
	if out.ReasoningTokens > 0 {
		details, _ := usage["completion_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
			usage["completion_tokens_details"] = details
		}
		details["reasoning_tokens"] = out.ReasoningTokens
	}
}

func applyGeminiUsage(out protocolOutput, response map[string]any) {
	if !hasProtocolUsage(out) {
		return
	}
	usage, _ := response["usageMetadata"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
		response["usageMetadata"] = usage
	}
	if protocolIntValue(usage["promptTokenCount"]) == 0 {
		usage["promptTokenCount"] = out.Input
	}
	if protocolIntValue(usage["candidatesTokenCount"])+protocolIntValue(usage["thoughtsTokenCount"]) == 0 {
		usage["candidatesTokenCount"] = out.Output
	}
	if protocolIntValue(usage["totalTokenCount"]) == 0 {
		usage["totalTokenCount"] = out.Total
	}
	if out.CachedInputTokens > 0 && protocolIntValue(usage["cachedContentTokenCount"]) == 0 {
		usage["cachedContentTokenCount"] = out.CachedInputTokens
	}
}

type invalidUTF8Error struct{}

func (invalidUTF8Error) Error() string { return "invalid UTF-8 in request body" }

type trailingJSONValueError struct {
	cause error
}

func (e trailingJSONValueError) Error() string {
	if e.cause == nil {
		return "request body must contain exactly one JSON value"
	}
	return "request body must contain exactly one JSON value: " + e.cause.Error()
}

func (e trailingJSONValueError) Unwrap() error { return e.cause }

type utf8ValidatingReader struct {
	reader       io.Reader
	sequence     [utf8.UTFMax]byte
	sequenceLen  int
	sequenceWant int
}

func (r *utf8ValidatingReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	data := buffer[:read]
	if r.sequenceLen > 0 {
		needed := r.sequenceWant - r.sequenceLen
		take := min(needed, len(data))
		for _, value := range data[:take] {
			if value&0xc0 != 0x80 {
				return 0, invalidUTF8Error{}
			}
		}
		copy(r.sequence[r.sequenceLen:], data[:take])
		r.sequenceLen += take
		data = data[take:]
		if r.sequenceLen == r.sequenceWant {
			if !utf8.Valid(r.sequence[:r.sequenceLen]) {
				return 0, invalidUTF8Error{}
			}
			r.sequenceLen = 0
			r.sequenceWant = 0
		}
	}
	if r.sequenceLen == 0 && len(data) > 0 && !utf8.Valid(data) {
		if !r.keepIncompleteSuffix(data) {
			return 0, invalidUTF8Error{}
		}
	}
	if err == io.EOF && r.sequenceLen != 0 {
		return 0, invalidUTF8Error{}
	}
	return read, err
}

func (r *utf8ValidatingReader) keepIncompleteSuffix(data []byte) bool {
	maximum := min(utf8.UTFMax-1, len(data))
	for suffixLen := 1; suffixLen <= maximum; suffixLen++ {
		start := len(data) - suffixLen
		want := utf8SequenceSize(data[start])
		if want <= suffixLen {
			continue
		}
		continuations := true
		for _, value := range data[start+1:] {
			if value&0xc0 != 0x80 {
				continuations = false
				break
			}
		}
		if continuations && utf8.Valid(data[:start]) {
			copy(r.sequence[:], data[start:])
			r.sequenceLen = suffixLen
			r.sequenceWant = want
			return true
		}
	}
	return false
}

func utf8SequenceSize(first byte) int {
	switch {
	case first >= 0xc2 && first <= 0xdf:
		return 2
	case first >= 0xe0 && first <= 0xef:
		return 3
	case first >= 0xf0 && first <= 0xf4:
		return 4
	default:
		return 0
	}
}

func decodeJSONValue(reader io.Reader, target any) error {
	validated := &utf8ValidatingReader{reader: reader}
	decoder := json.NewDecoder(validated)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return trailingJSONValueError{cause: err}
	}
	return nil
}

func decodeJSONObject(reader io.Reader) (map[string]any, error) {
	var body map[string]any
	if err := decodeJSONValue(reader, &body); err != nil {
		return nil, err
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, nil
}

func outputFromOAI(resp map[string]any) protocolOutput {
	var out protocolOutput
	choices, _ := resp["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		out.Finish, _ = choice["finish_reason"].(string)
		message, _ := choice["message"].(map[string]any)
		out.Text, _ = message["content"].(string)
		out.Reasoning, _ = message["reasoning_content"].(string)
		toolCalls := anySlice(message["tool_calls"])
		if len(toolCalls) > 0 {
			out.ToolCalls = make([]protocolToolCall, 0, len(toolCalls))
		}
		for _, raw := range toolCalls {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			id := stringValue(tc["id"])
			if id == "" {
				id = "call_" + reqID24()
			}
			out.ToolCalls = append(out.ToolCalls, protocolToolCall{
				ID: id, Name: stringValue(fn["name"]), Arguments: jsonString(fn["arguments"]),
			})
		}
	}
	if usage, ok := resp["usage"].(map[string]any); ok {
		out.Input = protocolIntValue(usage["prompt_tokens"])
		out.Output = protocolIntValue(usage["completion_tokens"])
		out.Total = protocolIntValue(usage["total_tokens"])
		if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			out.CachedInputTokens = protocolIntValue(details["cached_tokens"])
		}
		if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
			out.ReasoningTokens = protocolIntValue(details["reasoning_tokens"])
		}
	}
	if out.Total == 0 && out.Input > 0 && out.Output > 0 {
		out.Total = addProtocolCounts(out.Input, out.Output)
	}
	return out
}

func outputFromGeminiChunk(chunk map[string]any) protocolOutput {
	return outputFromGeminiChunkWithUsage(chunk, transform.NormalizedUsage{}, false)
}

func outputFromCanonicalTextStreamData(
	chunk vertex.CanonicalTextStreamData,
	prefillFilter *transform.AssistantPrefillStreamFilter,
) protocolOutput {
	return protocolOutput{
		Text:   prefillFilter.FilterTextChunk(0, chunk.Text, chunk.FinishReason),
		Finish: chunk.FinishReason,
	}
}

func outputFromGeminiChunkWithUsage(
	chunk map[string]any,
	normalizedUsage transform.NormalizedUsage,
	hasNormalizedUsage bool,
) protocolOutput {
	var out protocolOutput
	var textAccumulator transform.StringAccumulator
	var reasoningAccumulator transform.StringAccumulator
	candidates := anySlice(chunk["candidates"])
	var candidate map[string]any
	if len(candidates) > 0 {
		candidate, _ = candidates[0].(map[string]any)
		// 匿名 Vertex 端点即使不返回 usageMetadata，也经常在最终候选中
		// 返回真实的 tokenCount。该字段只代表候选输出，不能据此估算输入。
		out.Output = protocolIntValue(candidate["tokenCount"])
		out.Finish = stringValue(candidate["finishReason"])
		content, _ := candidate["content"].(map[string]any)
		parts := anySlice(content["parts"])
		for partIndex, raw := range parts {
			part, _ := raw.(map[string]any)
			if part == nil {
				continue
			}
			if fc, ok := part["functionCall"].(map[string]any); ok && stringValue(fc["name"]) != "" {
				if out.ToolCalls == nil {
					toolCallCount := 1
					for _, remainingRaw := range parts[partIndex+1:] {
						remainingPart, _ := remainingRaw.(map[string]any)
						remainingCall, _ := remainingPart["functionCall"].(map[string]any)
						if stringValue(remainingCall["name"]) != "" {
							toolCallCount++
						}
					}
					out.ToolCalls = make([]protocolToolCall, 0, toolCallCount)
				}
				id := stringValue(fc["id"])
				if id == "" {
					id = "call_" + reqID24()
				}
				out.ToolCalls = append(out.ToolCalls, protocolToolCall{
					ID: id, Name: stringValue(fc["name"]), Arguments: jsonString(fc["args"]),
				})
				continue
			}
			text := stringValue(part["text"])
			if text == "" {
				continue
			}
			if protocolBoolValue(part["thought"]) {
				reasoningAccumulator.WriteString(text)
			} else {
				textAccumulator.WriteString(text)
			}
		}
	}
	out.Text = textAccumulator.String()
	out.Reasoning = reasoningAccumulator.String()
	if usage, ok := chunk["usageMetadata"].(map[string]any); ok {
		if !hasNormalizedUsage {
			normalizedUsage = transform.NormalizeUsageForCandidate(usage, candidate)
		}
		out.Input = normalizedUsage.PromptTokens
		if output := normalizedUsage.CompletionTokens; output > 0 {
			out.Output = output
		}
		out.Total = normalizedUsage.TotalTokens
		out.CachedInputTokens = normalizedUsage.CachedInputTokens
		out.ReasoningTokens = normalizedUsage.ReasoningTokens
	}
	if out.Total == 0 && out.Input > 0 && out.Output > 0 {
		out.Total = addProtocolCounts(out.Input, out.Output)
	}
	return out
}

func anySlice(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	default:
		return []any{x}
	}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func protocolBoolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

func protocolIntValue(v any) int {
	var count int
	switch n := v.(type) {
	case int:
		count = n
	case int64:
		if n < 0 || uint64(n) > uint64(math.MaxInt) {
			return 0
		}
		count = int(n)
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || math.Trunc(n) != n ||
			n >= float64(uint64(1)<<(strconv.IntSize-1)) {
			return 0
		}
		count = int(n)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0
		}
		count = parsed
	default:
		return 0
	}
	if count < 0 {
		return 0
	}
	return count
}

func addProtocolCounts(left, right int) int {
	if left < 0 || right < 0 || left > math.MaxInt-right {
		return 0
	}
	return left + right
}

func jsonString(v any) string {
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	if v == nil {
		return "{}"
	}
	data, err := jsonx.MarshalString(v)
	if err != nil {
		return "{}"
	}
	return data
}

func jsonValue(s string) any {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}
	}
	var value any
	if err := json.Unmarshal([]byte(s), &value); err != nil {
		return map[string]any{"raw": s}
	}
	return value
}

func namedSSE(event string, payload any) string {
	var output strings.Builder
	output.Grow(len(event) + 256)
	output.WriteString("event: ")
	output.WriteString(event)
	output.WriteString("\ndata: ")
	if err := jsonx.Encode(&output, payload); err != nil {
		return "event: " + event +
			"\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"serialization failed\"}}\n\n"
	}
	output.WriteByte('\n')
	return output.String()
}
