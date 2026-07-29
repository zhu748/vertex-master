package api

import (
	"encoding/json"
	"testing"
)

func TestReusableResponsesMessageOnlyAcceptsCanonicalReadOnlyShape(t *testing.T) {
	content := []any{map[string]any{"type": "input_text", "text": "hello"}}
	canonical := map[string]any{"type": "message", "role": "user", "content": content}
	converted, err := responseContentToChat(content)
	if err != nil || !reusableResponsesMessage(canonical, converted) {
		t.Fatalf("canonical message was not reusable: converted=%#v err=%v", converted, err)
	}

	withExtraField := map[string]any{
		"type": "message", "role": "assistant", "content": "hello",
		"tool_calls": []any{map[string]any{"name": "must-not-leak"}},
	}
	if reusableResponsesMessage(withExtraField, "hello") {
		t.Fatal("message with extra Chat fields must be copied and sanitized")
	}
	if reusableResponsesMessage(map[string]any{"type": "message", "content": "hello"}, "hello") {
		t.Fatal("message requiring a default role must not be reused")
	}
}

func TestProtocolArrayTextConversionPreservesSeparatorsAndInput(t *testing.T) {
	parts := []any{
		map[string]any{"type": "text", "text": "first"},
		map[string]any{"type": "text", "text": ""},
		map[string]any{"type": "tool_use", "name": "ignored"},
		map[string]any{"type": "text", "text": "second"},
	}
	before, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	if got := responseInstructions(parts); got != "first\nsecond" {
		t.Fatalf("Responses text=%q", got)
	}
	if got := anthropicText(parts); got != "first\n\nsecond" {
		t.Fatalf("Anthropic text=%q", got)
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
	message := messages[0].(map[string]any)
	if message["content"] != "beforeafter" {
		t.Fatalf("assistant text=%#v", message["content"])
	}
	toolCalls := message["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("assistant tools=%#v", toolCalls)
	}
	function := toolCalls[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "lookup" || function["arguments"] != `{"q":"x"}` {
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
	last := appended[1].(map[string]any)
	if last["role"] != "assistant" || last["content"] != "answer" {
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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if converted, err := responsesToChatRequest(test.body); err == nil {
				t.Fatalf("缺损上下文必须返回错误，got %#v", converted)
			}
		})
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
	assistant := messages[len(messages)-1].(map[string]any)
	if assistant["content"] != "visible" {
		t.Fatalf("可见 assistant 上下文应保留: %#v", assistant)
	}
}
