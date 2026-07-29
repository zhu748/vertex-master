package api

import "testing"

func TestSummarizePromptCorrelatesWithoutExposingText(t *testing.T) {
	payload := map[string]any{
		"systemInstruction": map[string]any{"parts": []any{
			map[string]any{"text": "private system"},
		}},
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}},
			map[string]any{"role": "model", "parts": []any{
				map[string]any{"text": "Alice:"},
				map[string]any{"functionCall": map[string]any{
					"name": "lookup", "args": map[string]any{"query": "first"},
				}},
			}},
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "continue"}}},
		},
	}

	first := summarizePrompt(payload)
	second := summarizePrompt(payload)
	if first.Fingerprint == "" || first.Fingerprint != second.Fingerprint {
		t.Fatalf("同一进程中的相同提示必须具有稳定摘要: %#v %#v", first, second)
	}
	if first.Turns != 3 || first.UserTurns != 2 || first.ModelTurns != 1 {
		t.Fatalf("轮次摘要错误: %#v", first)
	}
	if first.SystemBytes != len("private system") ||
		first.TextBytes != len("private systemhelloAlice:continue") {
		t.Fatalf("文本长度摘要错误: %#v", first)
	}

	payload["contents"].([]any)[0].(map[string]any)["parts"] =
		[]any{map[string]any{"text": "changed"}}
	changed := summarizePrompt(payload)
	if changed.Fingerprint == first.Fingerprint {
		t.Fatal("提示文本变化后摘要必须变化")
	}

	payload["contents"].([]any)[0].(map[string]any)["parts"] =
		[]any{map[string]any{"text": "hello"}}
	functionCall := payload["contents"].([]any)[1].(map[string]any)["parts"].([]any)[1].(map[string]any)["functionCall"].(map[string]any)
	functionCall["args"] = map[string]any{"query": "second"}
	nonTextChanged := summarizePrompt(payload)
	if nonTextChanged.Fingerprint == first.Fingerprint {
		t.Fatal("工具参数变化后摘要必须变化")
	}
}
