package transform

import (
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

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
	last := contents[len(contents)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("Gemini 3.6 请求最后一轮必须转换为 user，got %v", last["role"])
	}
	parts := last["parts"].([]any)
	instruction := parts[len(parts)-1].(map[string]any)["text"].(string)
	if !strings.Contains(instruction, "only the new continuation") ||
		!strings.Contains(instruction, `Alice: \"I`) {
		t.Fatalf("续写指令未保留预填充: %q", instruction)
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
