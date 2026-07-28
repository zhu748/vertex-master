package vertex

import "testing"

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
