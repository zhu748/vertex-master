package vertex

import "testing"

func TestPickBestResultKeepsSelectedProxyURI(t *testing.T) {
	response := func(text string) map[string]any {
		return map[string]any{
			"candidates": []any{map[string]any{
				"finishReason": "OTHER",
				"content": map[string]any{
					"parts": []any{map[string]any{"text": text}},
				},
			}},
		}
	}
	results := []candidateResult{
		{proxyURI: "http://8.8.8.8:8080", resp: response("short")},
		{proxyURI: "socks5://1.1.1.1:1080", resp: response("a much longer fallback response")},
	}

	selected, err := pickBestResult(results)
	if err != nil {
		t.Fatal(err)
	}
	if selected.proxyURI != "socks5://1.1.1.1:1080" {
		t.Fatalf("selected proxy=%q, want longest fallback proxy", selected.proxyURI)
	}
}

func TestPromptFeedbackBlockReason(t *testing.T) {
	// 这些 helper 函数仍然保留，但生产路径已不再据此触发语义重试 ——
	// 匿名 Gemini 上游经常在正常响应里附带 BLOCKED_REASON_UNSPECIFIED，
	// 之前的语义重试会误判为拦截并提前 abort 流。这里仅验证 helper 本身。
	resp := map[string]any{
		"promptFeedback": map[string]any{"blockReason": "BLOCKED_REASON_UNSPECIFIED"},
	}
	if got := promptFeedbackBlockReason(resp); got != "BLOCKED_REASON_UNSPECIFIED" {
		t.Fatalf("block reason=%q", got)
	}
	if !isUnspecifiedBlockReason(promptFeedbackBlockReason(resp)) {
		t.Fatal("BLOCKED_REASON_UNSPECIFIED should be classified as unspecified")
	}
	if !isUnspecifiedBlockReason("BLOCK_REASON_UNSPECIFIED") {
		t.Fatal("official BLOCK_REASON_UNSPECIFIED spelling should also be classified as unspecified")
	}
	if isUnspecifiedBlockReason("SAFETY") {
		t.Fatal("specific safety reasons must not be classified as unspecified")
	}
}
