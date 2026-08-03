package api

import (
	"encoding/json"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/transform"
)

func TestReusableResponsesMessageOnlyAcceptsCanonicalReadOnlyShape(t *testing.T) {
	content := []any{map[string]any{"type": "input_text", "text": "hello"}}
	canonical := map[string]any{"type": "message", "role": "user", "content": content}
	if !reusableResponsesMessage(canonical) {
		t.Fatalf("canonical message was not reusable: %#v", canonical)
	}

	withExtraField := map[string]any{
		"type": "message", "role": "assistant", "content": "hello",
		"tool_calls": []any{map[string]any{"name": "must-not-leak"}},
	}
	if reusableResponsesMessage(withExtraField) {
		t.Fatal("message with extra Chat fields must be copied and sanitized")
	}
	if reusableResponsesMessage(map[string]any{"type": "message", "content": "hello"}) {
		t.Fatal("message requiring a default role must not be reused")
	}
}

func TestAnthropicMessagePassThroughOnlyAcceptsCanonicalReadOnlyShape(t *testing.T) {
	userTextBlocks := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "hello", "cache_control": map[string]any{"type": "ephemeral"}},
		},
	}
	if !anthropicMessageCanPassThrough(userTextBlocks, "user") {
		t.Fatalf("canonical user text message was not reusable: %#v", userTextBlocks)
	}
	assistantText := map[string]any{"role": "assistant", "content": "hello"}
	if !anthropicMessageCanPassThrough(assistantText, "assistant") {
		t.Fatalf("canonical assistant string message was not reusable: %#v", assistantText)
	}
	assistantTextBlock := map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": "hello"},
		},
	}
	if !anthropicMessageCanPassThrough(assistantTextBlock, "assistant") {
		t.Fatalf("single assistant text block was not reusable: %#v", assistantTextBlock)
	}

	for name, message := range map[string]map[string]any{
		"multiple assistant blocks require flattening": {
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "text", "text": "again"},
			},
		},
		"extra fields must be sanitized": {
			"role": "user", "content": "hello", "tool_call_id": "must-not-leak",
		},
		"empty blocks do not produce a message": {
			"role": "user", "content": []any{},
		},
		"empty assistant text requires flattening": {
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": ""},
			},
		},
		"invalid text block requires validation": {
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": float64(1)},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if anthropicMessageCanPassThrough(message, stringValue(message["role"])) {
				t.Fatalf("non-canonical message was reused: %#v", message)
			}
		})
	}
}

func TestAnthropicSingleAssistantTextBlockPassesThroughFullConversion(t *testing.T) {
	content := []any{map[string]any{
		"type": "text", "text": "assistant prefill",
		"cache_control": map[string]any{"type": "ephemeral"},
	}}
	body := map[string]any{"messages": []any{map[string]any{
		"role": "assistant", "content": content,
	}}}
	before, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := anthropicToChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	message := chat["messages"].([]any)[0].(map[string]any)
	passedContent := message["content"].([]any)
	if &passedContent[0] != &content[0] {
		t.Fatal("single assistant text block was copied instead of reused")
	}

	chat["model"] = "gemini-test"
	_, payload, err := transform.DefaultRequestConverter().Convert(
		chat,
		config.StaticProvider(config.DefaultConfig()),
	)
	if err != nil {
		t.Fatal(err)
	}
	contents := payload["contents"].([]any)
	compact, ok := contents[0].(interface {
		CanonicalTextContent() (role, text string, ok bool)
	})
	if !ok {
		t.Fatalf("converted assistant content type=%T", contents[0])
	}
	role, text, valid := compact.CanonicalTextContent()
	if !valid || role != "model" || text != "assistant prefill" {
		t.Fatalf("converted assistant content=(%q, %q, %v)", role, text, valid)
	}
	after, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("conversion mutated input:\n before: %s\n after:  %s", before, after)
	}
}

func TestProtocolArrayTextConversionPreservesSeparatorsAndInput(t *testing.T) {
	parts := []any{
		map[string]any{"type": "text", "text": "first"},
		map[string]any{"type": "text", "text": ""},
		map[string]any{"type": "text", "text": "second"},
	}
	before, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	responseParts := []any{
		map[string]any{"type": "text", "text": "first"},
		map[string]any{"type": "text", "text": ""},
		map[string]any{"type": "text", "text": "second"},
	}
	got, err := responseInstructions(responseParts)
	if err != nil || got != "first\nsecond" {
		t.Fatalf("Responses text=%q err=%v", got, err)
	}
	if got, ok := anthropicTextBlocks(parts); !ok || got != "first\n\nsecond" {
		t.Fatalf("Anthropic text=%q ok=%v", got, ok)
	}
	after, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("input parts were mutated:\n before: %s\n after:  %s", before, after)
	}
}

func TestAnthropicAssistantConversionPreservesTextAndToolOrder(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "before"},
		map[string]any{
			"type": "tool_use", "id": "call_1", "name": "lookup", "input": map[string]any{"q": "x"},
		},
		map[string]any{"type": "text", "text": "after"},
	}
	before, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := appendAnthropicMessageToChat(nil, "assistant", content)
	if err != nil || len(messages) != 1 {
		t.Fatalf("conversion failed: messages=%#v err=%v", messages, err)
	}
	message, ok := messages[0].(transform.CanonicalChatMessage)
	if !ok {
		t.Fatalf("assistant message has unexpected type %T", messages[0])
	}
	if message.Content != "beforeafter" {
		t.Fatalf("assistant text=%#v", message.Content)
	}
	toolCalls := message.ToolCalls
	if len(toolCalls) != 1 {
		t.Fatalf("assistant tools=%#v", toolCalls)
	}
	call, ok := toolCalls[0].(transform.CanonicalOAIToolCall)
	if !ok || call.Function.Name != "lookup" ||
		call.Function.Arguments.(map[string]any)["q"] != "x" {
		t.Fatalf("assistant tools=%#v", toolCalls)
	}
	after, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("assistant input was mutated:\n before: %s\n after:  %s", before, after)
	}
}

func TestAppendAnthropicMessagePreservesExistingMessages(t *testing.T) {
	prefix := map[string]any{"role": "system", "content": "prefix"}
	messages := make([]any, 1, 2)
	messages[0] = prefix
	appended, err := appendAnthropicMessageToChat(messages, "assistant", "answer")
	if err != nil || len(appended) != 2 {
		t.Fatalf("append failed: messages=%#v err=%v", appended, err)
	}
	if appended[0].(map[string]any)["content"] != prefix["content"] {
		t.Fatalf("existing message changed: %#v", appended[0])
	}
	last, ok := appended[1].(transform.CanonicalChatMessage)
	if !ok || last.Role != "assistant" || last.Content != "answer" {
		t.Fatalf("appended message=%#v", last)
	}
}

func TestResponsesConversionRejectsItemsThatWouldLoseContext(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
	}{
		{
			name: "non-object input item",
			body: map[string]any{"input": []any{
				map[string]any{"type": "message", "role": "user", "content": "keep"},
				"drop-me",
			}},
		},
		{
			name: "unknown input item",
			body: map[string]any{"input": []any{
				map[string]any{"type": "message", "role": "user", "content": "keep"},
				map[string]any{"type": "future_item", "content": "drop-me"},
			}},
		},
		{
			name: "instructions is not a string or array",
			body: map[string]any{
				"instructions": map[string]any{"type": "text", "text": "drop-me"},
				"input":        "keep",
			},
		},
		{
			name: "unknown instructions block",
			body: map[string]any{
				"instructions": []any{
					map[string]any{"type": "input_text", "text": "keep"},
					map[string]any{"type": "future_instruction", "text": "drop-me"},
				},
				"input": "keep",
			},
		},
		{
			name: "function call without identity",
			body: map[string]any{"input": []any{
				map[string]any{"type": "message", "role": "user", "content": "keep"},
				map[string]any{"type": "function_call", "arguments": `{}`},
			}},
		},
		{
			name: "function output without call id",
			body: map[string]any{"input": []any{
				map[string]any{"type": "message", "role": "user", "content": "keep"},
				map[string]any{"type": "function_call_output", "output": "drop-me"},
			}},
		},
		{
			name: "non-object content block",
			body: map[string]any{"input": []any{
				map[string]any{
					"type": "message", "role": "user",
					"content": []any{
						map[string]any{"type": "input_text", "text": "keep"},
						"drop-me",
					},
				},
			}},
		},
		{
			name: "non-object tool",
			body: map[string]any{
				"input": "keep",
				"tools": []any{
					map[string]any{"type": "function", "name": "valid"},
					"drop-me",
				},
			},
		},
		{
			name: "tools is not an array",
			body: map[string]any{
				"input": "keep",
				"tools": map[string]any{"type": "function", "name": "drop-me"},
			},
		},
		{
			name: "non-object namespace tool",
			body: map[string]any{
				"input": "keep",
				"tools": []any{map[string]any{
					"type": "namespace", "name": "demo",
					"tools": []any{
						map[string]any{"type": "function", "name": "valid"},
						"drop-me",
					},
				}},
			},
		},
		{
			name: "namespace tools is not an array",
			body: map[string]any{
				"input": "keep",
				"tools": []any{map[string]any{
					"type": "namespace", "name": "demo",
					"tools": map[string]any{"type": "function", "name": "drop-me"},
				}},
			},
		},
		{
			name: "namespace child without name",
			body: map[string]any{
				"input": "keep",
				"tools": []any{map[string]any{
					"type": "namespace", "name": "demo",
					"tools": []any{
						map[string]any{"type": "function", "parameters": map[string]any{}},
					},
				}},
			},
		},
		{
			name: "unsupported namespace child type",
			body: map[string]any{
				"input": "keep",
				"tools": []any{map[string]any{
					"type": "namespace", "name": "demo",
					"tools": []any{
						map[string]any{"type": "future_tool", "name": "drop-me"},
					},
				}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if converted, err := responsesToChatRequest(test.body); err == nil {
				t.Fatalf("缺损上下文必须返回错误，got %#v", converted)
			}
		})
	}
}

func TestAnthropicToolResultPreservesMixedContent(t *testing.T) {
	converted, err := anthropicToChatRequest(map[string]any{
		"max_tokens": float64(64),
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "call_1",
				"content": []any{
					map[string]any{"type": "text", "text": "visible"},
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type": "base64", "media_type": "image/png", "data": "aGVsbG8=",
						},
					},
				},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := converted["messages"].([]any)
	tool, ok := messages[0].(transform.CanonicalChatMessage)
	if !ok {
		t.Fatalf("复合 tool_result 消息类型异常: %T", messages[0])
	}
	blocks, ok := tool.Content.([]any)
	if !ok {
		t.Fatalf("复合 tool_result 应保留已解码数组，got %#v", tool.Content)
	}
	if len(blocks) != 2 {
		t.Fatalf("复合 tool_result 丢失内容块: %#v", blocks)
	}
	image := blocks[1].(map[string]any)
	source := image["source"].(map[string]any)
	if source["data"] != "aGVsbG8=" {
		t.Fatalf("复合 tool_result 图片数据未保留: %#v", blocks)
	}
}

func TestAnthropicConversionRejectsBlocksThatWouldLoseContext(t *testing.T) {
	validUser := map[string]any{"role": "user", "content": "keep"}
	tests := []struct {
		name string
		body map[string]any
	}{
		{
			name: "non-object message",
			body: map[string]any{
				"max_tokens": float64(64),
				"messages":   []any{validUser, "drop-me"},
			},
		},
		{
			name: "unknown assistant block",
			body: map[string]any{
				"max_tokens": float64(64),
				"messages": []any{
					validUser,
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "text", "text": "keep"},
						map[string]any{"type": "future_block", "text": "drop-me"},
					}},
				},
			},
		},
		{
			name: "unknown user block",
			body: map[string]any{
				"max_tokens": float64(64),
				"messages": []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "keep"},
					map[string]any{"type": "document", "source": map[string]any{}},
				}}},
			},
		},
		{
			name: "unknown image source",
			body: map[string]any{
				"max_tokens": float64(64),
				"messages": []any{map[string]any{"role": "user", "content": []any{
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type": "future_source", "data": "drop-me",
						},
					},
				}}},
			},
		},
		{
			name: "tool result without id",
			body: map[string]any{
				"max_tokens": float64(64),
				"messages": []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "content": "drop-me"},
				}}},
			},
		},
		{
			name: "non-object tool",
			body: map[string]any{
				"max_tokens": float64(64),
				"messages":   []any{validUser},
				"tools":      []any{map[string]any{"name": "valid"}, "drop-me"},
			},
		},
		{
			name: "unknown tool choice",
			body: map[string]any{
				"max_tokens": float64(64),
				"messages":   []any{validUser},
				"tool_choice": map[string]any{
					"type": "future_choice",
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if converted, err := anthropicToChatRequest(test.body); err == nil {
				t.Fatalf("缺损上下文必须返回错误，got %#v", converted)
			}
		})
	}
}

func TestAnthropicConversionIntentionallyIgnoresThinkingHistory(t *testing.T) {
	converted, err := anthropicToChatRequest(map[string]any{
		"max_tokens": float64(64),
		"messages": []any{
			map[string]any{"role": "user", "content": "continue"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "private", "signature": "sig"},
				map[string]any{"type": "text", "text": "visible"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := converted["messages"].([]any)
	assistant, ok := messages[len(messages)-1].(transform.CanonicalChatMessage)
	if !ok || assistant.Content != "visible" {
		t.Fatalf("可见 assistant 上下文应保留: %#v", assistant)
	}
}
