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
