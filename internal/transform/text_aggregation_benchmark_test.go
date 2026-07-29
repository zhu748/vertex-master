package transform

import (
	"strings"
	"testing"
)

var benchmarkAggregatedText string //nolint:gochecknoglobals
var benchmarkAdaptedContents []any //nolint:gochecknoglobals

func BenchmarkConvertTrailingAssistantPrefillOneMiBSplit(b *testing.B) {
	text := strings.Repeat("A", 64<<10)
	parts := make([]any, 16)
	for index := range parts {
		parts[index] = map[string]any{"text": text}
	}
	modelMessage := map[string]any{"role": "model", "parts": parts}
	b.ReportAllocs()
	b.SetBytes(1 << 20)
	for range b.N {
		contents := make([]any, 1)
		contents[0] = modelMessage
		benchmarkAdaptedContents, benchmarkAggregatedText = convertTrailingAssistantPrefill(contents)
		if len(benchmarkAggregatedText) != 1<<20 || len(benchmarkAdaptedContents) != 1 {
			b.Fatal("unexpected prefill conversion result")
		}
	}
}

func BenchmarkExtractTextFromInstructionOneMiBSplit(b *testing.B) {
	text := strings.Repeat("A", 64<<10)
	parts := make([]any, 16)
	for index := range parts {
		parts[index] = map[string]any{"text": text}
	}
	instruction := map[string]any{"parts": parts}
	b.ReportAllocs()
	b.SetBytes(1 << 20)
	for range b.N {
		benchmarkAggregatedText = extractTextFromInstruction(instruction)
		if len(benchmarkAggregatedText) != 1<<20 {
			b.Fatalf("instruction text length=%d", len(benchmarkAggregatedText))
		}
	}
}
