package vertex

import "testing"

var benchmarkChunkCollectorResult *ParseResult //nolint:gochecknoglobals

func TestChunkCollectorAggregatesPartsAndMetadata(t *testing.T) {
	collector := newChunkCollector()
	collector.Add(map[string]any{
		"candidates": []any{map[string]any{
			"index": 2,
			"content": map[string]any{"parts": []any{
				map[string]any{"text": "hel"},
			}},
			"safetyRatings":    []any{"first-safety"},
			"citationMetadata": map[string]any{"source": "first"},
			"tokenCount":       1,
			"avgLogprobs":      -0.5,
		}},
		"promptFeedback": map[string]any{"blockReason": "FIRST"},
		"usageMetadata":  map[string]any{"totalTokenCount": 1},
		"createTime":     "first-time",
		"modelVersion":   "first-model",
		"responseId":     "first-id",
	})
	collector.Add(map[string]any{
		"candidates": []any{map[string]any{
			"index":         3,
			"finishReason":  "STOP",
			"finishMessage": "done",
			"content": map[string]any{"parts": []any{
				map[string]any{"text": "lo"},
			}},
			"safetyRatings":     []any{"last-safety"},
			"groundingMetadata": map[string]any{"source": "last"},
			"tokenCount":        2,
			"logprobsResult":    map[string]any{"chosen": true},
		}},
		"promptFeedback": map[string]any{"blockReason": "SECOND"},
		"usageMetadata":  map[string]any{"totalTokenCount": 2},
		"createTime":     "last-time",
		"modelVersion":   "last-model",
		"responseId":     "last-id",
	})

	result := collector.Result()
	if collector.Len() != 2 || len(result.Parts) != 1 || result.Parts[0]["text"] != "hello" {
		t.Fatalf("parts/count mismatch: count=%d parts=%#v", collector.Len(), result.Parts)
	}
	if result.CandidateIndex != 3 || result.FinishReason != "STOP" || result.FinishMessage != "done" {
		t.Fatalf("candidate metadata mismatch: %#v", result)
	}
	if result.TokenCount != 2 || result.AvgLogprobs != -0.5 || result.LogprobsResult == nil {
		t.Fatalf("candidate details mismatch: %#v", result)
	}
	if result.PromptFeedback["blockReason"] != "FIRST" || result.UsageMetadata["totalTokenCount"] != 2 {
		t.Fatalf("feedback/usage precedence mismatch: %#v", result)
	}
	if result.CreateTime != "last-time" || result.ModelVersion != "last-model" || result.ResponseID != "last-id" {
		t.Fatalf("top-level metadata mismatch: %#v", result)
	}
}

func BenchmarkChunkCollection(b *testing.B) {
	chunks := make([]map[string]any, 4096)
	for index := range chunks {
		chunks[index] = map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{
					map[string]any{"text": "0123456789abcdef"},
				}},
			}},
		}
	}

	b.Run("incremental", func(b *testing.B) {
		for range b.N {
			collector := newChunkCollector()
			for _, chunk := range chunks {
				collector.Add(chunk)
			}
			benchmarkChunkCollectorResult = collector.Result()
		}
	})
	b.Run("retained_outer_chunks", func(b *testing.B) {
		for range b.N {
			retained := make([]map[string]any, 0, len(chunks))
			retained = append(retained, chunks...)
			benchmarkChunkCollectorResult = collectChunksToParseResult(retained)
		}
	})
}

func BenchmarkChunkCollectionSizes(b *testing.B) {
	for _, benchmark := range []struct {
		name       string
		chunkCount int
		thought    bool
	}{
		{name: "short_text", chunkCount: 8},
		{name: "long_text", chunkCount: 4096},
		{name: "alternating_text_and_thought", chunkCount: 4096, thought: true},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			chunks := make([]map[string]any, benchmark.chunkCount)
			for index := range chunks {
				part := map[string]any{"text": "0123456789abcdef"}
				if benchmark.thought && index%2 == 0 {
					part["thought"] = true
				}
				chunks[index] = map[string]any{
					"candidates": []any{map[string]any{
						"content": map[string]any{"parts": []any{part}},
					}},
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				collector := newChunkCollector()
				for _, chunk := range chunks {
					collector.Add(chunk)
				}
				result := collector.Result()
				if len(result.Parts) == 0 {
					b.Fatal("unexpected empty result")
				}
				benchmarkChunkCollectorResult = result
			}
		})
	}
}
