package transform

import (
	"strconv"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

func TestBuildAssistantPrefillInstructionMatchesStrconvQuote(t *testing.T) {
	prefixes := []string{
		"plain ASCII",
		"quote: \" slash: \\",
		"controls:\a\b\f\n\r\t\v\x00\x1f\x7f",
		"中文与 emoji 🦊",
		"unicode escapes: \u0085\u200b\u2028\U0001f600",
		string([]byte{'a', 0xff, 'b'}),
	}
	allBytes := make([]byte, 256)
	for index := range allBytes {
		allBytes[index] = byte(index)
	}
	prefixes = append(prefixes, string(allBytes))

	for _, prefix := range prefixes {
		want := assistantPrefillInstructionPrefix + strconv.Quote(prefix)
		if got := buildAssistantPrefillInstruction(prefix); got != want {
			t.Fatalf("buildAssistantPrefillInstruction(%q)=%q, want %q", prefix, got, want)
		}
	}
}

func TestGemini36ConvertsTrailingAssistantPrefill(t *testing.T) {
	body := map[string]any{
		"model": "gemini-3.6-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "Continue the scene"},
			map[string]any{"role": "assistant", "content": "Alice: \"I"},
		},
	}
	model, payload, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if got := AssistantPrefillFromPayload(payload); got != "Alice: \"I" {
		t.Fatalf("预填充元数据错误: %q", got)
	}
	contents := payload["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("酒馆预填充应保留原轮次并追加 nudge，got %#v", contents)
	}
	preserved := contents[1].(map[string]any)
	if preserved["role"] != "model" {
		t.Fatalf("预填充必须保留 assistant/model 角色，got %v", preserved["role"])
	}
	preservedParts := preserved["parts"].([]any)
	if got := preservedParts[0].(map[string]any)["text"]; got != `Alice: "I` {
		t.Fatalf("预填充文本被改变: %q", got)
	}
	last := contents[len(contents)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("Gemini 3.6 请求最后一轮必须是 user nudge，got %v", last["role"])
	}
	parts := last["parts"].([]any)
	nudge := parts[len(parts)-1].(map[string]any)["text"].(string)
	if nudge != assistantPrefillContinueNudge || strings.Contains(nudge, "Alice") {
		t.Fatalf("续写 nudge 不应复制或解释预填充: %q", nudge)
	}

	vars := BuildVertexVariables(model, payload, config.StaticProvider(config.DefaultConfig()))
	if _, leaked := vars[assistantPrefillMetadataKey]; leaked {
		t.Fatal("内部预填充元数据不得发送到上游")
	}
	upstreamContents := vars["contents"].([]any)
	if upstreamContents[len(upstreamContents)-1].(map[string]any)["role"] == "model" {
		t.Fatal("上游 Gemini 3.6 请求不得以 model 轮次结束")
	}
}

func TestGemini36ConvertsTrailingAssistantTextArrayPrefill(t *testing.T) {
	body := map[string]any{
		"model": "gemini-3.6-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "Continue"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "AB"},
				map[string]any{"type": "output_text", "text": "C"},
			}},
		},
	}
	_, payload, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if got := AssistantPrefillFromPayload(payload); got != "ABC" {
		t.Fatalf("assistant 文本数组未合并为精确预填充: %q", got)
	}
	contents := payload["contents"].([]any)
	if got := contents[len(contents)-1].(map[string]any)["role"]; got != "user" {
		t.Fatalf("转换后最后一轮应为 user，got %v", got)
	}
}

func TestAdaptGemini36NativePrefill(t *testing.T) {
	payload := map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "Continue"}}},
			map[string]any{"role": "model", "parts": []any{map[string]any{"text": "ABC"}}},
		},
		assistantPrefillMetadataKey: "untrusted",
	}
	if got := AdaptGemini36Prefill("gemini-3.6-flash", payload); got != "ABC" {
		t.Fatalf("原生 Gemini 预填充适配错误: %q", got)
	}
	if got := AssistantPrefillFromPayload(payload); got != "ABC" {
		t.Fatalf("客户端伪造的内部元数据未被替换: %q", got)
	}
	contents := payload["contents"].([]any)
	if got := contents[len(contents)-1].(map[string]any)["role"]; got != "user" {
		t.Fatalf("原生 Gemini 3.6 请求最后一轮必须是 user，got %v", got)
	}
	vars := BuildVertexVariables(
		"gemini-3.6-flash",
		payload,
		config.StaticProvider(config.DefaultConfig()),
	)
	if _, leaked := vars[assistantPrefillMetadataKey]; leaked {
		t.Fatal("原生 Gemini 内部元数据不得发送上游")
	}
}

func TestAdaptGemini36ModelOnlyPrefillUsesSafeFallback(t *testing.T) {
	payload := map[string]any{
		"contents": []any{
			map[string]any{"role": "model", "parts": []any{map[string]any{"text": "ABC"}}},
		},
	}
	if got := AdaptGemini36Prefill("gemini-3.6-flash", payload); got != "ABC" {
		t.Fatalf("原生 Gemini model-only 预填充适配错误: %q", got)
	}
	contents := payload["contents"].([]any)
	if len(contents) != 1 || contents[0].(map[string]any)["role"] != "user" {
		t.Fatalf("model-only 请求必须使用 user 数据指令回退: %#v", contents)
	}
	text := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `Assistant prefix JSON: "ABC"`) {
		t.Fatalf("回退指令未安全保留前缀: %q", text)
	}
}

func TestAdaptGemini36NativePrefillRejectsMultimodalAndClearsMetadata(t *testing.T) {
	payload := map[string]any{
		"contents": []any{
			map[string]any{"role": "model", "parts": []any{
				map[string]any{"text": "ABC"},
				map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "AA=="}},
			}},
		},
		assistantPrefillMetadataKey: "untrusted",
	}
	if got := AdaptGemini36Prefill("gemini-3.6-flash", payload); got != "" {
		t.Fatalf("多模态 model 历史不能被当作纯文本预填充: %q", got)
	}
	if got := AssistantPrefillFromPayload(payload); got != "" {
		t.Fatalf("无适配时必须清除客户端伪造元数据: %q", got)
	}
}

func TestOlderGeminiKeepsTrailingAssistantPrefill(t *testing.T) {
	body := map[string]any{
		"model": "gemini-3.5-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "Continue"},
			map[string]any{"role": "assistant", "content": "Alice:"},
		},
	}
	_, payload, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if got := AssistantPrefillFromPayload(payload); got != "" {
		t.Fatalf("旧模型不应启用 3.6 兼容改写: %q", got)
	}
	contents := payload["contents"].([]any)
	if contents[len(contents)-1].(map[string]any)["role"] != "model" {
		t.Fatal("旧模型的预填充行为应保持不变")
	}
}

func TestAssistantPrefillEchoRemovalForNonStream(t *testing.T) {
	response := map[string]any{"choices": []any{
		map[string]any{"message": map[string]any{"content": "Alice: hello"}},
		map[string]any{"message": map[string]any{"content": "new text"}},
	}}
	StripAssistantPrefillFromOAI(response, "Alice:")
	choices := response["choices"].([]any)
	if got := choices[0].(map[string]any)["message"].(map[string]any)["content"]; got != " hello" {
		t.Fatalf("应移除模型重复输出的预填充，got %q", got)
	}
	if got := choices[1].(map[string]any)["message"].(map[string]any)["content"]; got != "new text" {
		t.Fatalf("未重复预填充的输出不应改变，got %q", got)
	}
}

func TestAssistantPrefillEchoRemovalForNativeGemini(t *testing.T) {
	response := prefillTestChunk("ABCDEF", "STOP")
	StripAssistantPrefillFromGemini(response, "ABC")
	if got := prefillTestText(response); got != "DEF" {
		t.Fatalf("原生 Gemini 完整响应应移除重复前缀，got %q", got)
	}

	mismatch := prefillTestChunk("XYZ", "STOP")
	StripAssistantPrefillFromGemini(mismatch, "ABC")
	if got := prefillTestText(mismatch); got != "XYZ" {
		t.Fatalf("原生 Gemini 非重复输出不得改变，got %q", got)
	}
}

func TestAssistantPrefillStreamFilterAcrossChunks(t *testing.T) {
	filter := NewAssistantPrefillStreamFilter("Alice:")
	first := prefillTestChunk("Ali", FinishReasonUnspecified)
	filter.FilterGeminiChunk(first)
	if got := prefillTestText(first); got != "" {
		t.Fatalf("前缀尚未判定时应暂存，got %q", got)
	}

	second := prefillTestChunk("ce: hello", "STOP")
	filter.FilterGeminiChunk(second)
	if got := prefillTestText(second); got != " hello" {
		t.Fatalf("跨块重复前缀应被移除，got %q", got)
	}
	if !filter.SawText() {
		t.Fatal("过滤器应记录上游确实产生过文本")
	}
}

func TestAssistantPrefillStreamFilterPreservesMismatchAndPartialFinish(t *testing.T) {
	noEcho := NewAssistantPrefillStreamFilter("Alice:")
	chunk := prefillTestChunk("Hello", "STOP")
	noEcho.FilterGeminiChunk(chunk)
	if got := prefillTestText(chunk); got != "Hello" {
		t.Fatalf("非重复文本必须原样输出，got %q", got)
	}

	partial := NewAssistantPrefillStreamFilter("Alice:")
	partialChunk := prefillTestChunk("Ali", "STOP")
	partial.FilterGeminiChunk(partialChunk)
	if got := prefillTestText(partialChunk); got != "Ali" {
		t.Fatalf("流结束时不完整匹配必须释放，got %q", got)
	}
}

func TestAssistantPrefillStreamFilterPreservesPartialThenMidChunkMismatch(t *testing.T) {
	filter := NewAssistantPrefillStreamFilter("Alice:")
	first := prefillTestChunk("Al", FinishReasonUnspecified)
	filter.FilterGeminiChunk(first)
	if got := prefillTestText(first); got != "" {
		t.Fatalf("partial prefix should remain buffered, got %q", got)
	}

	second := prefillTestChunk("icX and more", "STOP")
	filter.FilterGeminiChunk(second)
	if got := prefillTestText(second); got != "AlicX and more" {
		t.Fatalf("mid-chunk mismatch lost buffered bytes, got %q", got)
	}
}

func TestAssistantPrefillStreamFilterFinishOnlyChunk(t *testing.T) {
	filter := NewAssistantPrefillStreamFilter("Alice:")
	first := prefillTestChunk("Ali", FinishReasonUnspecified)
	filter.FilterGeminiChunk(first)

	finishOnly := map[string]any{"candidates": []any{map[string]any{
		"finishReason": "STOP",
	}}}
	filter.FilterGeminiChunk(finishOnly)
	if got := prefillTestText(finishOnly); got != "Ali" {
		t.Fatalf("无 content 的结束帧必须安全释放部分匹配，got %q", got)
	}
}

func TestAssistantPrefillStreamFilterFinalizeReleasesPartial(t *testing.T) {
	filter := NewAssistantPrefillStreamFilter("Alice:")
	chunk := prefillTestChunk("Ali", FinishReasonUnspecified)
	filter.FilterGeminiChunk(chunk)
	if got := prefillTestText(chunk); got != "" {
		t.Fatalf("流未结束时应暂存部分匹配，got %q", got)
	}
	if got := filter.Finalize(); got != "Ali" {
		t.Fatalf("连接结束时必须释放部分匹配，got %q", got)
	}
	if got := filter.Finalize(); got != "" {
		t.Fatalf("重复结束不得重复输出，got %q", got)
	}
}

func BenchmarkAssistantPrefillStreamFilterSingleByteChunks(b *testing.B) {
	prefix := strings.Repeat("x", 32<<10)
	b.SetBytes(int64(len(prefix)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		filter := NewAssistantPrefillStreamFilter(prefix)
		for index := range prefix {
			if got := filter.filterText(prefix[index:index+1], false); got != "" {
				b.Fatal("matching prefix should remain buffered")
			}
		}
	}
}

func prefillTestChunk(text, finish string) map[string]any {
	return map[string]any{"candidates": []any{map[string]any{
		"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}},
		"finishReason": finish,
	}}}
}

func prefillTestText(chunk map[string]any) string {
	parts := candidateParts(firstCandidate(chunk))
	text, _, _ := ExtractParts(parts, true)
	return text
}
