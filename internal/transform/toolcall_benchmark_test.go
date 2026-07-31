package transform

import "testing"

var benchmarkExtractedOAIToolCall oaiToolCall //nolint:gochecknoglobals

func BenchmarkExtractCanonicalOAIToolCalls(b *testing.B) {
	for _, benchmark := range []struct {
		name string
		make func(index int) any
	}{
		{name: "canonical_value", make: func(index int) any {
			return CanonicalOAIToolCall{
				ID:   "call_benchmark",
				Type: "function",
				Function: CanonicalOAIFunctionCallData{
					Name: "lookup", Arguments: map[string]any{"index": float64(index)},
				},
			}
		}},
		{name: "canonical_pointer", make: func(index int) any {
			return &CanonicalOAIToolCall{
				ID:   "call_benchmark",
				Type: "function",
				Function: CanonicalOAIFunctionCallData{
					Name: "lookup", Arguments: map[string]any{"index": float64(index)},
				},
			}
		}},
		{name: "generic_map", make: func(index int) any {
			return map[string]any{
				"id": "call_benchmark",
				"function": map[string]any{
					"name": "lookup", "arguments": map[string]any{"index": float64(index)},
				},
			}
		}},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			toolCalls := make([]any, 16)
			for index := range toolCalls {
				toolCalls[index] = benchmark.make(index)
			}
			b.ReportAllocs()
			for range b.N {
				for _, rawToolCall := range toolCalls {
					parsed, ok := extractOAIToolCall(rawToolCall)
					if !ok {
						b.Fatal("tool call was not parsed")
					}
					benchmarkExtractedOAIToolCall = parsed
				}
			}
		})
	}
}
