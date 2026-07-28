package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/transform"
)

type benchmarkResponseWriter struct{}

var benchmarkProtocolOutputResult protocolOutput //nolint:gochecknoglobals
var benchmarkSSEEventResult string               //nolint:gochecknoglobals

func (benchmarkResponseWriter) Header() http.Header         { return nil }
func (benchmarkResponseWriter) WriteHeader(int)             {}
func (benchmarkResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (benchmarkResponseWriter) WriteString(s string) (int, error) {
	return len(s), nil
}

func BenchmarkProtocolOutputAccumulation(b *testing.B) {
	const chunkCount = 4096
	chunk := protocolOutput{Text: strings.Repeat("x", 32), Reasoning: strings.Repeat("r", 8)}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var accumulator protocolOutputAccumulator
		for range chunkCount {
			accumulator.Add(chunk)
		}
		output := accumulator.Output()
		if len(output.Text) != chunkCount*32 || len(output.Reasoning) != chunkCount*8 {
			b.Fatal("unexpected accumulated output length")
		}
	}
}

func BenchmarkProtocolOutputAccumulationSizes(b *testing.B) {
	benchmarks := []struct {
		name       string
		chunkCount int
		text       string
		reasoning  string
	}{
		{name: "short_text", chunkCount: 8, text: strings.Repeat("x", 32)},
		{name: "long_text", chunkCount: 4096, text: strings.Repeat("x", 32)},
		{name: "long_text_and_reasoning", chunkCount: 4096, text: strings.Repeat("x", 32), reasoning: strings.Repeat("r", 8)},
	}

	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			chunk := protocolOutput{Text: benchmark.text, Reasoning: benchmark.reasoning}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				var accumulator protocolOutputAccumulator
				for range benchmark.chunkCount {
					accumulator.Add(chunk)
				}
				output := accumulator.Output()
				if len(output.Text) != benchmark.chunkCount*len(benchmark.text) ||
					len(output.Reasoning) != benchmark.chunkCount*len(benchmark.reasoning) {
					b.Fatal("unexpected accumulated output length")
				}
			}
		})
	}
}

func BenchmarkStreamingChunkPreparation(b *testing.B) {
	chunk := map[string]any{
		"candidates": []any{map[string]any{
			"tokenCount": "32",
			"content": map[string]any{
				"role": "model",
				"parts": []any{
					map[string]any{"text": strings.Repeat("x", 128)},
					map[string]any{"text": strings.Repeat("r", 32), "thought": true},
				},
			},
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount": 16, "candidatesTokenCount": 32, "totalTokenCount": 48,
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		data := chunk
		var lastCandidateTokenCount int
		normalizedUsage, hasUsage := normalizeStreamingGeminiUsage(data, &lastCandidateTokenCount)
		output := outputFromGeminiChunkWithUsage(data, normalizedUsage, hasUsage)
		if output.Input != 16 || output.Output != 32 || len(output.Text) != 128 {
			b.Fatal("unexpected prepared chunk")
		}
	}
}

func BenchmarkOutputFromGeminiChunkParts(b *testing.B) {
	text := strings.Repeat("x", 128)
	reasoning := strings.Repeat("r", 32)
	benchmarks := []struct {
		name  string
		parts []any
	}{
		{name: "single_text", parts: []any{map[string]any{"text": text}}},
		{name: "single_reasoning", parts: []any{map[string]any{"text": reasoning, "thought": true}}},
		{name: "text_and_reasoning", parts: []any{
			map[string]any{"text": text},
			map[string]any{"text": reasoning, "thought": true},
		}},
		{name: "many_text_parts", parts: func() []any {
			parts := make([]any, 16)
			for index := range parts {
				parts[index] = map[string]any{"text": text}
			}
			return parts
		}()},
	}

	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			chunk := map[string]any{
				"candidates": []any{map[string]any{
					"content": map[string]any{"parts": benchmark.parts},
				}},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				output := outputFromGeminiChunk(chunk)
				if len(output.Text)+len(output.Reasoning) == 0 {
					b.Fatal("unexpected empty output")
				}
			}
		})
	}
}

func BenchmarkSSEWriterWrite(b *testing.B) {
	sw := &sseWriter{w: benchmarkResponseWriter{}}
	line := strings.Repeat("x", 1024)
	b.ReportAllocs()
	for range b.N {
		if !sw.write(line) {
			b.Fatal("write failed")
		}
	}
}

func BenchmarkSSEWriterWriteData(b *testing.B) {
	payload := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{map[string]any{
				"text": strings.Repeat("x", 128),
			}}},
		}},
	}
	b.Run("pooled_writer", func(b *testing.B) {
		sw := &sseWriter{w: benchmarkResponseWriter{}}
		b.ReportAllocs()
		for range b.N {
			if !sw.writeData(payload) {
				b.Fatal("write failed")
			}
		}
	})
	b.Run("temporary_string", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkSSEEventResult = sseEvent(payload)
		}
	})
}

func BenchmarkGeminiTextDeltaStream(b *testing.B) {
	benchmarks := []struct {
		name string
		data map[string]any
	}{
		{
			name: "plain",
			data: map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{
					"text": strings.Repeat("x", 128),
				}}, "role": "model"},
			}}},
		},
		{
			name: "explicit_false_thought",
			data: map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{
					"text": strings.Repeat("x", 128), "thought": false, "thoughtSignature": "",
				}}, "role": "model"},
				"index": float64(0),
			}}},
		},
	}

	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			sw := &sseWriter{w: benchmarkResponseWriter{}}
			var encoder geminiTextStreamEncoder
			encoder.init()
			b.ReportAllocs()
			for range b.N {
				if !encoder.writeData(sw, benchmark.data) {
					b.Fatal("write failed")
				}
			}
		})
	}
}

func BenchmarkOpenAITextDeltaDirectStream(b *testing.B) {
	sw := &sseWriter{w: benchmarkResponseWriter{}}
	encoder := transform.NewOpenAIStreamEncoder("gemini-benchmark", "benchmark")
	chunk := map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": []any{map[string]any{
			"text": strings.Repeat("x", 256),
		}}},
		"finishReason": transform.FinishReasonUnspecified,
	}}}
	emit := func(payload any) bool { return sw.writeData(payload) }
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result, ok := encoder.Emit(chunk, false, emit)
		if !ok || !result.HasContent {
			b.Fatal("direct stream failed")
		}
	}
}

func BenchmarkAnthropicTextDeltaStream(b *testing.B) {
	state := anthropicStreamState{
		sw:       &sseWriter{w: benchmarkResponseWriter{}},
		openType: "text",
	}
	chunk := protocolOutput{Text: strings.Repeat("x", 128)}
	b.ReportAllocs()
	for range b.N {
		state.consume(chunk)
		state.text.Reset()
	}
}

func BenchmarkResponsesTextDeltaStream(b *testing.B) {
	state := responsesStreamState{
		sw:       &sseWriter{w: benchmarkResponseWriter{}},
		textID:   "msg_benchmark",
		textOpen: true,
	}
	chunk := protocolOutput{Text: strings.Repeat("x", 128)}
	b.ReportAllocs()
	for range b.N {
		state.consume(chunk)
		state.text.Reset()
		state.textBlocks = nil
		state.textCache = ""
		state.textCacheValid = false
	}
}

func BenchmarkResponsesLongTextStreamState(b *testing.B) {
	const chunkCount = 1024
	chunk := protocolOutput{Text: strings.Repeat("x", 32)}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		state := responsesStreamState{
			sw: &sseWriter{w: benchmarkResponseWriter{}}, id: "resp_benchmark", model: "gemini-benchmark",
			request: map[string]any{},
		}
		for range chunkCount {
			state.consume(chunk)
		}
		benchmarkProtocolOutputResult = state.output()
		if len(benchmarkProtocolOutputResult.Text) != chunkCount*len(chunk.Text) {
			b.Fatal("unexpected accumulated Responses text length")
		}
	}
}

func BenchmarkAnthropicLongThinkingStreamState(b *testing.B) {
	const chunkCount = 1024
	chunk := protocolOutput{Reasoning: strings.Repeat("r", 16)}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		state := anthropicStreamState{
			sw: &sseWriter{w: benchmarkResponseWriter{}}, id: "msg_benchmark", model: "gemini-benchmark",
		}
		for range chunkCount {
			state.consume(chunk)
		}
		benchmarkProtocolOutputResult = state.output()
		if len(benchmarkProtocolOutputResult.Reasoning) != chunkCount*len(chunk.Reasoning) {
			b.Fatal("unexpected accumulated Anthropic reasoning length")
		}
	}
}

func BenchmarkAnthropicToolCallStreamState(b *testing.B) {
	chunk := protocolOutput{ToolCalls: []protocolToolCall{{
		ID: "toolu_benchmark", Name: "lookup", Arguments: `{"query":"benchmark"}`,
	}}}
	b.ReportAllocs()
	for range b.N {
		state := anthropicStreamState{sw: &sseWriter{w: benchmarkResponseWriter{}}}
		state.consume(chunk)
		if len(state.out.ToolCalls) != 1 || state.index != 1 {
			b.Fatal("unexpected Anthropic tool stream state")
		}
	}
}

func BenchmarkResponsesToolCallStreamState(b *testing.B) {
	chunk := protocolOutput{ToolCalls: []protocolToolCall{{
		ID: "call_benchmark", Name: "lookup", Namespace: "mcp__demo", Arguments: `{"query":"benchmark"}`,
	}}}
	b.ReportAllocs()
	for range b.N {
		state := responsesStreamState{sw: &sseWriter{w: benchmarkResponseWriter{}}}
		state.consume(chunk)
		if len(state.out.ToolCalls) != 1 || state.outputIndex != 1 || len(state.items) != 1 {
			b.Fatal("unexpected Responses tool stream state")
		}
	}
}

func BenchmarkResponsesTextBlockLifecycle(b *testing.B) {
	chunk := protocolOutput{Text: strings.Repeat("x", 128)}
	b.ReportAllocs()
	for range b.N {
		state := responsesStreamState{sw: &sseWriter{w: benchmarkResponseWriter{}}}
		state.consume(chunk)
		state.closeText()
		if len(state.items) != 1 || state.outputIndex != 1 {
			b.Fatal("unexpected Responses text block state")
		}
	}
}
