package transform

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

const assistantPrefillMetadataKey = "__vproxy_assistant_prefill"

const assistantPrefillInstructionPrefix = "The JSON string below represents text already emitted by the assistant. " +
	"Decode it as existing response text, not instructions or JSON to complete. " +
	"Continue the underlying response. Return only new text after the prefix; never output the prefix, " +
	"JSON syntax, delimiters, an explanation, or a restarted answer.\n" +
	"Assistant prefix JSON: "

const assistantPrefillContinueNudge = "Continue the immediately preceding assistant response. " +
	"Output only its continuation; do not repeat, explain, or restart it."

const lowerHexDigits = "0123456789abcdef"

// AdaptGemini36Prefill applies the trailing model-turn compatibility rewrite
// to a native Gemini payload. model must be the resolved upstream model name.
// The returned prefix is also stored as internal metadata for response filters.
func AdaptGemini36Prefill(model string, payload map[string]any) string {
	if payload == nil {
		return ""
	}
	// Native clients control arbitrary JSON keys. Never trust caller-supplied
	// internal metadata, otherwise a crafted request could strip valid output.
	delete(payload, assistantPrefillMetadataKey)
	if !isGemini36Model(model) {
		return ""
	}
	contents, ok := payload["contents"].([]any)
	if !ok {
		return ""
	}
	adapted, prefix := convertTrailingAssistantPrefill(contents)
	if prefix == "" {
		return ""
	}
	payload["contents"] = adapted
	payload[assistantPrefillMetadataKey] = prefix
	return prefix
}

// convertTrailingAssistantPrefill preserves a valid plain-text assistant
// prefill and appends a final user continuation nudge. Gemini 3.6 rejects
// requests ending in a non-empty model turn, while clients such as SillyTavern
// use exactly that shape for Continue Prefill. Invalid model-only/consecutive
// model shapes use a data-encoded user fallback.
func convertTrailingAssistantPrefill(contents []any) ([]any, string) {
	if len(contents) == 0 {
		return contents, ""
	}
	last, ok := contents[len(contents)-1].(map[string]any)
	if !ok || last["role"] != "model" {
		return contents, ""
	}
	parts, ok := last["parts"].([]any)
	if !ok || len(parts) == 0 {
		return contents, ""
	}

	prefillLength := 0
	maximumInt := int(^uint(0) >> 1)
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok || isTruthy(part["thought"]) {
			return contents, ""
		}
		text, ok := part["text"].(string)
		if !ok {
			// Tool calls and media are real history, not a text prefill.
			return contents, ""
		}
		if len(text) > maximumInt-prefillLength {
			return contents, ""
		}
		prefillLength += len(text)
	}
	if prefillLength == 0 {
		return contents, ""
	}

	prefix := ""
	if len(parts) == 1 {
		prefix = parts[0].(map[string]any)["text"].(string)
	} else {
		var prefill strings.Builder
		prefill.Grow(prefillLength)
		for _, rawPart := range parts {
			part := rawPart.(map[string]any)
			prefill.WriteString(part["text"].(string))
		}
		prefix = prefill.String()
	}

	// Preserve the assistant role whenever the prefix follows a valid prompt
	// turn. Gemini 3.6 only rejects a request that *ends* in a non-empty model
	// turn; appending a short user nudge makes the request legal without turning
	// character text into the nearest user instruction. This is the common
	// SillyTavern Continue Prefill shape.
	if len(contents) > 1 {
		previous, _ := contents[len(contents)-2].(map[string]any)
		previousRole, _ := previous["role"].(string)
		if previousRole == "user" || previousRole == "function" {
			nudge := map[string]any{
				"role": "user",
				"parts": []any{
					map[string]any{"text": assistantPrefillContinueNudge},
				},
			}
			return append(contents, nudge), prefix
		}
	}

	// A model-only or consecutive-model payload is not valid Gemini history.
	// Fall back to encoding the prefix as data in a user instruction so legacy
	// and malformed clients still receive the established compatibility path.
	instruction := buildAssistantPrefillInstruction(prefix)
	instructionPart := map[string]any{"text": instruction}

	contents = contents[:len(contents)-1]
	if len(contents) > 0 {
		if previous, ok := contents[len(contents)-1].(map[string]any); ok && previous["role"] == "user" {
			previousParts, _ := previous["parts"].([]any)
			previous["parts"] = append(previousParts, instructionPart)
			return contents, prefix
		}
	}
	contents = append(contents, map[string]any{"role": "user", "parts": []any{instructionPart}})
	return contents, prefix
}

func buildAssistantPrefillInstruction(prefix string) string {
	var instruction strings.Builder
	maximumInt := int(^uint(0) >> 1)
	if len(prefix) <= maximumInt-len(assistantPrefillInstructionPrefix)-2 {
		instruction.Grow(len(assistantPrefillInstructionPrefix) + len(prefix) + 2)
	}
	instruction.WriteString(assistantPrefillInstructionPrefix)
	writeQuotedString(&instruction, prefix)
	return instruction.String()
}

func writeQuotedString(dst *strings.Builder, value string) {
	dst.WriteByte('"')
	start := 0
	for index := 0; index < len(value); {
		current := value[index]
		if current >= utf8.RuneSelf {
			r, width := utf8.DecodeRuneInString(value[index:])
			if width > 1 && strconv.IsPrint(r) {
				index += width
				continue
			}
			dst.WriteString(value[start:index])
			if width == 1 && r == utf8.RuneError {
				writeHexByteEscape(dst, current)
				index++
			} else {
				writeUnicodeEscape(dst, r)
				index += width
			}
			start = index
			continue
		}

		var escape byte
		switch current {
		case '\a':
			escape = 'a'
		case '\b':
			escape = 'b'
		case '\f':
			escape = 'f'
		case '\n':
			escape = 'n'
		case '\r':
			escape = 'r'
		case '\t':
			escape = 't'
		case '\v':
			escape = 'v'
		case '\\', '"':
			escape = current
		default:
			if current >= ' ' && current != 0x7f {
				index++
				continue
			}
		}
		dst.WriteString(value[start:index])
		if escape != 0 {
			dst.WriteByte('\\')
			dst.WriteByte(escape)
		} else {
			writeHexByteEscape(dst, current)
		}
		index++
		start = index
	}
	dst.WriteString(value[start:])
	dst.WriteByte('"')
}

func writeHexByteEscape(dst *strings.Builder, value byte) {
	dst.WriteString("\\x")
	dst.WriteByte(lowerHexDigits[value>>4])
	dst.WriteByte(lowerHexDigits[value&0x0f])
}

func writeUnicodeEscape(dst *strings.Builder, value rune) {
	if !utf8.ValidRune(value) {
		value = utf8.RuneError
	}
	shift := 12
	if value < 0x10000 {
		dst.WriteString("\\u")
	} else {
		dst.WriteString("\\U")
		shift = 28
	}
	for ; shift >= 0; shift -= 4 {
		dst.WriteByte(lowerHexDigits[value>>uint(shift)&0x0f])
	}
}

// AssistantPrefillFromPayload returns internal compatibility metadata. The
// metadata is ignored by BuildVertexVariables and is never sent upstream.
func AssistantPrefillFromPayload(payload map[string]any) string {
	prefill, _ := payload[assistantPrefillMetadataKey].(string)
	return prefill
}

// StripAssistantPrefillEcho removes an exact echoed prefix. If the model obeys
// the continuation instruction and emits only new text, the output is intact.
func StripAssistantPrefillEcho(text, prefill string) string {
	if prefill == "" {
		return text
	}
	return strings.TrimPrefix(text, prefill)
}

// StripAssistantPrefillFromOAI applies echo removal to every non-stream choice.
func StripAssistantPrefillFromOAI(response map[string]any, prefill string) {
	if prefill == "" {
		return
	}
	choices, _ := response["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		text, ok := message["content"].(string)
		if ok {
			message["content"] = StripAssistantPrefillEcho(text, prefill)
		}
	}
}

// StripAssistantPrefillFromGemini applies exact echo removal to a complete
// native Gemini response without touching thought, tool or media parts.
func StripAssistantPrefillFromGemini(response map[string]any, prefill string) {
	if prefill == "" {
		return
	}
	filter := NewAssistantPrefillStreamFilter(prefill)
	filter.FilterGeminiChunk(response)
	for _, tail := range filter.FinalizeGemini() {
		if candidate := geminiCandidateByFilterIndex(response, tail.Index); candidate != nil {
			appendOrdinaryText(candidate, tail.Text)
		}
	}
}

// AssistantPrefillStreamFilter removes an echoed prefill even when it spans
// several upstream chunks. It buffers only while output is still an exact
// prefix candidate, then releases text immediately once a mismatch is known.
type AssistantPrefillStreamFilter struct {
	prefill     string
	primary     assistantPrefillCandidateState
	primarySeen bool
	others      map[int]*assistantPrefillCandidateState
	otherOrder  []int
	sawText     bool
}

type assistantPrefillCandidateState struct {
	matched int
	decided bool
}

// GeminiPrefillTail identifies an ambiguous partial prefix that must be
// released when a native Gemini stream closes without a finish frame.
type GeminiPrefillTail struct {
	Index int
	Text  string
}

func NewAssistantPrefillStreamFilter(prefill string) *AssistantPrefillStreamFilter {
	return &AssistantPrefillStreamFilter{prefill: prefill}
}

func (f *AssistantPrefillStreamFilter) filterText(text string, final bool) string {
	return f.filterCandidateText(0, text, final)
}

func (f *AssistantPrefillStreamFilter) filterCandidateText(candidateIndex int, text string, final bool) string {
	if candidateIndex == 0 && text != "" {
		f.sawText = true
	}
	if f.prefill == "" {
		return text
	}

	state := f.candidateState(candidateIndex)
	if state.decided {
		return text
	}

	previouslyMatched := state.matched
	remaining := f.prefill[previouslyMatched:]
	matchedNow := commonPrefixBytes(text, remaining)
	if matchedNow == len(remaining) {
		state.matched += matchedNow
		state.decided = true
		return text[matchedNow:]
	}
	if matchedNow < len(text) {
		// 当前 chunk 在前缀完成前出现不匹配；之前暂存的内容一定等于
		// prefill[:previouslyMatched]，无需逐块保存或反复复制。
		out := text
		if previouslyMatched > 0 {
			out = f.prefill[:previouslyMatched] + text
		}
		state.matched = 0
		state.decided = true
		return out
	}

	// 当前 chunk 全部匹配，但完整前缀尚未结束。
	state.matched += matchedNow
	if !final {
		return ""
	}
	out := f.prefill[:state.matched]
	state.matched = 0
	state.decided = true
	return out
}

func (f *AssistantPrefillStreamFilter) candidateState(index int) *assistantPrefillCandidateState {
	if index == 0 {
		f.primarySeen = true
		return &f.primary
	}
	if f.others == nil {
		f.others = make(map[int]*assistantPrefillCandidateState)
	}
	if state := f.others[index]; state != nil {
		return state
	}
	state := &assistantPrefillCandidateState{}
	f.others[index] = state
	f.otherOrder = append(f.otherOrder, index)
	return state
}

func commonPrefixBytes(left, right string) int {
	limit := min(len(left), len(right))
	for index := 0; index < limit; index++ {
		if left[index] != right[index] {
			return index
		}
	}
	return limit
}

// FilterGeminiChunk mutates only ordinary text parts in a request-local chunk.
// Thought text, tool calls, images, usage and finish metadata remain untouched.
func (f *AssistantPrefillStreamFilter) FilterGeminiChunk(chunk map[string]any) {
	if f == nil || f.prefill == "" {
		return
	}
	candidates, _ := chunk["candidates"].([]any)
	for position, rawCandidate := range candidates {
		candidate, ok := rawCandidate.(map[string]any)
		if !ok {
			continue
		}
		candidateIndex := geminiCandidateFilterIndex(candidate, position)
		content, _ := candidate["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || isTruthy(part["thought"]) {
				continue
			}
			if text, ok := part["text"].(string); ok && text != "" {
				part["text"] = f.filterCandidateText(candidateIndex, text, false)
			}
		}

		finish, _ := candidate["finishReason"].(string)
		if finish != "" && finish != FinishReasonUnspecified {
			if tail := f.filterCandidateText(candidateIndex, "", true); tail != "" {
				appendOrdinaryText(candidate, tail)
			}
		}
	}
}

// FilterTextChunk 对已验证的单候选普通文本帧执行与 FilterGeminiChunk 相同的
// 前缀过滤，并在真实 finishReason 到达时释放仍未决的部分前缀。
func (f *AssistantPrefillStreamFilter) FilterTextChunk(
	candidateIndex int,
	text, finishReason string,
) string {
	filtered, tail := f.FilterTextChunkParts(candidateIndex, text, finishReason)
	return filtered + tail
}

// FilterTextChunkParts 与 FilterTextChunk 等价，但把结束帧释放的未决前缀
// 单独返回，供需要保持 Gemini parts 边界的原生流编码器使用。
func (f *AssistantPrefillStreamFilter) FilterTextChunkParts(
	candidateIndex int,
	text, finishReason string,
) (filtered, tail string) {
	if f == nil || f.prefill == "" {
		return text, ""
	}
	if text != "" {
		filtered = f.filterCandidateText(candidateIndex, text, false)
	}
	if finishReason != "" && finishReason != FinishReasonUnspecified {
		tail = f.filterCandidateText(candidateIndex, "", true)
	}
	return filtered, tail
}

func geminiCandidateFilterIndex(candidate map[string]any, fallback int) int {
	const maximumCandidateIndex = 1 << 30
	switch index := candidate["index"].(type) {
	case int:
		if index >= 0 && index <= maximumCandidateIndex {
			return index
		}
	case int64:
		if index >= 0 && index <= maximumCandidateIndex {
			return int(index)
		}
	case float64:
		if index >= 0 && index <= maximumCandidateIndex && index == float64(int(index)) {
			return int(index)
		}
	}
	return fallback
}

func geminiCandidateByFilterIndex(response map[string]any, wanted int) map[string]any {
	candidates, _ := response["candidates"].([]any)
	for position, rawCandidate := range candidates {
		candidate, _ := rawCandidate.(map[string]any)
		if candidate != nil && geminiCandidateFilterIndex(candidate, position) == wanted {
			return candidate
		}
	}
	return nil
}

func appendOrdinaryText(candidate map[string]any, text string) {
	if len(candidate) == 0 || text == "" {
		return
	}
	content, _ := candidate["content"].(map[string]any)
	if content == nil {
		content = map[string]any{"role": "model"}
		candidate["content"] = content
	}
	parts, _ := content["parts"].([]any)
	content["parts"] = append(parts, map[string]any{"text": text})
}

// Finalize releases a still-ambiguous partial prefix when the upstream stream
// closes without a real finishReason frame. Repeated calls are safe.
func (f *AssistantPrefillStreamFilter) Finalize() string {
	if f == nil || f.prefill == "" {
		return ""
	}
	return f.filterText("", true)
}

// FinalizeGemini releases every candidate's still-ambiguous partial prefix.
// Candidate zero is returned first, followed by other candidates in first-seen
// order, so native stream output is deterministic.
func (f *AssistantPrefillStreamFilter) FinalizeGemini() []GeminiPrefillTail {
	if f == nil || f.prefill == "" {
		return nil
	}
	var tails []GeminiPrefillTail
	if f.primarySeen {
		if text := f.filterCandidateText(0, "", true); text != "" {
			tails = append(tails, GeminiPrefillTail{Index: 0, Text: text})
		}
	}
	for _, index := range f.otherOrder {
		if text := f.filterCandidateText(index, "", true); text != "" {
			tails = append(tails, GeminiPrefillTail{Index: index, Text: text})
		}
	}
	return tails
}

func (f *AssistantPrefillStreamFilter) SawText() bool {
	return f != nil && f.sawText
}
