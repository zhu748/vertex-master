package api

import (
	"strings"
	"testing"
)

var benchmarkGeminiResponseText string //nolint:gochecknoglobals

func TestGeminiResponseTextCombinesCandidatesAndSkipsThoughts(t *testing.T) {
	response := map[string]any{"candidates": []any{
		map[string]any{"content": map[string]any{"parts": []any{
			map[string]any{"text": "first"},
			map[string]any{"text": "hidden", "thought": true},
		}}},
		map[string]any{"content": map[string]any{"parts": []any{
			map[string]any{"text": " second"},
			map[string]any{"text": 123},
		}}},
	}}
	if got := geminiResponseText(response); got != "first second" {
		t.Fatalf("geminiResponseText()=%q, want %q", got, "first second")
	}
}

func BenchmarkGeminiResponseTextOneMiBSplit(b *testing.B) {
	text := strings.Repeat("A", 64<<10)
	parts := make([]any, 16)
	for index := range parts {
		parts[index] = map[string]any{"text": text}
	}
	response := map[string]any{"candidates": []any{
		map[string]any{"content": map[string]any{"parts": parts}},
	}}
	b.ReportAllocs()
	b.SetBytes(1 << 20)
	for range b.N {
		benchmarkGeminiResponseText = geminiResponseText(response)
		if len(benchmarkGeminiResponseText) != 1<<20 {
			b.Fatalf("response text length=%d", len(benchmarkGeminiResponseText))
		}
	}
}
