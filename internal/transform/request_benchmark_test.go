package transform

import (
	"strconv"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

var benchmarkConvertedModel string           //nolint:gochecknoglobals
var benchmarkConvertedPayload map[string]any //nolint:gochecknoglobals

func BenchmarkConvertChatRequest(b *testing.B) {
	cfg := config.StaticProvider(config.DefaultConfig())
	text := strings.Repeat("x", 128)
	messages := make([]any, 16)
	for index := range messages {
		role := "user"
		if index%2 != 0 {
			role = "assistant"
		}
		messages[index] = map[string]any{"role": role, "content": text}
	}
	for _, test := range []struct {
		name string
		body map[string]any
	}{
		{
			name: "plain_text",
			body: map[string]any{
				"model":    "gemini-3.1-flash",
				"messages": []any{map[string]any{"role": "user", "content": text}},
			},
		},
		{
			name: "sixteen_messages",
			body: map[string]any{"model": "gemini-3.1-flash", "messages": messages},
		},
		{
			name: "large_tool_schema",
			body: largeToolSchemaBenchmarkBody(text),
		},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				model, payload, err := ConvertChatRequest(test.body, cfg)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkConvertedModel = model
				benchmarkConvertedPayload = payload
			}
		})
	}
}

func BenchmarkBuildVertexVariablesLargeToolSchema(b *testing.B) {
	cfg := config.StaticProvider(config.DefaultConfig())
	body := largeToolSchemaBenchmarkBody("question")
	_, payload, err := ConvertChatRequest(body, cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkConvertedPayload = BuildVertexVariables("gemini-3.1-flash", payload, cfg)
	}
}

func BenchmarkConvertAndBuildVertexVariablesLargeToolSchema(b *testing.B) {
	cfg := config.StaticProvider(config.DefaultConfig())
	body := largeToolSchemaBenchmarkBody("question")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		model, payload, err := ConvertChatRequest(body, cfg)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkConvertedPayload = BuildVertexVariables(model, payload, cfg)
	}
}

func largeToolSchemaBenchmarkBody(message string) map[string]any {
	properties := make(map[string]any, 64)
	for index := range 64 {
		properties["field_"+strconv.Itoa(index)] = map[string]any{
			"type": "string", "description": strings.Repeat("x", 128),
		}
	}
	body := map[string]any{
		"model":    "gemini-3.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": message}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "lookup", "parameters": map[string]any{
					"type": "object", "properties": properties,
				},
			},
		}},
	}
	return body
}
