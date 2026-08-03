package api

import (
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/transform"
)

var benchmarkRequestConversionResult any  //nolint:gochecknoglobals
var benchmarkRequestConversionText string //nolint:gochecknoglobals

func BenchmarkProtocolArrayTextConversion(b *testing.B) {
	text := strings.Repeat("x", 128)
	for _, benchmark := range []struct {
		name  string
		parts []any
	}{
		{name: "single", parts: []any{map[string]any{"type": "text", "text": text}}},
		{name: "many", parts: func() []any {
			parts := make([]any, 16)
			for index := range parts {
				parts[index] = map[string]any{"type": "text", "text": text}
			}
			return parts
		}()},
	} {
		b.Run("responses_"+benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				benchmarkRequestConversionText, _ = responseInstructions(benchmark.parts)
			}
		})
		b.Run("anthropic_"+benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				benchmarkRequestConversionText, _ = anthropicTextBlocks(benchmark.parts)
			}
		})
	}
}

func BenchmarkAnthropicRequestConversion(b *testing.B) {
	text := strings.Repeat("x", 128)
	messages := make([]any, 16)
	for index := range messages {
		role := "user"
		if index%2 != 0 {
			role = "assistant"
		}
		messages[index] = map[string]any{
			"role": role,
			"content": []any{map[string]any{
				"type": "text", "text": text,
			}},
		}
	}
	body := map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": float64(1024),
		"system":     []any{map[string]any{"type": "text", "text": "system"}},
		"messages":   messages,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		converted, err := anthropicToChatRequest(body)
		if err != nil || len(converted["messages"].([]any)) != 17 {
			b.Fatal("unexpected Anthropic request conversion")
		}
		benchmarkRequestConversionResult = converted
	}
}

func BenchmarkAnthropicToolHistoryRequestPipeline(b *testing.B) {
	body := anthropicToolHistoryRequestBenchmarkBody()
	cfg := config.StaticProvider(config.DefaultConfig())
	converter := transform.DefaultRequestConverter()
	const modelName = "gemini-3.1-flash"
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		converted, err := anthropicToChatRequest(body)
		if err != nil {
			b.Fatal(err)
		}
		if _, err = applyClaudePromptPolicy(
			converted,
			cfg,
			"claude-sonnet-4-5",
			modelName,
		); err != nil {
			b.Fatal(err)
		}
		converted["model"] = modelName
		model, payload, err := converter.Convert(converted, cfg)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkRequestConversionResult = transform.BuildVertexVariables(model, payload, cfg)
	}
}

func anthropicToolHistoryRequestBenchmarkBody() map[string]any {
	messages := make([]any, 0, 24)
	for index := range 8 {
		callID := "call_" + string(rune('a'+index))
		messages = append(messages,
			map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "text", "text": "question",
				}},
			},
			map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "tool_use", "id": callID, "name": "lookup",
					"input": map[string]any{"q": index},
				}},
			},
			map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": callID,
					"content": map[string]any{"value": index},
				}},
			},
		)
	}
	return map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": float64(1024),
		"system":     "Be concise.",
		"messages":   messages,
		"tools": []any{map[string]any{
			"name": "lookup", "description": "Lookup a value",
			"input_schema": map[string]any{"type": "object"},
		}},
	}
}

func BenchmarkResponsesRequestConversion(b *testing.B) {
	benchmarkResponsesRequestConversion(b, false)
}

func BenchmarkResponsesStringToolRequestConversion(b *testing.B) {
	benchmarkResponsesRequestConversion(b, true)
}

func BenchmarkResponsesToolHistoryRequestPipeline(b *testing.B) {
	benchmarkResponsesToolHistoryRequestPipeline(b, "gemini-3.1-flash")
}

func BenchmarkResponsesToolHistoryRequestPipelineGemini36(b *testing.B) {
	benchmarkResponsesToolHistoryRequestPipeline(b, "gemini-3.6-flash")
}

func benchmarkResponsesToolHistoryRequestPipeline(b *testing.B, modelName string) {
	body := responsesRequestBenchmarkBody(false)
	cfg := config.StaticProvider(config.DefaultConfig())
	converter := transform.DefaultRequestConverter()
	modelValue := any(modelName)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		converted, err := responsesToChatRequest(body)
		if err != nil {
			b.Fatal(err)
		}
		converted["model"] = modelValue
		model, payload, err := converter.Convert(converted, cfg)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkRequestConversionResult = transform.BuildVertexVariables(model, payload, cfg)
	}
}

func BenchmarkResponsesTextHistoryRequestConversion(b *testing.B) {
	input := make([]any, 32)
	for index := range input {
		role := "user"
		if index%2 != 0 {
			role = "assistant"
		}
		input[index] = map[string]any{
			"type": "message", "role": role,
			"content": []any{map[string]any{"type": "input_text", "text": "message"}},
		}
	}
	body := map[string]any{"input": input}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		converted, err := responsesToChatRequest(body)
		if err != nil || len(converted["messages"].([]any)) != len(input) {
			b.Fatal("unexpected Responses text history conversion")
		}
		benchmarkRequestConversionResult = converted
	}
}

func benchmarkResponsesRequestConversion(b *testing.B, stringToolValues bool) {
	body := responsesRequestBenchmarkBody(stringToolValues)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		converted, err := responsesToChatRequest(body)
		if err != nil || len(converted["messages"].([]any)) != 25 {
			b.Fatal("unexpected Responses request conversion")
		}
		benchmarkRequestConversionResult = converted
	}
}

func responsesRequestBenchmarkBody(stringToolValues bool) map[string]any {
	input := make([]any, 0, 32)
	for index := range 8 {
		arguments := any(map[string]any{"q": index})
		output := any(map[string]any{"value": index})
		if stringToolValues {
			arguments = `{"q":1}`
			output = `{"value":1}`
		}
		input = append(input,
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "question"}},
			},
			map[string]any{
				"type": "function_call", "call_id": "call_" + string(rune('a'+index)),
				"name": "lookup", "arguments": arguments,
			},
			map[string]any{"type": "reasoning", "summary": []any{}},
			map[string]any{
				"type": "function_call_output", "call_id": "call_" + string(rune('a'+index)),
				"output": output,
			},
		)
	}
	return map[string]any{
		"model":        "gpt-5.2-codex",
		"instructions": "Be concise.",
		"input":        input,
		"tools": []any{
			map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "namespace", "name": "mcp__demo", "tools": []any{
				map[string]any{"name": "search", "parameters": map[string]any{"type": "object"}},
			}},
		},
	}
}

func BenchmarkAnthropicAssistantMessageConversion(b *testing.B) {
	text := strings.Repeat("x", 128)
	for _, benchmark := range []struct {
		name    string
		content []any
	}{
		{name: "single_text", content: []any{map[string]any{"type": "text", "text": text}}},
		{name: "many_text_and_tool", content: func() []any {
			content := make([]any, 0, 17)
			for index := range 16 {
				content = append(content, map[string]any{"type": "text", "text": text})
				if index == 7 {
					content = append(content, map[string]any{
						"type": "tool_use", "id": "call_1", "name": "lookup", "input": map[string]any{"q": "x"},
					})
				}
			}
			return content
		}()},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				var inline [1]any
				messages, err := appendAnthropicMessageToChat(inline[:0], "assistant", benchmark.content)
				if err != nil || len(messages) != 1 {
					b.Fatal("unexpected assistant conversion result")
				}
				benchmarkRequestConversionResult = messages[0]
			}
		})
	}
}
