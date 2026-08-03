package vertex

import (
	"encoding/json"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/transform"
)

func TestValueFitsBudget(t *testing.T) {
	remaining := 128
	if !valueFitsBudget(map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "hello"}},
	}, &remaining) {
		t.Fatal("small JSON-like value should fit budget")
	}

	remaining = 16
	if valueFitsBudget(map[string]any{"text": "this value is larger than the budget"}, &remaining) {
		t.Fatal("large string should exceed budget")
	}
}

func TestValueFitsBudgetRejectsExcessiveDepth(t *testing.T) {
	var value any = "leaf"
	for range valueBudgetMaxDepth + 2 {
		value = []any{value}
	}
	remaining := 1 << 20
	if valueFitsBudget(value, &remaining) {
		t.Fatal("excessively nested value should be rejected")
	}
}

func TestCompactContentsMatchMapBudgetAndTokenCacheKey(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	tests := []struct {
		name string
		body map[string]any
	}{
		{
			name: "text",
			body: map[string]any{
				"model": "gemini-3.1-flash",
				"messages": []any{
					map[string]any{"role": "user", "content": "question"},
					map[string]any{"role": "assistant", "content": "answer"},
				},
			},
		},
		{
			name: "tool_history",
			body: map[string]any{
				"model": "gemini-3.1-flash",
				"messages": []any{
					map[string]any{"role": "user", "content": "question"},
					map[string]any{
						"role": "assistant", "content": "",
						"tool_calls": []any{&transform.CanonicalOAIToolCall{
							ID:   "call_1",
							Type: "function",
							Function: transform.CanonicalOAIFunctionCallData{
								Name: "lookup",
								Arguments: map[string]any{
									"query": "value",
									"limit": float64(2),
								},
							},
						}},
					},
					map[string]any{
						"role": "tool", "tool_call_id": "call_1",
						"content": map[string]any{"result": []any{"one", "two"}},
					},
				},
			},
		},
		{
			name: "tool_history_without_ids",
			body: map[string]any{
				"model": "gemini-3.1-flash",
				"messages": []any{
					map[string]any{"role": "user", "content": "question"},
					map[string]any{
						"role": "assistant", "content": "",
						"tool_calls": []any{
							&transform.CanonicalOAIToolCall{
								Type: "function",
								Function: transform.CanonicalOAIFunctionCallData{
									Name: "first", Arguments: map[string]any{},
								},
							},
							&transform.CanonicalOAIToolCall{
								Type: "function",
								Function: transform.CanonicalOAIFunctionCallData{
									Name: "second", Arguments: map[string]any{"value": true},
								},
							},
						},
					},
					map[string]any{"role": "tool", "content": map[string]any{"result": "one"}},
					map[string]any{"role": "tool", "content": map[string]any{"result": "two"}},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapModel, mapPayload, err := transform.ConvertChatRequest(test.body, cfg)
			if err != nil {
				t.Fatal(err)
			}
			compactModel, compactPayload, err := transform.DefaultRequestConverter().Convert(test.body, cfg)
			if err != nil {
				t.Fatal(err)
			}

			mapRemaining := 1 << 20
			compactRemaining := mapRemaining
			if !valueFitsBudget(mapPayload, &mapRemaining) ||
				!valueFitsBudget(compactPayload, &compactRemaining) {
				t.Fatal("equivalent compact and map payloads should fit the same budget")
			}
			if compactRemaining != mapRemaining {
				t.Fatalf("remaining budget: compact=%d map=%d", compactRemaining, mapRemaining)
			}

			mapContents := mapPayload["contents"].([]any)
			compactContents := compactPayload["contents"].([]any)
			mapKey, mapOK := makeTokenCountCacheKey("gemini-3.1-flash", mapContents)
			compactKey, compactOK := makeTokenCountCacheKey("gemini-3.1-flash", compactContents)
			if !mapOK || !compactOK {
				t.Fatalf("cache key availability: compact=%v map=%v", compactOK, mapOK)
			}
			if compactKey != mapKey {
				t.Fatalf("compact and map token cache keys differ:\ncompact=%x\nmap=%x", compactKey, mapKey)
			}

			mapCountPayload := buildCountTokensPayload(
				"gemini-3.1-flash",
				mapContents,
				"token",
				cfg,
			)
			compactCountPayload := buildCountTokensPayload(
				"gemini-3.1-flash",
				compactContents,
				"token",
				cfg,
			)
			mapWire, err := json.Marshal(mapCountPayload)
			if err != nil {
				t.Fatal(err)
			}
			compactWire, err := json.Marshal(compactCountPayload)
			if err != nil {
				t.Fatal(err)
			}
			if string(compactWire) != string(mapWire) {
				t.Fatalf("compact CountTokens payload changed wire JSON:\ncompact=%s\nmap=%s",
					compactWire,
					mapWire,
				)
			}

			mapVariables := transform.BuildVertexVariables(mapModel, mapPayload, cfg)
			compactVariables := transform.BuildVertexVariables(
				compactModel,
				compactPayload,
				cfg,
			)
			mapRemaining = 1 << 20
			compactRemaining = mapRemaining
			if !valueFitsBudget(mapVariables, &mapRemaining) ||
				!valueFitsBudget(compactVariables, &compactRemaining) {
				t.Fatal("equivalent outbound variables should fit the same budget")
			}
			if compactRemaining != mapRemaining {
				t.Fatalf(
					"outbound remaining budget: compact=%d map=%d",
					compactRemaining,
					mapRemaining,
				)
			}
			mapContents = mapVariables["contents"].([]any)
			compactContents = compactVariables["contents"].([]any)
			mapKey, mapOK = makeTokenCountCacheKey("gemini-3.1-flash", mapContents)
			compactKey, compactOK = makeTokenCountCacheKey(
				"gemini-3.1-flash",
				compactContents,
			)
			if !mapOK || !compactOK || compactKey != mapKey {
				t.Fatalf(
					"outbound cache key mismatch: compact_ok=%v map_ok=%v compact=%x map=%x",
					compactOK,
					mapOK,
					compactKey,
					mapKey,
				)
			}
			mapWire, err = json.Marshal(mapVariables)
			if err != nil {
				t.Fatal(err)
			}
			compactWire, err = json.Marshal(compactVariables)
			if err != nil {
				t.Fatal(err)
			}
			if string(compactWire) != string(mapWire) {
				t.Fatalf(
					"compact outbound variables changed wire JSON:\ncompact=%s\nmap=%s",
					compactWire,
					mapWire,
				)
			}
		})
	}
}
