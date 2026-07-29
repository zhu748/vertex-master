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

// convertTrailingAssistantPrefill rewrites a plain-text assistant prefill into
// a final user continuation instruction. Gemini 3.6 rejects requests ending in
// a non-empty model turn, while clients such as SillyTavern use exactly that
// shape for Continue Prefill.
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

	var prefill strings.Builder
	prefill.Grow(prefillLength)
	for _, rawPart := range parts {
		part := rawPart.(map[string]any)
		text := part["text"].(string)
		prefill.WriteString(text)
	}

	prefix := prefill.String()
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
	if tail := filter.Finalize(); tail != "" {
		appendOrdinaryText(firstCandidate(response), tail)
	}
}

// AssistantPrefillStreamFilter removes an echoed prefill even when it spans
// several upstream chunks. It buffers only while output is still an exact
// prefix candidate, then releases text immediately once a mismatch is known.
type AssistantPrefillStreamFilter struct {
	prefill string
	matched int
	decided bool
	sawText bool
}

func NewAssistantPrefillStreamFilter(prefill string) *AssistantPrefillStreamFilter {
	return &AssistantPrefillStreamFilter{prefill: prefill}
}

func (f *AssistantPrefillStreamFilter) filterText(text string, final bool) string {
	if text != "" {
		f.sawText = true
	}
	if f.decided || f.prefill == "" {
		return text
	}

	previouslyMatched := f.matched
	remaining := f.prefill[previouslyMatched:]
	matchedNow := commonPrefixBytes(text, remaining)
	if matchedNow == len(remaining) {
		f.matched += matchedNow
		f.decided = true
		return text[matchedNow:]
	}
	if matchedNow < len(text) {
		// 当前 chunk 在前缀完成前出现不匹配；之前暂存的内容一定等于
		// prefill[:previouslyMatched]，无需逐块保存或反复复制。
		out := text
		if previouslyMatched > 0 {
			out = f.prefill[:previouslyMatched] + text
		}
		f.matched = 0
		f.decided = true
		return out
	}

	// 当前 chunk 全部匹配，但完整前缀尚未结束。
	f.matched += matchedNow
	if !final {
		return ""
	}
	out := f.prefill[:f.matched]
	f.matched = 0
	f.decided = true
	return out
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
	candidate := firstCandidate(chunk)
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok || isTruthy(part["thought"]) {
			continue
		}
		if text, ok := part["text"].(string); ok && text != "" {
			part["text"] = f.filterText(text, false)
		}
	}

	finish, _ := candidate["finishReason"].(string)
	if finish != "" && finish != FinishReasonUnspecified {
		if tail := f.filterText("", true); tail != "" {
			appendOrdinaryText(candidate, tail)
		}
	}
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

func (f *AssistantPrefillStreamFilter) SawText() bool {
	return f != nil && f.sawText
}
