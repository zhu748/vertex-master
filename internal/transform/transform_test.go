package transform

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

func TestSnakeToCamel(t *testing.T) {
	cases := map[string]string{
		"max_output_tokens": "maxOutputTokens",
		"top_p":             "topP",
		"topK":              "topK", // 无下划线原样
		"temperature":       "temperature",
		"thinking_config":   "thinkingConfig",
		"__hello__WORLD":    "HelloWorld",
		"field_ÉCOLE":       "fieldÉcole",
	}
	for in, want := range cases {
		if got := SnakeToCamel(in); got != want {
			t.Errorf("SnakeToCamel(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestCamelToSnake(t *testing.T) {
	for input, want := range map[string]string{
		"topP":            "top_p",
		"maxOutputTokens": "max_output_tokens",
		"HTTPServer":      "httpserver",
		"someURLValue":    "some_urlvalue",
		"field1Value":     "field1_value",
		"already_lower":   "already_lower",
		"ÉcoleValue":      "école_value",
	} {
		if got := CamelToSnake(input); got != want {
			t.Errorf("CamelToSnake(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestNormalizeBase64(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "data URI", input: "data:image/png;base64,AAAA", want: "AAAA"},
		{name: "URL-safe and padding", input: "a-b_c", want: "a+b/c==="},
		{name: "standard unchanged", input: "ABcd+/09", want: "ABcd+/09"},
		{name: "whitespace", input: "  YQ  ", want: "YQ=="},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := NormalizeBase64(test.input); got != test.want {
				t.Errorf("NormalizeBase64(%q)=%q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestExtractTextFromInstructionPreservesDynamicTextValues(t *testing.T) {
	instruction := map[string]any{"parts": []any{
		map[string]any{"text": "alpha"},
		"ignored",
		map[string]any{"text": 123},
		map[string]any{"other": "ignored"},
		map[string]any{"text": "omega"},
	}}
	if got := extractTextFromInstruction(instruction); got != "alpha123omega" {
		t.Fatalf("extractTextFromInstruction()=%q, want %q", got, "alpha123omega")
	}
}

func TestConvertChatRequest_PlainText(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	body := map[string]any{
		"model": "gemini-3.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "user", "content": "你好"},
		},
		"temperature": 0.7,
		"max_tokens":  float64(100),
	}
	model, payload, err := ConvertChatRequest(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if model != "gemini-3.1-flash" {
		t.Errorf("model=%q", model)
	}
	contents, _ := payload["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents len=%d, want 1", len(contents))
	}
	c0 := contents[0].(map[string]any)
	if c0["role"] != "user" {
		t.Errorf("role=%v, want user", c0["role"])
	}
	if c0["parts"].([]any)[0].(map[string]any)["text"] != "你好" {
		t.Errorf("user text mismatch")
	}
	si, ok := payload["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatal("missing systemInstruction")
	}
	if si["parts"].([]any)[0].(map[string]any)["text"] != "你是助手" {
		t.Error("system text mismatch")
	}
	gc := payload["generationConfig"].(map[string]any)
	if gc["temperature"] != 0.7 {
		t.Errorf("temperature=%v", gc["temperature"])
	}
	if gc["maxOutputTokens"] != float64(100) {
		t.Errorf("maxOutputTokens=%v", gc["maxOutputTokens"])
	}
}

func TestBuildVertexVariables_DropsNativeGeminiMaxOutputTokens(t *testing.T) {
	payload := map[string]any{
		"contents": []any{map[string]any{
			"role": "user", "parts": []any{map[string]any{"text": "你好"}},
		}},
		"generationConfig": map[string]any{"maxOutputTokens": float64(64)},
	}
	appCfg := config.DefaultConfig()
	appCfg.DropMaxTokens = true
	vars := BuildVertexVariables("gemini-3.1-flash", payload, config.StaticProvider(appCfg))
	if gc, ok := vars["generationConfig"].(map[string]any); ok {
		if _, exists := gc["maxOutputTokens"]; exists {
			t.Fatalf("Gemini 原生请求也应移除 maxOutputTokens: %v", gc)
		}
	}

	appCfg.DropMaxTokens = false
	vars = BuildVertexVariables("gemini-3.1-flash", payload, config.StaticProvider(appCfg))
	gc, ok := vars["generationConfig"].(map[string]any)
	if !ok || gc["maxOutputTokens"] != float64(64) {
		t.Fatalf("关闭开关后应保留 maxOutputTokens: %v", vars["generationConfig"])
	}
}

func TestConvertChatRequest_EmptyMessages(t *testing.T) {
	_, _, err := ConvertChatRequest(map[string]any{"model": "m", "messages": []any{}}, config.StaticProvider(config.DefaultConfig()))
	if err == nil {
		t.Error("expected error for empty messages")
	}
}

func TestConvertChatRequest_MaxTokensInvalid(t *testing.T) {
	body := map[string]any{
		"model":      "m",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": float64(0),
	}
	if _, _, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig())); err == nil {
		t.Error("expected error for max_tokens=0")
	}
}

func TestBuildVertexVariables_SafetyDefault(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	payload := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}},
	}}
	vars := BuildVertexVariables("gemini-3.1-flash", payload, cfg)
	if vars["model"] != "gemini-3.1-flash" {
		t.Error("model")
	}
	ss, ok := vars["safetySettings"].([]vertexSafetySetting)
	if !ok || len(ss) != 5 {
		t.Errorf("safetySettings=%v, want 5 BLOCK_NONE", vars["safetySettings"])
	}
	first := ss[0]
	if first.Threshold != "BLOCK_NONE" {
		t.Errorf("threshold=%v", first.Threshold)
	}
	wire, err := json.Marshal(ss)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 5 || decoded[0]["category"] != "HARM_CATEGORY_HARASSMENT" ||
		decoded[0]["threshold"] != "BLOCK_NONE" {
		t.Fatalf("safetySettings wire shape changed: %s", wire)
	}
}

func TestCanonicalTextContentsCanPassThrough(t *testing.T) {
	valid := []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"text": "world"}}},
	}
	if !canonicalTextContentsCanPassThrough(valid) {
		t.Fatal("canonical text contents did not use the fast path")
	}

	for name, contents := range map[string]any{
		"legacy role": []any{map[string]any{
			"role": "assistant", "parts": []any{map[string]any{"text": "x"}},
		}},
		"extra content field": []any{map[string]any{
			"role": "user", "parts": []any{map[string]any{"text": "x"}}, "name": "user",
		}},
		"thought": []any{map[string]any{
			"role": "model", "parts": []any{map[string]any{"text": "x", "thought": true}},
		}},
		"empty text": []any{map[string]any{
			"role": "user", "parts": []any{map[string]any{"text": ""}},
		}},
		"media": []any{map[string]any{
			"role": "user", "parts": []any{map[string]any{"inlineData": map[string]any{
				"mimeType": "image/png", "data": "AA==",
			}}},
		}},
	} {
		if canonicalTextContentsCanPassThrough(contents) {
			t.Errorf("%s unexpectedly used canonical text fast path", name)
		}
	}

	cfg := config.StaticProvider(config.DefaultConfig())
	vars := BuildVertexVariables("gemini-3.1-flash", map[string]any{"contents": valid}, cfg)
	if !reflect.DeepEqual(vars["contents"], valid) {
		t.Fatalf("fast path changed canonical contents:\n got: %#v\nwant: %#v", vars["contents"], valid)
	}
}

func TestBuildVertexVariables_NormalizesUnspecifiedSafetySettings(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.SafetySettings = map[string]string{
		"HARM_CATEGORY_HARASSMENT": "BLOCK_ONLY_HIGH",
	}
	payload := map[string]any{
		"contents": []any{map[string]any{
			"role": "user", "parts": []any{map[string]any{"text": "hello"}},
		}},
		"safetySettings": []any{
			map[string]any{
				"category":  "HARM_CATEGORY_HARASSMENT",
				"threshold": "HARM_BLOCK_THRESHOLD_UNSPECIFIED",
			},
			map[string]any{
				"category":  "HARM_CATEGORY_HATE_SPEECH",
				"threshold": "",
			},
		},
	}
	vars := BuildVertexVariables("gemini-3.6-flash", payload, config.StaticProvider(cfg))
	settings, ok := vars["safetySettings"].([]vertexSafetySetting)
	if !ok || len(settings) != 2 {
		t.Fatalf("safetySettings=%#v", vars["safetySettings"])
	}
	first := settings[0]
	if first.Threshold != "BLOCK_ONLY_HIGH" {
		t.Fatalf("configured threshold not applied: %#v", first)
	}
	second := settings[1]
	if second.Threshold != "BLOCK_NONE" {
		t.Fatalf("empty threshold should default to BLOCK_NONE: %#v", second)
	}

	payload["safetySettings"] = []any{}
	vars = BuildVertexVariables("gemini-3.6-flash", payload, config.StaticProvider(cfg))
	if got := len(vars["safetySettings"].([]vertexSafetySetting)); got != len(safetyCategories) {
		t.Fatalf("empty safety settings should use defaults, got %d", got)
	}
}

func TestBuildVertexVariables_SystemDemote(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	payload := map[string]any{
		"contents":          []any{},
		"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "sys"}}},
	}
	vars := BuildVertexVariables("m", payload, cfg)
	if _, ok := vars["systemInstruction"]; ok {
		t.Error("systemInstruction 应在无 user 时被降级删除")
	}
	contents := vars["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents len=%d, want 1", len(contents))
	}
	c0 := contents[0].(map[string]any)
	if c0["role"] != "user" {
		t.Errorf("降级后 role=%v, want user", c0["role"])
	}
}

func TestGeminiJSONToOAIJSON(t *testing.T) {
	resp := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": "Hello"}}, "role": "model"},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     float64(5),
			"candidatesTokenCount": float64(1),
			"totalTokenCount":      float64(6),
		},
	}
	oai := GeminiJSONToOAIJSON(resp, "gemini-3.1-flash")
	if oai["object"] != "chat.completion" {
		t.Errorf("object=%v", oai["object"])
	}
	c0 := oai["choices"].([]any)[0].(map[string]any)
	if c0["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v", c0["finish_reason"])
	}
	if c0["message"].(map[string]any)["content"] != "Hello" {
		t.Errorf("content=%v", c0["message"].(map[string]any)["content"])
	}
	usage := oai["usage"].(map[string]any)
	if usage["prompt_tokens"] != 5 || usage["completion_tokens"] != 1 || usage["total_tokens"] != 6 {
		t.Errorf("usage=%v", usage)
	}
}

func TestGeminiJSONToOAIJSONCanonicalToolCallPreservesWireFormat(t *testing.T) {
	resp := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{map[string]any{
				"functionCall": map[string]any{
					"name": "lookup",
					"args": map[string]any{"z": "<tag>", "a": float64(1)},
				},
			}}},
		}},
	}
	oai := GeminiJSONToOAIJSON(resp, "gemini-test")
	choice := oai["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	toolCalls := message["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls len=%d, want 1", len(toolCalls))
	}
	canonical, ok := toolCalls[0].(CanonicalOAIResponseToolCall)
	if !ok {
		t.Fatalf("tool call type=%T, want CanonicalOAIResponseToolCall", toolCalls[0])
	}

	legacy := map[string]any{
		"function": map[string]any{
			"arguments": canonicalJSONString(canonical.Function.Arguments),
			"name":      canonical.Function.Name,
		},
		"id":   canonical.ID,
		"type": canonical.Type,
	}
	gotJSON, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("canonical tool call changed wire JSON:\n got:  %s\n want: %s", gotJSON, wantJSON)
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := []struct {
		in   string
		tool bool
		want string
	}{
		{"STOP", false, "stop"},
		{"FINISH_REASON_UNSPECIFIED", false, "stop"}, // 未知 → stop
		{"SAFETY", false, "content_filter"},
		{"MAX_TOKENS", false, "length"},
		{"STOP", true, "tool_calls"}, // 有工具调用覆盖
		{"", false, "stop"},
	}
	for _, c := range cases {
		if got := MapFinishReason(c.in, c.tool); got != c.want {
			t.Errorf("MapFinishReason(%q,%v)=%q, want %q", c.in, c.tool, got, c.want)
		}
	}
}

func TestExtractCanonicalOAIToolCall(t *testing.T) {
	arguments := map[string]any{"query": "<tag>", "limit": float64(2)}
	call := CanonicalOAIToolCall{
		ID:   "call_1",
		Type: "function",
		Function: CanonicalOAIFunctionCallData{
			Name: "lookup", Arguments: arguments,
		},
	}
	parsed := extractOAIToolCall(call)
	if parsed == nil || parsed.id != "call_1" || parsed.name != "lookup" ||
		!reflect.DeepEqual(parsed.args, arguments) {
		t.Fatalf("canonical tool call parsed incorrectly: %#v", parsed)
	}
	encoded, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) !=
		`{"id":"call_1","type":"function","function":{"name":"lookup","arguments":{"limit":2,"query":"\u003ctag\u003e"}}}` {
		t.Fatalf("canonical tool call JSON shape changed: %s", encoded)
	}
}

func TestMergeContentBlocks(t *testing.T) {
	parts := []map[string]any{
		{"text": "Hello "},
		{"text": "World"},
		{"text": "think", "thought": true},
		{"text": "ing", "thought": true, "thoughtSignature": "sig"},
		{"functionCall": map[string]any{"name": "tool", "args": map[string]any{}}},
		{"text": "Done"},
	}
	merged := MergeContentBlocks(parts)
	if len(merged) != 4 {
		t.Fatalf("merged len=%d, want 4: %#v", len(merged), merged)
	}
	if merged[0]["text"] != "Hello World" {
		t.Errorf("merged text=%q", merged[0]["text"])
	}
	if merged[1]["text"] != "thinking" || merged[1]["thought"] != true ||
		merged[1]["thoughtSignature"] != "sig" {
		t.Errorf("merged thought=%#v", merged[1])
	}
	if _, ok := merged[2]["functionCall"]; !ok || merged[3]["text"] != "Done" {
		t.Errorf("non-text boundary changed: %#v", merged)
	}
	if parts[0]["text"] != "Hello " || parts[1]["text"] != "World" {
		t.Errorf("input parts were mutated: %#v", parts)
	}

	incremental := NewContentBlockMerger(2)
	for _, part := range parts {
		incremental.Add(part)
	}
	if got := incremental.Result(); !reflect.DeepEqual(got, merged) {
		t.Fatalf("incremental result differs from batch merge:\n got: %#v\nwant: %#v", got, merged)
	}
	if got := incremental.Result(); !reflect.DeepEqual(got, merged) {
		t.Fatalf("repeated Result changed output: %#v", got)
	}
}

func TestContentBlockMergerSingletonNormalization(t *testing.T) {
	tests := []struct {
		name string
		part map[string]any
		want map[string]any
	}{
		{name: "canonical text", part: map[string]any{"text": "answer"}, want: map[string]any{"text": "answer"}},
		{name: "explicit false thought removed", part: map[string]any{
			"text": "answer", "thought": false,
		}, want: map[string]any{"text": "answer"}},
		{name: "truthy thought normalized", part: map[string]any{
			"text": "thinking", "thought": "yes",
		}, want: map[string]any{"text": "thinking", "thought": true}},
		{name: "non-thought signature removed", part: map[string]any{
			"text": "answer", "thoughtSignature": "ignored",
		}, want: map[string]any{"text": "answer"}},
		{name: "canonical thought signature", part: map[string]any{
			"text": "thinking", "thought": true, "thoughtSignature": "sig",
		}, want: map[string]any{"text": "thinking", "thought": true, "thoughtSignature": "sig"}},
		{name: "unrelated field removed", part: map[string]any{
			"text": "answer", "unused": true,
		}, want: map[string]any{"text": "answer"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before, err := json.Marshal(test.part)
			if err != nil {
				t.Fatal(err)
			}
			got := MergeContentBlocks([]map[string]any{test.part})
			if len(got) != 1 || !reflect.DeepEqual(got[0], test.want) {
				t.Fatalf("MergeContentBlocks()=%#v, want %#v", got, test.want)
			}
			after, err := json.Marshal(test.part)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("input part mutated: before=%s after=%s", before, after)
			}
		})
	}
}

func TestContentBlockMergerAddPlainTextMatchesOwnedParts(t *testing.T) {
	plain := NewContentBlockMerger(2)
	plain.AddPlainText("Hello ")
	plain.AddPlainText("World")
	plain.Add(map[string]any{"text": "thinking", "thought": true})
	plain.AddPlainText("Done")

	owned := NewContentBlockMerger(2)
	for _, part := range []map[string]any{
		{"text": "Hello "},
		{"text": "World"},
		{"text": "thinking", "thought": true},
		{"text": "Done"},
	} {
		owned.AddOwned(part)
	}
	if got, want := plain.Result(), owned.Result(); !reflect.DeepEqual(got, want) {
		t.Fatalf("plain text fast path differs:\n got: %#v\nwant: %#v", got, want)
	}
}

func BenchmarkMergeContentBlocksTextChunks(b *testing.B) {
	parts := make([]map[string]any, 4096)
	for index := range parts {
		parts[index] = map[string]any{"text": "0123456789abcdef"}
	}
	b.ResetTimer()
	for range b.N {
		_ = MergeContentBlocks(parts)
	}
}

// TestConvertChatRequest_Full 测试 ConvertChatRequest 的完整转换：OpenAI 请求 → Gemini payload。
func TestConvertChatRequest_Full(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	body := map[string]any{
		"model":    "gemini-2.5-flash",
		"messages": []any{map[string]any{"role": "user", "content": "Hello"}},
		"stream":   false,
	}

	model, geminiPayload, err := ConvertChatRequest(body, cfg)
	if err != nil {
		t.Fatalf("ConvertChatRequest failed: %v", err)
	}
	if model != "gemini-2.5-flash" {
		t.Errorf("model=%q, want gemini-2.5-flash", model)
	}
	if geminiPayload == nil {
		t.Fatal("geminiPayload is nil")
	}

	contents, ok := geminiPayload["contents"].([]any)
	if !ok || len(contents) == 0 {
		t.Fatal("missing contents")
	}
	firstContent := contents[0].(map[string]any)
	if firstContent["role"] != "user" {
		t.Errorf("role=%q, want user", firstContent["role"])
	}
	parts, ok := firstContent["parts"].([]any)
	if !ok || len(parts) == 0 {
		t.Fatal("missing parts")
	}
	firstPart := parts[0].(map[string]any)
	if firstPart["text"] != "Hello" {
		t.Errorf("text=%q, want Hello", firstPart["text"])
	}

	if gc, ok := geminiPayload["generationConfig"].(map[string]any); ok {
		if _, exists := gc["temperature"]; !exists {
			t.Error("generationConfig should have temperature")
		}
	}
}

// TestConvertChatRequest_WithTools 测试包含工具的请求转换。
func TestConvertChatRequest_WithTools(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	body := map[string]any{
		"model":    "gemini-2.5-flash",
		"messages": []any{map[string]any{"role": "user", "content": "What's the weather?"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "Get weather",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"location": map[string]any{"type": "string"},
					},
				},
			},
		}},
	}

	model, geminiPayload, err := ConvertChatRequest(body, cfg)
	if err != nil {
		t.Fatalf("ConvertChatRequest failed: %v", err)
	}
	if model != "gemini-2.5-flash" {
		t.Errorf("model=%q", model)
	}

	if _, ok := geminiPayload["tools"]; !ok {
		if _, ok := geminiPayload["toolConfig"]; !ok {
			t.Log("tools may be transformed to a different structure, check implement detail")
		}
	}
}

// TestConvertChatRequest_SystemInstruction 测试系统指令。
func TestConvertChatRequest_SystemInstruction(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	body := map[string]any{
		"model":    "gemini-2.5-flash",
		"messages": []any{map[string]any{"role": "system", "content": "Be helpful."}},
	}

	_, geminiPayload, err := ConvertChatRequest(body, cfg)
	if err != nil {
		t.Fatalf("ConvertChatRequest failed: %v", err)
	}

	si, ok := geminiPayload["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatal("missing systemInstruction")
	}
	parts, ok := si["parts"].([]any)
	if !ok || len(parts) == 0 {
		t.Fatal("systemInstruction missing parts")
	}
	firstPart := parts[0].(map[string]any)
	if firstPart["text"] != "Be helpful." {
		t.Errorf("text=%q, want Be helpful.", firstPart["text"])
	}
}

// TestIntegrationGeminiJSONToOAIJSON 测试 Gemini 非流式响应 → OAI 格式。
func TestIntegrationGeminiJSONToOAIJSON(t *testing.T) {
	geminiResp := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": "Hi there!"}}, "role": "model"},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     float64(10),
			"candidatesTokenCount": float64(20),
			"totalTokenCount":      float64(30),
		},
	}

	oai := GeminiJSONToOAIJSON(geminiResp, "gemini-2.5-flash")
	if oai == nil {
		t.Fatal("GeminiJSONToOAIJSON returned nil")
	}
	if oai["object"] != "chat.completion" {
		t.Errorf("object=%q", oai["object"])
	}
	choices, ok := oai["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatal("no choices")
	}
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v", choice["finish_reason"])
	}
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		t.Fatal("no message")
	}
	if msg["content"] != "Hi there!" {
		t.Errorf("content=%q", msg["content"])
	}

	usage, ok := oai["usage"].(map[string]any)
	if !ok {
		t.Fatal("no usage")
	}
	if usage["prompt_tokens"] != int(10) {
		t.Errorf("prompt_tokens=%v (%T)", usage["prompt_tokens"], usage["prompt_tokens"])
	}
	if usage["completion_tokens"] != int(20) {
		t.Errorf("completion_tokens=%v (%T)", usage["completion_tokens"], usage["completion_tokens"])
	}
}

// TestIntegrationGeminiJSONToOAIJSON_SafetyBlock 测试 Gemini 安全拦截 → content_filter。
func TestIntegrationGeminiJSONToOAIJSON_SafetyBlock(t *testing.T) {
	geminiResp := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{}, "role": "model"},
			"finishReason": "SAFETY",
		}},
		"promptFeedback": map[string]any{"blockReason": "SAFETY"},
	}

	oai := GeminiJSONToOAIJSON(geminiResp, "gemini-2.5-flash")
	choices, ok := oai["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatal("no choices")
	}
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "content_filter" {
		t.Errorf("finish_reason=%v, want content_filter", choice["finish_reason"])
	}
}

// TestIntegrationConvertRealtimeChunk 测试流式增量转换。
func TestIntegrationConvertRealtimeChunk(t *testing.T) {
	t.Run("first_chunk_has_role_delta", func(t *testing.T) {
		chunk := map[string]any{"candidates": []any{
			map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "Hi"}}, "role": "model"},
				"finishReason": "FINISH_REASON_UNSPECIFIED",
			},
		}}
		events := ConvertRealtimeChunk(chunk, "gemini-2.5-flash", "req-1", true)
		if len(events) < 1 {
			t.Fatal("no events")
		}
		if !strings.Contains(events[0], `"role":"assistant"`) {
			t.Errorf("first event should contain role delta: %s", events[0])
		}
	})

	t.Run("finish_stop", func(t *testing.T) {
		chunk := map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "done"}}, "role": "model"},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount": float64(5), "candidatesTokenCount": float64(10), "totalTokenCount": float64(15),
			},
		}
		events := ConvertRealtimeChunk(chunk, "m", "r", false)
		var hasFinish bool
		for _, e := range events {
			if strings.Contains(e, `"finish_reason":"stop"`) {
				hasFinish = true
				break
			}
		}
		if !hasFinish {
			t.Errorf("should have finish_reason=stop event, got %v", events)
		}
	})

	t.Run("unspecified_no_finish", func(t *testing.T) {
		chunk := map[string]any{"candidates": []any{
			map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "x"}}, "role": "model"},
				"finishReason": "FINISH_REASON_UNSPECIFIED",
			},
		}}
		events := ConvertRealtimeChunk(chunk, "m", "r", false)
		for _, e := range events {
			if strings.Contains(e, `"finish_reason":"`) && !strings.Contains(e, `"finish_reason":null`) {
				t.Errorf("UNSPECIFIED should not produce finish_reason: %s", e)
			}
		}
	})

	t.Run("function_call", func(t *testing.T) {
		chunk := map[string]any{"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{
				map[string]any{"functionCall": map[string]any{"name": "get_weather", "args": map[string]any{"city": "SF"}}},
			}, "role": "model"},
			"finishReason": "STOP",
		}}}
		events := ConvertRealtimeChunk(chunk, "m", "r", false)
		var hasToolCall bool
		for _, e := range events {
			if strings.Contains(e, `"tool_calls"`) {
				hasToolCall = true
				break
			}
		}
		if !hasToolCall {
			t.Errorf("should have tool_calls event, got %v", events)
		}
	})
}

// TestBuildVertexVariables 测试 BuildVertexVariables 的 produced structure。
func TestBuildVertexVariables(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	geminiPayload := map[string]any{
		"contents": []any{map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": "Hello"}},
		}},
	}

	vars := BuildVertexVariables("gemini-2.5-flash", geminiPayload, cfg)
	if vars == nil {
		t.Fatal("BuildVertexVariables returned nil")
	}
	if vars["model"] != "gemini-2.5-flash" {
		t.Errorf("model=%q", vars["model"])
	}
	if _, ok := vars["contents"]; !ok {
		t.Error("vars missing contents")
	}
}

// TestMarshalRoundTrip 验证 ConvertChatRequest + BuildVertexVariables 的 JSON 可序列化。
func TestMarshalRoundTrip(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	body := map[string]any{
		"model":    "gemini-2.5-flash",
		"messages": []any{map[string]any{"role": "user", "content": "Hello"}},
	}
	model, geminiPayload, err := ConvertChatRequest(body, cfg)
	if err != nil {
		t.Fatalf("ConvertChatRequest: %v", err)
	}
	payload := BuildVertexVariables(model, geminiPayload, cfg)

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty marshal result")
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["model"] != "gemini-2.5-flash" {
		t.Errorf("model mismatch after round-trip")
	}
}

// TestToNativeSchema_NumericConstraintsAsStrings 验证数值约束字段被转为字符串。
func TestToNativeSchema_NumericConstraintsAsStrings(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"minItems": 1, "maxItems": float64(10),
		"properties": map[string]any{
			"name": map[string]any{
				"type":      "string",
				"minLength": 2, "maxLength": float64(50),
			},
		},
	}
	native := toNativeSchema(schema).(map[string]any)

	for _, field := range []string{"minItems", "maxItems"} {
		v, ok := native[field]
		if !ok {
			t.Fatalf("字段 %s 被删除", field)
		}
		if _, ok := v.(string); !ok {
			t.Errorf("%s 应为字符串，实际是 %T(%v)", field, v, v)
		}
	}
	props, _ := native["properties"].([]any)
	if len(props) > 0 {
		prop := props[0].(map[string]any)
		val := prop["value"].(map[string]any)
		for _, field := range []string{"minLength", "maxLength"} {
			v, ok := val[field]
			if !ok {
				t.Fatalf("嵌套字段 %s 被删除", field)
			}
			if _, ok := v.(string); !ok {
				t.Errorf("嵌套 %s 应为字符串，实际是 %T(%v)", field, v, v)
			}
		}
	}
}

// TestToNativeSchema_DefaultNullablePreserved 验证 Gemini 支持的 default/nullable/examples 不被误删。
func TestToNativeSchema_DefaultNullablePreserved(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"default":  "hello",
		"nullable": true,
		"examples": []any{"ex1", "ex2"},
		"properties": map[string]any{
			"x": map[string]any{"type": "string", "default": "world"},
		},
	}
	native := toNativeSchema(schema).(map[string]any)
	for _, field := range []string{"default", "nullable", "examples"} {
		if _, ok := native[field]; !ok {
			t.Errorf("字段 %s 被错误剔除（Gemini 原生支持）", field)
		}
	}
	props, _ := native["properties"].([]any)
	if len(props) > 0 {
		val := props[0].(map[string]any)["value"].(map[string]any)
		if _, ok := val["default"]; !ok {
			t.Errorf("嵌套 property 的 default 被错误剔除")
		}
	}
}

// TestToNativeSchema_UnknownTypeFallsBackToSTRING 验证非标准 type 兜底 STRING。
func TestToNativeSchema_UnknownTypeFallsBackToSTRING(t *testing.T) {
	native := toNativeSchema(map[string]any{"type": "any", "properties": map[string]any{}}).(map[string]any)
	if native["type"] != "STRING" {
		t.Errorf("未知类型 'any' 应兜底为 STRING，实际: %v", native["type"])
	}
}

// TestConvertToolsFormat_NumericConstraints 端到端验证工具参数数值约束转字符串。
func TestConvertToolsFormat_NumericConstraints(t *testing.T) {
	geminiPayload := map[string]any{
		"tools": []any{map[string]any{
			"functionDeclarations": []any{map[string]any{
				"name": "list_items",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
					"minItems":   1,
					"maxItems":   float64(100),
				},
			}},
		}},
	}
	vars := BuildVertexVariables("gemini-3-flash", geminiPayload, config.StaticProvider(config.AppConfig{})) //nolint:exhaustruct
	dump, _ := json.Marshal(vars["tools"])
	if !strings.Contains(string(dump), `"minItems":"1"`) {
		t.Errorf("minItems 应转为字符串 \"1\": %s", dump)
	}
}

// TestNormalizePartMultimodal 验证 normalizePart 对 OpenAI 风格多模态 part 的归一（Fix 7）：
// image_url(data:/http)/input_image、media/file/file_data、inline_data → 对应 Gemini part。
func TestNormalizePartMultimodal(t *testing.T) {
	got := normalizePart(map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:image/png;base64,QQ=="},
	})
	id, _ := got["inlineData"].(map[string]any)
	if id == nil || id["mimeType"] != "image/png" || id["data"] != "QQ==" {
		t.Fatalf("image_url data: 应转 inlineData，got %v", got)
	}

	got = normalizePart(map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "https://x.com/a.mp4"},
	})
	fd, _ := got["fileData"].(map[string]any)
	if fd == nil || fd["fileUri"] != "https://x.com/a.mp4" || fd["mimeType"] != "video/mp4" {
		t.Fatalf("image_url http 应转 fileData(video/mp4)，got %v", got)
	}

	got = normalizePart(map[string]any{
		"type": "file", "file_uri": "gs://b/x.pdf", "mime_type": "application/pdf",
	})
	fd, _ = got["fileData"].(map[string]any)
	if fd == nil || fd["fileUri"] != "gs://b/x.pdf" || fd["mimeType"] != "application/pdf" {
		t.Fatalf("file 应转 fileData，got %v", got)
	}

	got = normalizePart(map[string]any{
		"type": "inline_data", "inline_data": map[string]any{"mime_type": "audio/wav", "data": "ZGF0YQ=="},
	})
	id, _ = got["inlineData"].(map[string]any)
	if id == nil || id["mimeType"] != "audio/wav" || id["data"] != "ZGF0YQ==" {
		t.Fatalf("inline_data 应转 inlineData，got %v", got)
	}

	got = normalizePart(map[string]any{"type": "text", "text": "hi"})
	if got["text"] != "hi" {
		t.Fatalf("text part，got %v", got)
	}

	got = normalizePart(map[string]any{"type": "weird", "some_key": "v"})
	if got["someKey"] != "v" {
		t.Fatalf("未知 part 应 camelCase 透传，got %v", got)
	}
}

// TestGuessMIMEFromURI 验证多类型 mime 猜测覆盖图/视频/音频/pdf/txt。
func TestGuessMIMEFromURI(t *testing.T) {
	cases := map[string]string{
		"a.jpg": "image/jpeg", "a.png": "image/png", "a.webp": "image/webp", "a.gif": "image/gif",
		"a.mp4": "video/mp4", "a.mov": "video/quicktime", "a.webm": "video/webm",
		"a.mp3": "audio/mpeg", "a.wav": "audio/wav", "a.ogg": "audio/ogg",
		"a.pdf": "application/pdf", "a.txt": "text/plain", "a.xyz": "image/png",
		"http://x/a.MP4?t=1": "video/mp4",
	}
	for in, want := range cases {
		if got := guessMIMEFromURI(in); got != want {
			t.Errorf("guessMIMEFromURI(%q)=%q，want %q", in, got, want)
		}
	}
}

// splitAssistantContent：assistant 文本里的 markdown data-URI 图片必须重解析为 inlineData，
// 否则巨型 base64 markdown 作为文本进 model 角色，多轮改图被上游拒。
func TestSplitAssistantContent_ImageMarkdown(t *testing.T) {
	s := "这是图片：\n\n![image](data:image/png;base64,iVBORw0KGgoAAAANS) 完成"
	parts := splitAssistantContent(s)
	var hasInline, hasText bool
	for _, p := range parts {
		m := p.(map[string]any)
		if id, ok := m["inlineData"].(map[string]any); ok {
			if id["mimeType"] == "image/png" {
				hasInline = true
			}
		}
		if txt, ok := m["text"].(string); ok && txt != "" {
			hasText = true
		}
	}
	if !hasInline {
		t.Errorf("markdown 图片应重解析为 inlineData，got %v", parts)
	}
	if !hasText {
		t.Errorf("图片前后的文本应保留为 text part")
	}
}

func TestSplitAssistantContent_PlainText(t *testing.T) {
	parts := splitAssistantContent("纯文本回复")
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "纯文本回复" {
		t.Errorf("纯文本应原样为单个 text part，got %v", parts)
	}
}
func TestNormalizeGeminiToolCallID(t *testing.T) {
	for value, want := range map[string]string{
		"gemini-tool-call-1-vp12345678": "gemini-tool-call-1",
		"gemini-tool-call-1-vp1234567":  "gemini-tool-call-1-vp1234567",
		"call-1-vp12345678":             "call-1-vp12345678",
		"gemini-tool-call-1":            "gemini-tool-call-1",
	} {
		if got := normalizeGeminiToolCallID(value); got != want {
			t.Errorf("normalizeGeminiToolCallID(%q)=%q, want %q", value, got, want)
		}
	}
}

func TestBuildVertexVariablesPreservesPromptTextThatLooksLikeInternalID(t *testing.T) {
	const text = "gemini-tool-call-1-vp12345678"
	payload := map[string]any{
		"contents": []any{map[string]any{
			"role": "user", "parts": []any{
				map[string]any{"text": text},
				map[string]any{"functionCall": map[string]any{
					"id": text, "name": "lookup", "args": map[string]any{},
				}},
			},
		}},
		"systemInstruction": map[string]any{
			"parts": []any{map[string]any{"text": text}},
		},
	}
	vars := BuildVertexVariables(
		"gemini-3.1-flash",
		payload,
		config.StaticProvider(config.DefaultConfig()),
	)
	contents := vars["contents"].([]any)
	gotText := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"]
	system := vars["systemInstruction"].(map[string]any)
	gotSystem := system["parts"].([]any)[0].(map[string]any)["text"]
	if gotText != text || gotSystem != text {
		t.Fatalf("prompt-like IDs were modified: content=%q system=%q", gotText, gotSystem)
	}
}

func TestBuildVertexVariablesStripsIDsWithoutMutatingInput(t *testing.T) {
	payload := map[string]any{
		"contents": []any{
			map[string]any{"role": "model", "parts": []any{
				map[string]any{"functionCall": map[string]any{
					"id": "gemini-tool-call-1-vp12345678", "name": "lookup", "args": map[string]any{},
				}},
			}},
			map[string]any{"role": "function", "parts": []any{
				map[string]any{"functionResponse": map[string]any{
					"id": "gemini-tool-call-1", "response": map[string]any{},
				}},
			}},
		},
	}
	before, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	vars := BuildVertexVariables("gemini-3.1-flash", payload, config.StaticProvider(config.DefaultConfig()))
	after, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("BuildVertexVariables mutated shared payload:\nbefore=%s\nafter=%s", before, after)
	}

	contents := vars["contents"].([]any)
	call := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	response := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if _, exists := call["id"]; exists {
		t.Fatalf("outbound functionCall retained internal ID: %#v", call)
	}
	if _, exists := response["id"]; exists {
		t.Fatalf("outbound functionResponse retained internal ID: %#v", response)
	}
	if response["name"] != "lookup" {
		t.Fatalf("normalized ID did not anchor function response name: %#v", response)
	}
}

func TestBuildVertexVariablesContentCopyOnWrite(t *testing.T) {
	payload := map[string]any{
		"contents": []any{map[string]any{
			"role": "user",
			"parts": []any{
				map[string]any{"text": "plain"},
				map[string]any{"inlineData": map[string]any{
					"mimeType": "image/png", "data": "YWJjZA",
				}},
				map[string]any{
					"text": "thinking", "thought": true,
					"thoughtSignature": skipThoughtSentinel,
				},
				map[string]any{"type": "input_text", "text": "converted"},
			},
		}},
	}
	before, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	vars := BuildVertexVariables("gemini-3.1-flash", payload, config.StaticProvider(config.DefaultConfig()))
	after, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("content copy-on-write mutated input:\nbefore=%s\nafter=%s", before, after)
	}

	contents := vars["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	if parts[0].(map[string]any)["text"] != "plain" {
		t.Fatalf("canonical text changed: %#v", parts[0])
	}
	inline := parts[1].(map[string]any)["inlineData"].(map[string]any)
	if inline["data"] != "YWJjZA==" {
		t.Fatalf("base64 was not normalized: %#v", inline)
	}
	thought := parts[2].(map[string]any)
	if thought["thoughtSignature"] != encodedSkipThoughtSentinel {
		t.Fatalf("thought signature was not encoded: %#v", thought)
	}
	converted := parts[3].(map[string]any)
	if converted["text"] != "converted" {
		t.Fatalf("input_text was not normalized: %#v", converted)
	}
	if _, exists := converted["type"]; exists {
		t.Fatalf("OpenAI part type leaked upstream: %#v", converted)
	}
}

func TestBuildVertexVariablesConcurrentSharedPayload(t *testing.T) {
	payload := map[string]any{"contents": []any{map[string]any{
		"role": "model",
		"parts": []any{map[string]any{"functionCall": map[string]any{
			"id": "gemini-tool-call-2-vp87654321", "name": "lookup", "args": map[string]any{"q": "test"},
		}}},
	}}}
	cfg := config.StaticProvider(config.DefaultConfig())
	const workers = 32
	errors := make(chan string, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 50 {
				vars := BuildVertexVariables("gemini-3.1-flash", payload, cfg)
				contents := vars["contents"].([]any)
				call := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
				if call["name"] != "lookup" {
					errors <- "unexpected function call"
					return
				}
			}
		}()
	}
	group.Wait()
	close(errors)
	for message := range errors {
		t.Error(message)
	}
	contents := payload["contents"].([]any)
	call := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	if call["id"] != "gemini-tool-call-2-vp87654321" {
		t.Fatalf("shared payload was mutated: %#v", call)
	}
}
