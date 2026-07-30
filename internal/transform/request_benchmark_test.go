package transform

import (
	"strconv"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/jsonx"
)

var benchmarkConvertedModel string           //nolint:gochecknoglobals
var benchmarkConvertedPayload map[string]any //nolint:gochecknoglobals
var benchmarkEncodedPayloadSize int          //nolint:gochecknoglobals

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
	contentParts := make([]any, 16)
	for index := range contentParts {
		contentParts[index] = map[string]any{"type": "text", "text": text}
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
			name: "sixteen_content_parts",
			body: map[string]any{
				"model": "gemini-3.1-flash",
				"messages": []any{map[string]any{
					"role": "user", "content": contentParts,
				}},
			},
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

func BenchmarkTextHistoryRequestPipeline(b *testing.B) {
	messages := make([]any, 16)
	for index := range messages {
		role := "user"
		if index%2 != 0 {
			role = "assistant"
		}
		messages[index] = map[string]any{
			"role": role, "content": strings.Repeat("x", 128),
		}
	}
	benchmarkTextHistoryPipeline(b, messages)
}

func BenchmarkProtocolTextHistoryRequestPipeline(b *testing.B) {
	messages := make([]any, 0, 17)
	messages = append(messages, map[string]any{"role": "system", "content": "system prompt"})
	for index := range 16 {
		role := "user"
		partType := "input_text"
		if index%2 != 0 {
			role = "assistant"
			partType = "output_text"
		}
		messages = append(messages, map[string]any{
			"role": role,
			"content": []any{map[string]any{
				"type": partType, "text": strings.Repeat("x", 128),
			}},
		})
	}
	benchmarkTextHistoryPipeline(b, messages)
}

func benchmarkTextHistoryPipeline(b *testing.B, messages []any) {
	cfg := config.StaticProvider(config.DefaultConfig())
	body := map[string]any{"model": "gemini-3.1-flash", "messages": messages}
	converter := DefaultRequestConverter()

	for _, benchmark := range []struct {
		name    string
		convert func() (string, map[string]any, error)
	}{
		{name: "map_compatibility", convert: func() (string, map[string]any, error) {
			return ConvertChatRequest(body, cfg)
		}},
		{name: "default_compact", convert: func() (string, map[string]any, error) {
			return converter.Convert(body, cfg)
		}},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				model, payload, err := benchmark.convert()
				if err != nil {
					b.Fatal(err)
				}
				vars := BuildVertexVariables(model, payload, cfg)
				if err := jsonx.MarshalView(vars, func(encoded []byte) {
					benchmarkEncodedPayloadSize = len(encoded)
				}); err != nil {
					b.Fatal(err)
				}
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

func BenchmarkConvertBuildAndMarshalLargeToolSchema(b *testing.B) {
	cfg := config.StaticProvider(config.DefaultConfig())
	body := largeToolSchemaBenchmarkBody("question")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		model, payload, err := ConvertChatRequest(body, cfg)
		if err != nil {
			b.Fatal(err)
		}
		vars := BuildVertexVariables(model, payload, cfg)
		if err := jsonx.MarshalView(vars, func(encoded []byte) {
			benchmarkEncodedPayloadSize = len(encoded)
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConvertChatRequestToolSchemaSize(b *testing.B) {
	cfg := config.StaticProvider(config.DefaultConfig())
	for _, propertyCount := range []int{1, 16, 64, 256} {
		b.Run(strconv.Itoa(propertyCount), func(b *testing.B) {
			body := toolSchemaBenchmarkBody("question", propertyCount)
			b.ReportAllocs()
			for range b.N {
				model, payload, err := ConvertChatRequest(body, cfg)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkConvertedModel = model
				benchmarkConvertedPayload = payload
			}
		})
	}
}

func largeToolSchemaBenchmarkBody(message string) map[string]any {
	return toolSchemaBenchmarkBody(message, 64)
}

func toolSchemaBenchmarkBody(message string, propertyCount int) map[string]any {
	properties := make(map[string]any, propertyCount)
	for index := range propertyCount {
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
