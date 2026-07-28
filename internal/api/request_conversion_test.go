package api

import (
	"encoding/json"
	"testing"
)

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
