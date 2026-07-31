package transform

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

func TestDefaultRequestConverterCompactsPlainTextWithoutChangingWireJSON(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	body := map[string]any{
		"model": "gemini-3.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "system prompt"},
			map[string]any{"role": "user", "content": "question"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "answer"},
			}},
			map[string]any{"role": "user", "content": "follow-up"},
		},
		"temperature": float64(0.5),
	}
	model, compatibilityPayload, err := ConvertChatRequest(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	compactModel, compactPayload, err := DefaultRequestConverter().Convert(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if compactModel != model {
		t.Fatalf("model=%q, want %q", compactModel, model)
	}
	contents, ok := compactPayload["contents"].([]any)
	if !ok || len(contents) != 3 {
		t.Fatalf("compact contents=%#v", compactPayload["contents"])
	}
	for index, content := range contents {
		if _, ok := content.(*canonicalSingleTextContent); !ok {
			t.Fatalf("content[%d] type=%T, want compact text content", index, content)
		}
	}
	systemInstruction, ok := compactPayload["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("compact system instruction=%#v", compactPayload["systemInstruction"])
	}
	systemParts, ok := systemInstruction["parts"].([]any)
	if !ok || len(systemParts) != 1 ||
		systemParts[0].(map[string]any)["text"] != "system prompt" {
		t.Fatalf("compact system parts=%#v", systemInstruction["parts"])
	}
	compatibilityJSON, err := json.Marshal(compatibilityPayload)
	if err != nil {
		t.Fatal(err)
	}
	compactJSON, err := json.Marshal(compactPayload)
	if err != nil {
		t.Fatal(err)
	}
	if string(compactJSON) != string(compatibilityJSON) {
		t.Fatalf("compact payload changed JSON:\ncompact=%s\ncompat=%s", compactJSON, compatibilityJSON)
	}

	compatibilityVariables := BuildVertexVariables(model, compatibilityPayload, cfg)
	compactVariables := BuildVertexVariables(compactModel, compactPayload, cfg)
	compatibilityJSON, err = json.Marshal(compatibilityVariables)
	if err != nil {
		t.Fatal(err)
	}
	compactJSON, err = json.Marshal(compactVariables)
	if err != nil {
		t.Fatal(err)
	}
	if string(compactJSON) != string(compatibilityJSON) {
		t.Fatalf("compact variables changed JSON:\ncompact=%s\ncompat=%s", compactJSON, compatibilityJSON)
	}
}

func TestDefaultRequestConverterCompactsTextInsideToolHistory(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	body := map[string]any{
		"model": "gemini-3.1-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "question"},
			}},
			map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{&CanonicalOAIToolCall{
					ID:   "call_1",
					Type: "function",
					Function: CanonicalOAIFunctionCallData{
						Name: "lookup", Arguments: map[string]any{"query": "value"},
					},
				}},
			},
			map[string]any{
				"role": "tool", "tool_call_id": "call_1",
				"content": map[string]any{"result": "answer"},
			},
		},
	}
	model, compatibilityPayload, err := ConvertChatRequest(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	compactModel, compactPayload, err := DefaultRequestConverter().Convert(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	contents := compactPayload["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("compact contents=%d, want 3", len(contents))
	}
	if _, ok := contents[0].(*canonicalSingleTextContent); !ok {
		t.Fatalf("mixed user text type=%T, want compact text content", contents[0])
	}
	if !canonicalToolContentsCanSkipNormalization(compactPayload["contents"]) {
		t.Fatal("canonical mixed tool history did not select the normalization fast path")
	}

	for stage, values := range []struct {
		compatibility any
		compact       any
	}{
		{compatibility: compatibilityPayload, compact: compactPayload},
		{
			compatibility: BuildVertexVariables(model, compatibilityPayload, cfg),
			compact:       BuildVertexVariables(compactModel, compactPayload, cfg),
		},
	} {
		compatibilityJSON, err := json.Marshal(values.compatibility)
		if err != nil {
			t.Fatal(err)
		}
		compactJSON, err := json.Marshal(values.compact)
		if err != nil {
			t.Fatal(err)
		}
		if string(compactJSON) != string(compatibilityJSON) {
			t.Fatalf(
				"stage %d compact tool history changed JSON:\ncompact=%s\ncompat=%s",
				stage,
				compactJSON,
				compatibilityJSON,
			)
		}
	}
}

func TestCanonicalToolContentsSkipNormalizationOnlyWhenSafe(t *testing.T) {
	canonical := func(call, response map[string]any) []any {
		return []any{
			map[string]any{
				"role":  "model",
				"parts": []any{map[string]any{"functionCall": call}},
			},
			map[string]any{
				"role":  "function",
				"parts": []any{map[string]any{"functionResponse": response}},
			},
		}
	}
	validCall := map[string]any{
		"id": "call_1", "name": "lookup", "args": map[string]any{"query": "value"},
	}
	validResponse := map[string]any{
		"id": "call_1", "name": "lookup", "response": map[string]any{"result": "value"},
	}
	if !canonicalToolContentsCanSkipNormalization(canonical(validCall, validResponse)) {
		t.Fatal("canonical tool history should skip redundant normalization")
	}
	fastContents := canonical(validCall, validResponse)
	fallbackContents := canonical(validCall, validResponse)
	fallbackContents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["type"] = ""
	cfg := config.StaticProvider(config.DefaultConfig())
	fastVariables := BuildVertexVariables("gemini-test", map[string]any{"contents": fastContents}, cfg)
	fallbackVariables := BuildVertexVariables(
		"gemini-test",
		map[string]any{"contents": fallbackContents},
		cfg,
	)
	fastJSON, err := json.Marshal(fastVariables)
	if err != nil {
		t.Fatal(err)
	}
	fallbackJSON, err := json.Marshal(fallbackVariables)
	if err != nil {
		t.Fatal(err)
	}
	if string(fastJSON) != string(fallbackJSON) {
		t.Fatalf("fast path changed normalized output:\nfast=%s\nfallback=%s", fastJSON, fallbackJSON)
	}

	tests := []struct {
		name     string
		call     map[string]any
		response map[string]any
	}{
		{
			name: "string arguments need decoding",
			call: map[string]any{
				"id": "call_1", "name": "lookup", "args": `{"query":"value"}`,
			},
			response: validResponse,
		},
		{
			name: "scalar response needs wrapping",
			call: validCall,
			response: map[string]any{
				"id": "call_1", "name": "lookup", "response": "value",
			},
		},
		{
			name: "aliased call id needs normalization",
			call: map[string]any{
				"tool_call_id": "call_1", "name": "lookup", "args": map[string]any{},
			},
			response: validResponse,
		},
		{
			name: "nested base64 needs normalization",
			call: map[string]any{
				"id": "call_1", "name": "lookup",
				"args": map[string]any{
					"inlineData": map[string]any{"mimeType": "text/plain", "data": "YWJjZA"},
				},
			},
			response: validResponse,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if canonicalToolContentsCanSkipNormalization(canonical(test.call, test.response)) {
				t.Fatal("history requiring normalization selected the fast path")
			}
		})
	}

	adjacentResponses := append(
		canonical(validCall, validResponse),
		map[string]any{
			"role":  "function",
			"parts": []any{map[string]any{"functionResponse": validResponse}},
		},
	)
	if canonicalToolContentsCanSkipNormalization(adjacentResponses) {
		t.Fatal("adjacent function turns requiring merge selected the fast path")
	}
}

func TestHandleInlineDataCaseReusesCanonicalFunctionReferences(t *testing.T) {
	contents := []any{map[string]any{
		"role": "model",
		"parts": []any{
			map[string]any{"functionCall": map[string]any{
				"id": "call_1", "name": "lookup",
				"args": map[string]any{"snake_key_is_payload": true},
			}},
			map[string]any{"functionResponse": map[string]any{
				"id": "call_1", "name": "lookup",
				"response": map[string]any{"snake_key_is_payload": true},
			}},
		},
	}}
	got, changed := handleInlineDataCaseCopy(contents)
	if changed || &got.([]any)[0] != &contents[0] {
		t.Fatalf("canonical function references were copied: changed=%v, got=%#v", changed, got)
	}

	aliased := map[string]any{
		"tool_call_id": "call_2",
		"name":         "lookup",
		"response":     map[string]any{"value": true},
	}
	normalized, changed := camelizeFunctionRefCopy(aliased, "response")
	if !changed || normalized["id"] != "call_2" || normalized["toolCallId"] != nil {
		t.Fatalf("aliased function reference was not normalized: %#v", normalized)
	}
	if aliased["tool_call_id"] != "call_2" {
		t.Fatalf("function reference input was mutated: %#v", aliased)
	}
}

func TestCleanPartKeepsPublicThoughtSignatureContract(t *testing.T) {
	cleaned, ok := CleanPart(map[string]any{
		"functionCall": map[string]any{"name": "lookup", "args": map[string]any{}},
	}, nil, nil)
	if !ok || cleaned["thoughtSignature"] != skipThoughtSentinel {
		t.Fatalf("CleanPart signature=%#v, want raw sentinel", cleaned)
	}
	encoded := EncodeThoughtSignature([]any{map[string]any{
		"role": "model", "parts": []any{cleaned},
	}}, 0).([]any)
	part := encoded[0].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if part["thoughtSignature"] != encodedSkipThoughtSentinel {
		t.Fatalf("encoded signature=%#v, want base64 sentinel", part)
	}
}

func TestFilterEmptyContentsPackedPartsRemainIndependent(t *testing.T) {
	functionPart := func(id, name string) map[string]any {
		return map[string]any{"functionCall": map[string]any{
			"id": id, "name": name, "args": map[string]any{},
		}}
	}
	contents := []any{
		map[string]any{
			"role": "model", "parts": []any{functionPart("call_1", "first")},
		},
		map[string]any{
			"role": "model", "parts": []any{
				functionPart("call_2", "second"),
				functionPart("call_3", "third"),
			},
		},
	}
	filtered := filterEmptyContents(contents).([]any)
	firstParts := filtered[0].(map[string]any)["parts"].([]any)
	secondParts := filtered[1].(map[string]any)["parts"].([]any)
	if len(firstParts) != 1 || len(secondParts) != 2 {
		t.Fatalf("packed parts lengths=%d/%d, want 1/2", len(firstParts), len(secondParts))
	}

	firstParts = append(firstParts, map[string]any{"text": "appended"})
	if len(firstParts) != 2 || firstParts[1].(map[string]any)["text"] != "appended" {
		t.Fatalf("append to first packed slice failed: %#v", firstParts)
	}
	secondCall := secondParts[0].(map[string]any)["functionCall"].(map[string]any)
	if secondCall["name"] != "second" {
		t.Fatalf("appending first packed slice overwrote the second: %#v", secondParts)
	}
	originalCall := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	if originalCall["id"] != "call_1" {
		t.Fatalf("packed cleaning mutated input: %#v", originalCall)
	}
}

func TestDefaultRequestConverterKeepsGemini36PrefillMapShape(t *testing.T) {
	body := map[string]any{
		"model": "gemini-3.6-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "question"},
			map[string]any{"role": "assistant", "content": "prefix"},
		},
	}
	_, payload, err := DefaultRequestConverter().Convert(
		body,
		config.StaticProvider(config.DefaultConfig()),
	)
	if err != nil {
		t.Fatal(err)
	}
	contents := payload["contents"].([]any)
	if _, ok := contents[0].(map[string]any); !ok {
		t.Fatalf("Gemini 3.6 history should keep map shape, got %T", contents[0])
	}
	if got := AssistantPrefillFromPayload(payload); got != "prefix" {
		t.Fatalf("prefill=%q, want prefix", got)
	}
}

func TestConvertChatRequestRejectsSilentlyDroppedMessages(t *testing.T) {
	validToolCall := []any{map[string]any{
		"id": "call_1",
		"function": map[string]any{
			"name":      "lookup",
			"arguments": "{}",
		},
	}}
	tests := []struct {
		name     string
		messages []any
	}{
		{
			name:     "non-object message",
			messages: []any{"plain string"},
		},
		{
			name: "unsupported role",
			messages: []any{
				map[string]any{"role": "observer", "content": "hidden prompt"},
			},
		},
		{
			name: "unconvertible user content",
			messages: []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "unknown", "value": "hidden prompt"},
				}},
			},
		},
		{
			name: "partially unconvertible user content",
			messages: []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "visible prompt"},
					map[string]any{"type": "unknown", "value": "hidden prompt"},
				}},
			},
		},
		{
			name: "unconvertible system content",
			messages: []any{
				map[string]any{"role": "system", "content": []any{
					map[string]any{"type": "unknown", "value": "hidden prompt"},
				}},
			},
		},
		{
			name: "partially unconvertible system content",
			messages: []any{
				map[string]any{"role": "system", "content": []any{
					map[string]any{"type": "text", "text": "visible prompt"},
					map[string]any{"type": "unknown", "value": "hidden prompt"},
				}},
			},
		},
		{
			name: "unconvertible assistant content is not masked by tool calls",
			messages: []any{
				map[string]any{
					"role":       "assistant",
					"content":    map[string]any{"unexpected": "hidden prompt"},
					"tool_calls": validToolCall,
				},
			},
		},
		{
			name: "no system or valid contents",
			messages: []any{
				map[string]any{"role": "assistant", "content": nil},
			},
		},
		{
			name: "empty user is not valid content",
			messages: []any{
				map[string]any{"role": "user", "content": ""},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ConvertChatRequest(
				map[string]any{"model": "m", "messages": tc.messages},
				config.StaticProvider(config.DefaultConfig()),
			)
			if err == nil {
				t.Fatal("expected invalid message content to return an error")
			}
		})
	}
}

func TestConvertChatRequestAllowsAssistantToolCallWithoutText(t *testing.T) {
	body := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call_1", "function": map[string]any{
					"name": "lookup", "arguments": "{}",
				}},
			}},
		},
	}
	_, payload, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	if err != nil {
		t.Fatalf("assistant tool-call turn should remain supported: %v", err)
	}
	contents, _ := payload["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents len=%d, want 1", len(contents))
	}
}

func TestConvertChatRequestValidatesMaxTokenInteger(t *testing.T) {
	invalid := []any{
		float64(0),
		float64(-1),
		1.5,
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		"128",
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		for _, value := range invalid {
			t.Run(field, func(t *testing.T) {
				body := map[string]any{
					"model":    "m",
					"messages": []any{map[string]any{"role": "user", "content": "hi"}},
					field:      value,
				}
				if _, _, err := ConvertChatRequest(
					body,
					config.StaticProvider(config.DefaultConfig()),
				); err == nil {
					t.Fatalf("%s=%v should be rejected", field, value)
				}
			})
		}
	}

	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		body := map[string]any{
			"model":    "m",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			field:      float64(128),
		}
		if _, _, err := ConvertChatRequest(
			body,
			config.StaticProvider(config.DefaultConfig()),
		); err != nil {
			t.Fatalf("%s integer should remain valid: %v", field, err)
		}
	}
}

// ============ 多模态 content 转换 ============

func TestConvertUserContent_ImageDataURI(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "看图"},
		map[string]any{"type": "image_url", "image_url": map[string]any{
			"url": "data:image/png;base64,AAAA",
		}},
	}
	parts := convertUserContent(content)
	if len(parts) < 2 {
		t.Fatalf("parts len=%d, want at least 2", len(parts))
	}
	id, ok := parts[1].(map[string]any)["inlineData"].(map[string]any)
	if !ok {
		t.Fatalf("part[1] 不是 inlineData: %v", parts[1])
	}
	if id["mimeType"] != "image/png" || id["data"] != "AAAA" {
		t.Errorf("inlineData=%v", id)
	}
}

func TestConvertUserContent_ImageRemoteURL(t *testing.T) {
	content := []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{
			"url": "https://example.com/cat.jpeg",
		}},
	}
	parts := convertUserContent(content)
	if len(parts) < 1 {
		t.Fatalf("parts len=%d, want at least 1", len(parts))
	}
	fd, ok := parts[0].(map[string]any)["fileData"].(map[string]any)
	if !ok {
		t.Fatalf("远程 URL 应转 fileData: %v", parts[0])
	}
	if fd["mimeType"] != "image/jpeg" || fd["fileUri"] != "https://example.com/cat.jpeg" {
		t.Errorf("fileData=%v", fd)
	}
}

func TestConvertUserContent_Video(t *testing.T) {
	// data: URI 不带 video/* mime → 回退 video/mp4
	content := []any{
		map[string]any{"type": "video_url", "video_url": map[string]any{"url": "data:application/octet-stream;base64,QkJC"}},
	}
	parts := convertUserContent(content)
	if len(parts) < 1 {
		t.Fatalf("parts len=%d, want at least 1", len(parts))
	}
	id := parts[0].(map[string]any)["inlineData"].(map[string]any)
	if id["mimeType"] != "video/mp4" {
		t.Errorf("video mime=%v, want video/mp4 回退", id["mimeType"])
	}
	// input_video 字段名 + 显式 video mime
	content2 := []any{
		map[string]any{"type": "input_video", "input_video": "data:video/webm;base64,QkJC"},
	}
	parts2 := convertUserContent(content2)
	if len(parts2) < 1 {
		t.Fatalf("parts2 len=%d, want at least 1", len(parts2))
	}
	id2 := parts2[0].(map[string]any)["inlineData"].(map[string]any)
	if id2["mimeType"] != "video/webm" {
		t.Errorf("input_video mime=%v", id2["mimeType"])
	}
}

func TestConvertUserContent_InputAudio(t *testing.T) {
	// {data, format} 形态
	content := []any{
		map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "QUFB", "format": "mp3"}},
	}
	parts := convertUserContent(content)
	if len(parts) < 1 {
		t.Fatalf("parts len=%d, want at least 1", len(parts))
	}
	id := parts[0].(map[string]any)["inlineData"].(map[string]any)
	if id["mimeType"] != "audio/mpeg" {
		t.Errorf("audio mime=%v, want audio/mpeg", id["mimeType"])
	}
	// 未知 format → 回退 audio/wav
	content2 := []any{
		map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "QUFB", "format": "xyz"}},
	}
	parts2 := convertUserContent(content2)
	if len(parts2) < 1 {
		t.Fatalf("parts2 len=%d, want at least 1", len(parts2))
	}
	id2 := parts2[0].(map[string]any)["inlineData"].(map[string]any)
	if id2["mimeType"] != "audio/wav" {
		t.Errorf("未知 format mime=%v, want audio/wav 回退", id2["mimeType"])
	}
	// data: URI 形态
	content3 := []any{
		map[string]any{"type": "input_audio", "input_audio": "data:audio/flac;base64,QUFB"},
	}
	parts3 := convertUserContent(content3)
	if len(parts3) < 1 {
		t.Fatalf("parts3 len=%d, want at least 1", len(parts3))
	}
	id3 := parts3[0].(map[string]any)["inlineData"].(map[string]any)
	if id3["mimeType"] != "audio/flac" {
		t.Errorf("audio data URI mime=%v", id3["mimeType"])
	}
}

// ============ 工具调用：声明 + tool_choice ============

func TestConvertChatRequest_Tools(t *testing.T) {
	body := map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "天气?"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "查天气",
				"parameters": map[string]any{
					"type":                 "object",
					"$schema":              "http://json-schema.org/draft-07/schema#", // 应被白名单剔除
					"additionalProperties": false,                                     // 应被剔除
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
					},
					"required": []any{"city"},
				},
			},
		}},
		"tool_choice": "required",
	}
	_, payload, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	tools := payload["tools"].([]any)
	fd := tools[0].(map[string]any)["functionDeclarations"].([]any)
	decl := fd[0].(map[string]any)
	if decl["name"] != "get_weather" {
		t.Errorf("name=%v", decl["name"])
	}
	params := decl["parameters"].(map[string]any)
	if _, ok := params["$schema"]; ok {
		t.Error("$schema 应被白名单剔除")
	}
	if _, ok := params["additionalProperties"]; ok {
		t.Error("additionalProperties 应被剔除")
	}
	if _, ok := params["properties"]; !ok {
		t.Error("properties 应保留")
	}
	originalParams := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if _, ok := originalParams["$schema"]; !ok {
		t.Fatal("schema 清洗修改了客户端原始请求")
	}
	originalCity := originalParams["properties"].(map[string]any)["city"].(map[string]any)
	if originalCity["type"] != "string" {
		t.Fatalf("schema 清洗修改了客户端嵌套字段: %#v", originalCity)
	}
	tc := payload["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
	if tc["mode"] != "ANY" {
		t.Errorf("required → mode=%v, want ANY", tc["mode"])
	}
}

func TestConvertChatRequest_ToolChoiceRequiredNoTools(t *testing.T) {
	body := map[string]any{
		"model":       "m",
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"tool_choice": "required",
	}
	if _, _, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig())); err == nil {
		t.Error("required 无工具应报错")
	}
}

func TestConvertChatRequest_ToolChoiceUnknownFunc(t *testing.T) {
	body := map[string]any{
		"model":       "m",
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":       []any{map[string]any{"type": "function", "function": map[string]any{"name": "a"}}},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "b"}},
	}
	if _, _, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig())); err == nil {
		t.Error("引用未声明函数应报错")
	}
}

func TestConvertChatRequest_LegacyFunctions(t *testing.T) {
	// 顶层 functions（已废弃）+ function_call。
	body := map[string]any{
		"model":         "m",
		"messages":      []any{map[string]any{"role": "user", "content": "hi"}},
		"functions":     []any{map[string]any{"name": "f1", "description": "d"}},
		"function_call": "auto",
	}
	_, payload, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["tools"]; !ok {
		t.Error("legacy functions 应转成 tools")
	}
	if payload["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"] != "AUTO" {
		t.Error("legacy function_call=auto → AUTO")
	}
}

// ============ 工具调用：id 锚点 + 哨兵（多轮 round-trip） ============

func TestFunctionCallNameIndexInlineAndOverflow(t *testing.T) {
	var index functionCallNameIndex
	for _, entry := range []functionCallNameEntry{
		{id: "call_1", name: "one"},
		{id: "call_2", name: "two"},
		{id: "call_3", name: "three"},
		{id: "call_4", name: "four"},
		{id: "call_5", name: "five"},
		{id: "call_6", name: "six"},
		{id: "call_7", name: "seven"},
		{id: "call_8", name: "eight"},
	} {
		index.Set(entry.id, entry.name)
	}
	index.Set("call_2", "two-updated")
	if index.overflow != nil || index.Get("call_2") != "two-updated" {
		t.Fatalf("inline index changed unexpectedly: %#v", index)
	}

	index.Set("call_9", "nine")
	index.Set("call_10", "ten")
	index.Set("call_1", "one-updated")
	if index.overflow == nil {
		t.Fatal("ninth unique ID did not promote the index")
	}
	for id, want := range map[string]string{
		"call_1":  "one-updated",
		"call_2":  "two-updated",
		"call_8":  "eight",
		"call_9":  "nine",
		"call_10": "ten",
	} {
		if got := index.Get(id); got != want {
			t.Fatalf("index.Get(%q)=%q, want %q", id, got, want)
		}
	}
	if got := index.Get("missing"); got != "" {
		t.Fatalf("missing ID resolved to %q", got)
	}
}

func TestCleanPartWithIDSingleToolParts(t *testing.T) {
	call := map[string]any{
		"functionCall": map[string]any{
			"id":   "call_1",
			"name": "lookup",
			"args": `{"city":"深圳"}`,
		},
	}
	cleanedCall, ok := cleanPartWithID(call, nil, -1, nil)
	if !ok {
		t.Fatal("single functionCall should remain valid")
	}
	functionCall := cleanedCall["functionCall"].(map[string]any)
	if _, exists := functionCall["id"]; exists {
		t.Fatalf("cleaned functionCall retained id: %#v", functionCall)
	}
	if _, ok := functionCall["args"].(map[string]any); !ok {
		t.Fatalf("cleaned functionCall args were not decoded: %#v", functionCall)
	}
	if cleanedCall["thoughtSignature"] != encodedSkipThoughtSentinel {
		t.Fatalf("thoughtSignature=%v, want %v", cleanedCall["thoughtSignature"], encodedSkipThoughtSentinel)
	}
	if call["functionCall"].(map[string]any)["id"] != "call_1" {
		t.Fatalf("cleaning mutated input functionCall: %#v", call)
	}

	response := map[string]any{
		"functionResponse": map[string]any{
			"id":       "call_1",
			"response": "sunny",
		},
	}
	var callIDIndex functionCallNameIndex
	callIDIndex.Set("call_1", "lookup")
	cleanedResponse, ok := cleanPartWithID(
		response,
		[]string{"fallback"},
		0,
		&callIDIndex,
	)
	if !ok {
		t.Fatal("single functionResponse should remain valid")
	}
	functionResponse := cleanedResponse["functionResponse"].(map[string]any)
	if functionResponse["name"] != "lookup" {
		t.Fatalf("functionResponse.name=%v, want lookup", functionResponse["name"])
	}
	if _, exists := functionResponse["id"]; exists {
		t.Fatalf("cleaned functionResponse retained id: %#v", functionResponse)
	}
	body, ok := functionResponse["response"].(map[string]any)
	if !ok || body["result"] != "sunny" {
		t.Fatalf("functionResponse.response=%#v, want wrapped sunny result", functionResponse["response"])
	}
	if response["functionResponse"].(map[string]any)["id"] != "call_1" {
		t.Fatalf("cleaning mutated input functionResponse: %#v", response)
	}
}

func TestToolCallRoundTrip_IDAnchor(t *testing.T) {
	// 模拟多轮：user → assistant(tool_calls) → tool(结果) → 走完整 BuildVertexVariables 管线。
	body := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "北京和上海天气?"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call_BJ", "type": "function", "function": map[string]any{
					"name": "get_weather", "arguments": `{"city":"北京"}`,
				}},
				map[string]any{"id": "call_SH", "type": "function", "function": map[string]any{
					"name": "get_weather", "arguments": `{"city":"上海"}`,
				}},
			}},
			// 故意乱序回传 + 缺 name，验证按 id 反查精确配对（不会错配）。
			map[string]any{"role": "tool", "tool_call_id": "call_SH", "content": `{"temp":"20C"}`},
			map[string]any{"role": "tool", "tool_call_id": "call_BJ", "content": `{"temp":"5C"}`},
		},
	}
	model, gemini, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	vars := BuildVertexVariables(model, gemini, config.StaticProvider(config.DefaultConfig()))
	contents := vars["contents"].([]any)

	// 找到 model 消息：两个 functionCall 都应带 base64 编码的 thoughtSignature 哨兵、不带 id。
	var modelParts []any
	var funcParts []any
	for _, c := range contents {
		cm := c.(map[string]any)
		switch cm["role"] {
		case "model":
			modelParts = cm["parts"].([]any)
		case "function":
			funcParts = append(funcParts, cm["parts"].([]any)...)
		}
	}
	if len(modelParts) != 2 {
		t.Fatalf("model parts=%d, want 2", len(modelParts))
	}
	wantSig := base64.StdEncoding.EncodeToString([]byte(skipThoughtSentinel))
	for _, p := range modelParts {
		pm := p.(map[string]any)
		fc := pm["functionCall"].(map[string]any)
		if _, hasID := fc["id"]; hasID {
			t.Error("functionCall 不应残留内部 id 锚点")
		}
		// args 应被解析为对象
		if _, ok := fc["args"].(map[string]any); !ok {
			t.Errorf("args 应是对象: %v", fc["args"])
		}
		if pm["thoughtSignature"] != wantSig {
			t.Errorf("哨兵 thoughtSignature=%v, want %v", pm["thoughtSignature"], wantSig)
		}
	}

	// functionResponse：按 id 反查 name。call_SH→get_weather；乱序也不错配。
	if len(funcParts) != 2 {
		t.Fatalf("function parts=%d, want 2", len(funcParts))
	}
	for _, p := range funcParts {
		fr := p.(map[string]any)["functionResponse"].(map[string]any)
		if fr["name"] != "get_weather" {
			t.Errorf("functionResponse.name=%v, want get_weather（id 反查）", fr["name"])
		}
		if _, hasID := fr["id"]; hasID {
			t.Error("functionResponse 不应残留内部 id 锚点")
		}
		if _, ok := fr["response"].(map[string]any); !ok {
			t.Errorf("response 应是对象: %v", fr["response"])
		}
	}
}

func TestToolCallNameResolution_PositionalFallback(t *testing.T) {
	// 无 id 时按位置兜底：两个 functionResponse 按出现顺序配 [fa, fb]。
	body := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "x"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"function": map[string]any{"name": "fa", "arguments": "{}"}},
				map[string]any{"function": map[string]any{"name": "fb", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "content": "r1"}, // 无 tool_call_id
			map[string]any{"role": "tool", "content": "r2"},
		},
	}
	model, gemini, _ := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	vars := BuildVertexVariables(model, gemini, config.StaticProvider(config.DefaultConfig()))
	var names []string
	for _, c := range vars["contents"].([]any) {
		cm := c.(map[string]any)
		if cm["role"] == "function" {
			for _, p := range cm["parts"].([]any) {
				fr := p.(map[string]any)["functionResponse"].(map[string]any)
				names = append(names, fr["name"].(string))
			}
		}
	}
	if len(names) != 2 || names[0] != "fa" || names[1] != "fb" {
		t.Errorf("位置兜底 names=%v, want [fa fb]", names)
	}
}

func TestEmptyToolCallRejected(t *testing.T) {
	// 空 name 的 tool_call 不能静默消失，否则后续 tool_result 会与历史错位。
	body := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "x"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "", "arguments": "{}"}},
			}},
		},
	}
	if _, _, err := ConvertChatRequest(
		body,
		config.StaticProvider(config.DefaultConfig()),
	); err == nil {
		t.Fatal("空 name tool_call 必须返回参数错误")
	}
}

func TestConvertChatRequestRejectsInvalidControls(t *testing.T) {
	tests := []struct {
		name   string
		update func(map[string]any)
	}{
		{
			name: "non-array tools",
			update: func(body map[string]any) {
				body["tools"] = "drop-me"
			},
		},
		{
			name: "malformed tool",
			update: func(body map[string]any) {
				body["tools"] = []any{
					map[string]any{"type": "function", "function": map[string]any{}},
				}
			},
		},
		{
			name: "unknown tool choice",
			update: func(body map[string]any) {
				body["tool_choice"] = "sometimes"
			},
		},
		{
			name: "mixed stop array",
			update: func(body map[string]any) {
				body["stop"] = []any{"valid", float64(42)}
			},
		},
		{
			name: "non-object response format",
			update: func(body map[string]any) {
				body["response_format"] = "json"
			},
		},
		{
			name: "json schema missing schema",
			update: func(body map[string]any) {
				body["response_format"] = map[string]any{
					"type":        "json_schema",
					"json_schema": map[string]any{"name": "answer"},
				}
			},
		},
		{
			name: "unknown response format",
			update: func(body map[string]any) {
				body["response_format"] = map[string]any{"type": "future_format"}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := map[string]any{
				"model":    "m",
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}
			test.update(body)
			if _, _, err := ConvertChatRequest(
				body,
				config.StaticProvider(config.DefaultConfig()),
			); err == nil {
				t.Fatal("无效控制字段必须返回参数错误")
			}
		})
	}
}

// ============ chat 参数映射 ============

func TestReasoningEffort(t *testing.T) {
	for effort, wantLevel := range map[string]string{
		"minimal": "MINIMAL", "low": "LOW", "medium": "MEDIUM", "high": "HIGH",
		"none": "NONE", "xhigh": "HIGH",
	} {
		body := map[string]any{
			"model":            "m",
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning_effort": effort,
		}
		_, payload, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
		if err != nil {
			t.Fatal(err)
		}
		gc := payload["generationConfig"].(map[string]any)
		tc := gc["thinkingConfig"].(map[string]any)
		if tc["thinkingLevel"] != wantLevel {
			t.Errorf("reasoning_effort=%q → thinkingLevel=%v, want %v", effort, tc["thinkingLevel"], wantLevel)
		}
	}
}

func TestMediaResolution(t *testing.T) {
	for in, want := range map[string]string{
		"high":                    "MEDIA_RESOLUTION_HIGH",
		"low":                     "MEDIA_RESOLUTION_LOW",
		"MEDIA_RESOLUTION_MEDIUM": "MEDIA_RESOLUTION_MEDIUM",
	} {
		body := map[string]any{
			"model":            "m",
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"media_resolution": in,
		}
		_, payload, _ := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
		gc := payload["generationConfig"].(map[string]any)
		if gc["mediaResolution"] != want {
			t.Errorf("media_resolution=%q → %v, want %v", in, gc["mediaResolution"], want)
		}
	}
	// 嵌在 extra_body
	body := map[string]any{
		"model":      "m",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"extra_body": map[string]any{"media_resolution": "low"},
	}
	_, payload, _ := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	if payload["generationConfig"].(map[string]any)["mediaResolution"] != "MEDIA_RESOLUTION_LOW" {
		t.Error("extra_body.media_resolution 未生效")
	}
}

func TestLogprobsAndSampling(t *testing.T) {
	body := map[string]any{
		"model":             "m",
		"messages":          []any{map[string]any{"role": "user", "content": "hi"}},
		"temperature":       0.5,
		"top_p":             0.9,
		"top_k":             float64(40),
		"seed":              float64(123),
		"logprobs":          true,
		"top_logprobs":      float64(5),
		"presence_penalty":  0.1,
		"frequency_penalty": 0.2,
	}
	_, payload, _ := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	gc := payload["generationConfig"].(map[string]any)
	checks := map[string]any{
		"temperature": 0.5, "topP": 0.9, "topK": float64(40), "seed": float64(123),
		"responseLogprobs": true, "logprobs": float64(5),
		"presencePenalty": 0.1, "frequencyPenalty": 0.2,
	}
	for k, want := range checks {
		if gc[k] != want {
			t.Errorf("genCfg[%q]=%v, want %v", k, gc[k], want)
		}
	}
}

func TestTopKClamp(t *testing.T) {
	// topK > 63 应在 BuildVertexVariables 的 generationConfig 转换里被 clamp 到 63。
	payload := map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}},
		"generationConfig": map[string]any{"topK": float64(100)},
	}
	vars := BuildVertexVariables("m", payload, config.StaticProvider(config.DefaultConfig()))
	gc := vars["generationConfig"].(map[string]any)
	if gc["topK"] != 63 {
		t.Errorf("topK=%v, want clamp 到 63", gc["topK"])
	}

	payload["generationConfig"] = map[string]any{"topK": float64(1.5)}
	vars = BuildVertexVariables("m", payload, config.StaticProvider(config.DefaultConfig()))
	gc = vars["generationConfig"].(map[string]any)
	if gc["topK"] != float64(1.5) {
		t.Errorf("fractional topK was silently truncated: %v", gc["topK"])
	}
}

func TestGemini36DropsDeprecatedGenerationFields(t *testing.T) {
	payload := map[string]any{
		"contents": []any{map[string]any{
			"role": "user", "parts": []any{map[string]any{"text": "hi"}},
		}},
		"generationConfig": map[string]any{
			"temperature":     0.7,
			"topP":            0.9,
			"topK":            40,
			"candidateCount":  2,
			"maxOutputTokens": 1024,
			"thinkingConfig": map[string]any{
				"thinkingLevel": "medium", "thinkingBudget": 256,
			},
		},
	}
	vars := BuildVertexVariables(
		"gemini-3.6-flash",
		payload,
		config.StaticProvider(config.DefaultConfig()),
	)
	gc := vars["generationConfig"].(map[string]any)
	for _, key := range []string{"temperature", "topP", "topK", "candidateCount"} {
		if _, exists := gc[key]; exists {
			t.Errorf("Gemini 3.6 不应继续发送已弃用字段 %s: %v", key, gc)
		}
	}
	if gc["maxOutputTokens"] != 1024 {
		t.Fatalf("兼容清理不应移除仍受支持的字段: %v", gc)
	}
	thinking := gc["thinkingConfig"].(map[string]any)
	if _, exists := thinking["thinkingBudget"]; exists {
		t.Fatalf("Gemini 3.6 不应发送 thinkingBudget: %v", thinking)
	}
	if thinking["thinkingLevel"] != "MEDIUM" {
		t.Fatalf("thinkingLevel 应保留并规范为大写: %v", thinking)
	}
	originalThinking := payload["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if originalThinking["thinkingBudget"] != 256 || originalThinking["thinkingLevel"] != "medium" {
		t.Fatalf("出站兼容清理不应修改原始 payload: %v", originalThinking)
	}
}

func TestGemini36NormalizesUnsupportedThinkingControls(t *testing.T) {
	tests := []struct {
		name     string
		thinking map[string]any
		want     string
	}{
		{name: "none level", thinking: map[string]any{"thinkingLevel": "NONE"}, want: "MINIMAL"},
		{name: "disabled level", thinking: map[string]any{"thinkingLevel": "disabled"}, want: "MINIMAL"},
		{name: "zero budget", thinking: map[string]any{"thinkingBudget": float64(0)}, want: "MINIMAL"},
		{name: "positive budget", thinking: map[string]any{"thinkingBudget": float64(1024)}, want: "MEDIUM"},
		{name: "explicit level wins", thinking: map[string]any{"thinkingLevel": "high", "thinkingBudget": float64(0)}, want: "HIGH"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{
				"contents": []any{map[string]any{
					"role": "user", "parts": []any{map[string]any{"text": "hi"}},
				}},
				"generationConfig": map[string]any{"thinkingConfig": tc.thinking},
			}
			vars := BuildVertexVariables(
				"gemini-3.6-flash",
				payload,
				config.StaticProvider(config.DefaultConfig()),
			)
			thinking := vars["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
			if thinking["thinkingLevel"] != tc.want {
				t.Fatalf("thinkingLevel=%v, want %s", thinking["thinkingLevel"], tc.want)
			}
			if _, exists := thinking["thinkingBudget"]; exists {
				t.Fatalf("Gemini 3.6 不应发送 thinkingBudget: %v", thinking)
			}
		})
	}
}

func TestGemini36ReasoningNoneBecomesMinimalAtOutboundLayer(t *testing.T) {
	body := map[string]any{
		"model": "gemini-3.6-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"reasoning_effort": "none",
	}
	model, payload, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	vars := BuildVertexVariables(model, payload, config.StaticProvider(config.DefaultConfig()))
	thinking := vars["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if thinking["thinkingLevel"] != "MINIMAL" {
		t.Fatalf("Gemini 3.6 reasoning_effort=none 应降级为 MINIMAL: %v", thinking)
	}
}

func TestParallelToolCalls_GracefullyAccepted(t *testing.T) {
	// parallel_tool_calls 应被优雅接受、不报错、不影响 payload。
	body := map[string]any{
		"model":               "m",
		"messages":            []any{map[string]any{"role": "user", "content": "hi"}},
		"parallel_tool_calls": false,
	}
	if _, _, err := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig())); err != nil {
		t.Errorf("parallel_tool_calls 不应报错: %v", err)
	}
}

func TestSafetySettingsPassthrough(t *testing.T) {
	custom := []any{map[string]any{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_LOW_AND_ABOVE"}}
	body := map[string]any{
		"model":           "m",
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"safety_settings": custom,
	}
	model, gemini, _ := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	vars := BuildVertexVariables(model, gemini, config.StaticProvider(config.DefaultConfig()))
	ss := vars["safetySettings"].([]vertexSafetySetting)
	if len(ss) != 1 || ss[0].Threshold != "BLOCK_LOW_AND_ABOVE" {
		t.Errorf("自定义 safety_settings 应透传、不被默认覆盖: %v", ss)
	}
}

// ============ usage 聚合 ============

func TestConvertUsage_Detailed(t *testing.T) {
	meta := map[string]any{
		"promptTokenCount":        float64(10),
		"toolUsePromptTokenCount": float64(2),
		"candidatesTokenCount":    float64(20),
		"thoughtsTokenCount":      float64(5),
		"totalTokenCount":         float64(37),
		"cachedContentTokenCount": float64(3),
		"promptTokensDetails":     []any{map[string]any{"modality": "AUDIO", "tokenCount": float64(4)}},
		"candidatesTokensDetails": []any{map[string]any{"modality": "IMAGE", "tokenCount": float64(6)}},
	}
	u := ConvertUsage(meta)
	if u["prompt_tokens"] != 12 { // 10 + 2 toolUse
		t.Errorf("prompt_tokens=%v, want 12", u["prompt_tokens"])
	}
	if u["completion_tokens"] != 25 { // 20 + 5 thoughts
		t.Errorf("completion_tokens=%v, want 25", u["completion_tokens"])
	}
	if u["total_tokens"] != 37 {
		t.Errorf("total_tokens=%v, want 37", u["total_tokens"])
	}
	pd := u["prompt_tokens_details"].(map[string]any)
	if pd["cached_tokens"] != 3 || pd["audio_tokens"] != 4 {
		t.Errorf("prompt_tokens_details=%v", pd)
	}
	cd := u["completion_tokens_details"].(map[string]any)
	if cd["reasoning_tokens"] != 5 || cd["image_tokens"] != 6 {
		t.Errorf("completion_tokens_details=%v", cd)
	}
}

func TestNormalizeUsageForCandidateIsAllocationFree(t *testing.T) {
	meta := map[string]any{
		"promptTokenCount":        float64(10),
		"toolUsePromptTokenCount": float64(2),
		"candidatesTokenCount":    float64(20),
		"thoughtsTokenCount":      float64(5),
		"totalTokenCount":         float64(37),
		"cachedContentTokenCount": float64(3),
		"promptTokensDetails": []any{
			map[string]any{"modality": "audio", "tokenCount": float64(4)},
		},
		"candidatesTokensDetails": []any{
			map[string]any{"modality": "image", "tokenCount": float64(6)},
		},
	}
	want := NormalizedUsage{
		PromptTokens:          12,
		CompletionTokens:      25,
		TotalTokens:           37,
		CachedInputTokens:     3,
		PromptAudioTokens:     4,
		ReasoningTokens:       5,
		CompletionImageTokens: 6,
	}
	if got := NormalizeUsageForCandidate(meta, nil); got != want {
		t.Fatalf("NormalizeUsageForCandidate() = %#v, want %#v", got, want)
	}
	if allocations := testing.AllocsPerRun(100, func() {
		if got := NormalizeUsageForCandidate(meta, nil); got != want {
			t.Fatalf("NormalizeUsageForCandidate() = %#v, want %#v", got, want)
		}
	}); allocations != 0 {
		t.Fatalf("NormalizeUsageForCandidate allocated %.1f times", allocations)
	}
}

func TestConvertUsage_DetailOnlyAndStringCounts(t *testing.T) {
	meta := map[string]any{
		"totalTokenCount": "84",
		"promptTokensDetails": []any{
			map[string]any{"modality": "text", "tokenCount": "76"},
		},
		"candidates_tokens_details": []any{
			map[string]any{"modality": "TEXT", "tokens": float64(8)},
		},
	}
	usage := ConvertUsage(meta)
	if usage["prompt_tokens"] != 76 || usage["completion_tokens"] != 8 || usage["total_tokens"] != 84 {
		t.Fatalf("detail-only usage 未正确归一化: %#v", usage)
	}
}

func TestNormalizeUsageRejectsInvalidAndOverflowingCounts(t *testing.T) {
	for _, value := range []any{
		float64(1.5),
		float64(-1),
		math.NaN(),
		math.Inf(1),
		math.MaxFloat64,
		float64(math.MaxInt) + 1,
		int64(-1),
		"-1",
	} {
		usage := NormalizeUsageForCandidate(
			map[string]any{"promptTokenCount": value},
			nil,
		)
		if usage.PromptTokens != 0 || usage.TotalTokens != 0 {
			t.Errorf("invalid count %v produced usage %+v", value, usage)
		}
	}

	usage := NormalizeUsageForCandidate(map[string]any{
		"promptTokenCount":        math.MaxInt,
		"toolUsePromptTokenCount": 1,
	}, nil)
	if usage.PromptTokens != 0 || usage.TotalTokens != 0 {
		t.Fatalf("overflowing usage was not discarded: %+v", usage)
	}

	usage = NormalizeUsageForCandidate(map[string]any{
		"promptTokensDetails": []any{
			map[string]any{"tokenCount": math.MaxInt},
			map[string]any{"tokenCount": 1},
			map[string]any{"tokenCount": 5},
		},
	}, nil)
	if usage.PromptTokens != 0 || usage.TotalTokens != 0 {
		t.Fatalf("overflowing usage details were not discarded: %+v", usage)
	}
}

func TestConvertUsage_TotalAndCandidateFallback(t *testing.T) {
	usage := ConvertUsageForCandidate(
		map[string]any{"totalTokenCount": float64(84)},
		map[string]any{"tokenCount": "8"},
	)
	if usage["prompt_tokens"] != 76 || usage["completion_tokens"] != 8 || usage["total_tokens"] != 84 {
		t.Fatalf("total + candidate fallback 未正确拆分: %#v", usage)
	}
}

func TestGeminiResponsesToOAIJSON_NAggregation(t *testing.T) {
	mk := func(text string, prompt, completion int) map[string]any {
		return map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": text}}, "role": "model"},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount":     float64(prompt),
				"candidatesTokenCount": float64(completion),
				"totalTokenCount":      float64(prompt + completion),
			},
		}
	}
	resps := []map[string]any{mk("A", 5, 3), mk("B", 5, 4)}
	out := GeminiResponsesToOAIJSON(resps, "m")
	choices := out["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("choices=%d, want 2", len(choices))
	}
	if choices[0].(map[string]any)["index"] != 0 || choices[1].(map[string]any)["index"] != 1 {
		t.Error("choice index 应 0,1 递增")
	}
	if choices[1].(map[string]any)["message"].(map[string]any)["content"] != "B" {
		t.Error("第二个 choice 内容错")
	}
	u := out["usage"].(map[string]any)
	if u["prompt_tokens"] != 10 || u["completion_tokens"] != 7 || u["total_tokens"] != 17 {
		t.Errorf("聚合 usage=%v, want prompt10/completion7/total17", u)
	}
}

// ============ 响应：图像 inlineData + 代码块 ============

func TestExtractParts_ImageAndCode(t *testing.T) {
	parts := []any{
		map[string]any{"text": "结果:"},
		map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "XXXX"}},
		map[string]any{"executableCode": map[string]any{"codeLanguage": "PYTHON", "code": "print(1)"}},
		map[string]any{"codeExecutionResult": map[string]any{"output": "1"}},
	}
	text, tools, _ := ExtractParts(parts, false)
	if tools != nil {
		t.Error("无工具调用")
	}
	if !strings.Contains(text, "![image](data:image/png;base64,XXXX)") {
		t.Errorf("图像 markdown 缺失: %q", text)
	}
	if !strings.Contains(text, "```python\nprint(1)\n```") {
		t.Errorf("代码块缺失: %q", text)
	}
	if !strings.Contains(text, "```output\n1\n```") {
		t.Errorf("output 块缺失: %q", text)
	}
}

// ============ 图像分辨率 ApplyImageConfig ============

func TestApplyImageConfig(t *testing.T) {
	// image_size 档位
	gp := map[string]any{}
	ApplyImageConfig(gp, map[string]any{"image_size": "2K"})
	if gp["generationConfig"].(map[string]any)["imageConfig"].(map[string]any)["imageSize"] != "2K" {
		t.Error("image_size=2K 未写入")
	}
	// 像素 → 档位
	gp2 := map[string]any{}
	ApplyImageConfig(gp2, map[string]any{"size": "2048x2048"})
	if gp2["generationConfig"].(map[string]any)["imageConfig"].(map[string]any)["imageSize"] != "2K" {
		t.Error("2048px 应映射到 2K")
	}
	// imageConfig 顶层透传
	gp3 := map[string]any{}
	ApplyImageConfig(gp3, map[string]any{"imageConfig": map[string]any{"aspectRatio": "16:9"}})
	if gp3["generationConfig"].(map[string]any)["imageConfig"].(map[string]any)["aspectRatio"] != "16:9" {
		t.Error("imageConfig 透传失败")
	}
	// 不命中：不动 payload
	gp4 := map[string]any{}
	ApplyImageConfig(gp4, map[string]any{})
	if len(gp4) != 0 {
		t.Errorf("无分辨率参数时不应改 payload: %v", gp4)
	}
}

// ============ 工具 schema → Vertex 原生格式 ============

func TestToNativeSchema(t *testing.T) {
	// 标准 JSON Schema → 原生：type 大写、properties 转 [{key,value}]、剥离 $schema。
	std := map[string]any{
		"type":    "object",
		"$schema": "http://json-schema.org/draft-07/schema#",
		"properties": map[string]any{
			"city": map[string]any{"type": "string", "description": "城市"},
		},
		"required": []any{"city"},
	}
	native := toNativeSchema(std).(map[string]any)
	if native["type"] != "OBJECT" {
		t.Errorf("type=%v, want OBJECT（大写）", native["type"])
	}
	if _, ok := native["$schema"]; ok {
		t.Error("$schema 应被剥离")
	}
	props, ok := native["properties"].([]any)
	if !ok || len(props) != 1 {
		t.Fatalf("properties 应转为 [{key,value}] 列表: %v", native["properties"])
	}
	p0 := props[0].(map[string]any)
	if p0["key"] != "city" {
		t.Errorf("property key=%v", p0["key"])
	}
	if p0["value"].(map[string]any)["type"] != "STRING" {
		t.Errorf("嵌套 type 应大写: %v", p0["value"])
	}
}

func TestCleanNativeFunctionParametersFusesCleaningWithoutMutatingInput(t *testing.T) {
	schema := map[string]any{
		"type":  []any{"null", "array"},
		"title": "removed",
		"anyOf": []any{map[string]any{"type": "string"}},
		"items": map[string]any{
			"type":      "object",
			"maxLength": float64(8),
			"properties": map[string]any{
				"enabled": map[string]any{"type": "boolean", "$ref": "removed"},
				"label":   map[string]any{"type": "string", "description": "display name"},
			},
		},
	}
	before, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	native := cleanNativeFunctionParameters(schema).(map[string]any)
	after, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("fused schema conversion mutated input:\nbefore=%s\nafter=%s", before, after)
	}
	if native["type"] != "ARRAY" || native["title"] != nil || native["anyOf"] != nil {
		t.Fatalf("top-level native schema=%#v", native)
	}
	items := native["items"].(map[string]any)
	if items["type"] != "OBJECT" || items["maxLength"] != "8" {
		t.Fatalf("native items=%#v", items)
	}
	properties := items["properties"].(nativeSchemaProperties)
	if len(properties) != 2 {
		t.Fatalf("native properties=%#v", properties)
	}
	propertiesByKey := make(map[string]any, len(properties))
	for _, property := range properties {
		propertiesByKey[property.Key] = property.Value
	}
	enabled, ok := propertiesByKey["enabled"].(*nativeTypeOnlySchema)
	if !ok || enabled.Type != "BOOLEAN" {
		t.Fatalf("native enabled property=%#v", propertiesByKey["enabled"])
	}
	label, ok := propertiesByKey["label"].(*nativeDescriptionSchema)
	if !ok || label.Type != "STRING" || label.Description != "display name" {
		t.Fatalf("native label property=%#v", propertiesByKey["label"])
	}
	encodedProperties, err := json.Marshal(properties)
	if err != nil {
		t.Fatal(err)
	}
	var genericProperties []map[string]any
	if err := json.Unmarshal(encodedProperties, &genericProperties); err != nil {
		t.Fatal(err)
	}
	if len(genericProperties) != 2 {
		t.Fatalf("紧凑属性切片的 JSON 结构不兼容: %s", encodedProperties)
	}
}

func TestCompactNativeSchemaLeafPreservesCleanedJSON(t *testing.T) {
	tests := []struct {
		name   string
		schema map[string]any
		want   string
	}{
		{
			name:   "type only with unsupported field",
			schema: map[string]any{"type": "string", "$ref": "#/$defs/value"},
			want:   `{"type":"STRING"}`,
		},
		{
			name:   "missing type defaults to object",
			schema: map[string]any{},
			want:   `{"type":"OBJECT"}`,
		},
		{
			name:   "empty description remains present",
			schema: map[string]any{"description": "", "type": "boolean"},
			want:   `{"description":"","type":"BOOLEAN"}`,
		},
		{
			name:   "nil enum remains present",
			schema: map[string]any{"enum": nil, "type": "string"},
			want:   `{"enum":null,"type":"STRING"}`,
		},
		{
			name: "description and enum",
			schema: map[string]any{
				"description": "mode", "enum": []any{"fast", "safe"}, "type": "string",
			},
			want: `{"description":"mode","enum":["fast","safe"],"type":"STRING"}`,
		},
		{
			name:   "zero default remains present",
			schema: map[string]any{"default": float64(0), "type": "integer"},
			want:   `{"default":0,"type":"INTEGER"}`,
		},
		{
			name: "false default remains present",
			schema: map[string]any{
				"default": false, "description": "enabled", "type": "boolean",
			},
			want: `{"default":false,"description":"enabled","type":"BOOLEAN"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before, err := json.Marshal(test.schema)
			if err != nil {
				t.Fatal(err)
			}
			compact, ok := compactNativeSchemaLeaf(test.schema)
			if !ok {
				t.Fatal("common schema leaf did not use compact representation")
			}
			encoded, err := json.Marshal(compact)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != test.want {
				t.Fatalf("compact leaf=%s, want %s", encoded, test.want)
			}
			after, err := json.Marshal(test.schema)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("compact conversion mutated input:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
	if compact, ok := compactNativeSchemaLeaf(map[string]any{
		"default": "fast", "enum": []any{"fast", "safe"}, "type": "string",
	}); ok || compact != nil {
		t.Fatalf("unsupported compact field combination should use general path: %#v", compact)
	}
}

func TestCanonicalNativeToolsPassThroughOnlyNativeFunctionDeclarations(t *testing.T) {
	native := []any{map[string]any{"functionDeclarations": []any{map[string]any{
		"name": "lookup",
		"parameters": map[string]any{
			"type": "OBJECT",
			"properties": []any{map[string]any{
				"key": "query", "value": map[string]any{"type": "STRING"},
			}},
		},
	}}}}
	got, ok := canonicalNativeTools(native)
	if !ok || len(got) != 1 || &got[0] != &native[0] {
		t.Fatalf("canonical native tools did not pass through: ok=%v got=%#v", ok, got)
	}

	standard := []any{map[string]any{"functionDeclarations": []any{map[string]any{
		"name": "lookup",
		"parameters": map[string]any{
			"type": "object", "properties": map[string]any{},
		},
	}}}}
	if got, ok := canonicalNativeTools(standard); ok || got != nil {
		t.Fatalf("standard schema unexpectedly passed native fast path: %#v", got)
	}
	compact := []any{map[string]any{"functionDeclarations": []any{map[string]any{
		"name":       "lookup",
		"parameters": nativeTypeOnlySchema{Type: "OBJECT"},
	}}}}
	if got, ok := canonicalNativeTools(compact); !ok || len(got) != 1 || &got[0] != &compact[0] {
		t.Fatalf("compact native schema did not pass through: ok=%v got=%#v", ok, got)
	}
	withExtraField := []any{map[string]any{"functionDeclarations": []any{map[string]any{
		"name": "lookup", "parameters": map[string]any{"type": "OBJECT"}, "unexpected": true,
	}}}}
	if got, ok := canonicalNativeTools(withExtraField); ok || got != nil {
		t.Fatalf("extended declaration unexpectedly passed native fast path: %#v", got)
	}
}

func TestConvertToolsFormat_NativeParameters(t *testing.T) {
	// 经 BuildVertexVariables 的 tools 归一后，parameters 应是原生格式（type 大写、properties 列表）。
	body := map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "f", "parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"x": map[string]any{"type": "integer"}},
			},
		}}},
	}
	model, gemini, _ := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	vars := BuildVertexVariables(model, gemini, config.StaticProvider(config.DefaultConfig()))
	tools := vars["tools"].([]any)
	decl := tools[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	params := decl["parameters"].(map[string]any)
	if params["type"] != "OBJECT" {
		t.Errorf("归一后 parameters.type=%v, want OBJECT", params["type"])
	}
	if !canonicalNativeProperties(params["properties"]) {
		t.Errorf("归一后 properties 应是列表: %v", params["properties"])
	}
}

// ============ 并行工具响应合并 ============

func TestParallelToolResponses_Coalesced(t *testing.T) {
	// OpenAI 把每个 tool 结果拆成独立 message；应合并进同一个 function content
	// （Gemini 要求并行调用 turn 的 functionResponse 数 = functionCall 数，在同一 content）。
	body := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "x"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "a", "function": map[string]any{"name": "f1", "arguments": "{}"}},
				map[string]any{"id": "b", "function": map[string]any{"name": "f2", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "b", "content": "rb"},
			map[string]any{"role": "tool", "tool_call_id": "a", "content": "ra"},
		},
	}
	model, gemini, _ := ConvertChatRequest(body, config.StaticProvider(config.DefaultConfig()))
	vars := BuildVertexVariables(model, gemini, config.StaticProvider(config.DefaultConfig()))
	var funcContents int
	var funcResponseParts int
	for _, c := range vars["contents"].([]any) {
		cm := c.(map[string]any)
		if cm["role"] == "function" {
			funcContents++
			funcResponseParts += len(cm["parts"].([]any))
		}
	}
	if funcContents != 1 {
		t.Errorf("并行响应应合并进 1 个 function content，实际 %d 个", funcContents)
	}
	if funcResponseParts != 2 {
		t.Errorf("function content 应含 2 个 functionResponse part，实际 %d", funcResponseParts)
	}
}
