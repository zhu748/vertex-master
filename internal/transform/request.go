package transform

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

// safetyCategories 是默认安全设置覆盖的 5 个类别（缺省全 BLOCK_NONE）。
var safetyCategories = []string{ //nolint:gochecknoglobals
	"HARM_CATEGORY_HARASSMENT",
	"HARM_CATEGORY_HATE_SPEECH",
	"HARM_CATEGORY_SEXUALLY_EXPLICIT",
	"HARM_CATEGORY_DANGEROUS_CONTENT",
	"HARM_CATEGORY_CIVIC_INTEGRITY",
}

// supportedVarFields 是从 geminiPayload 透传进 variables 的字段及其 snake_case
// 别名。固定表避免每个请求重复做正则键转换。
var supportedVarFields = []struct { //nolint:gochecknoglobals
	camel string
	snake string
}{
	{camel: "contents", snake: "contents"},
	{camel: "tools", snake: "tools"},
	{camel: "toolConfig", snake: "tool_config"},
	{camel: "systemInstruction", snake: "system_instruction"},
	{camel: "safetySettings", snake: "safety_settings"},
	{camel: "generationConfig", snake: "generation_config"},
}

// ConvertChatRequest 将 OpenAI ChatCompletion 请求体转为 (model, geminiPayload)。
func ConvertChatRequest(body map[string]any, cfg config.ConfigProvider) (string, map[string]any, error) {
	return convertChatRequest(body, cfg, false)
}

func convertChatRequest(
	body map[string]any,
	cfg config.ConfigProvider,
	compactTextHistory bool,
) (string, map[string]any, error) {
	model, _ := body["model"].(string)

	if cfg.DebugMode() {
		geminiPayloadStr, _ := json.Marshal(body)
		log.Printf("[DEBUG] Payload 打印: ConvertChatRequest 转换前 payload: %s", string(geminiPayloadStr))
	}

	messagesRaw, ok := body["messages"].([]any)
	if !ok || len(messagesRaw) == 0 {
		return "", nil, fmt.Errorf("messages 不能为空 (messages must be a non-empty array)")
	}

	resolvedModel := ""
	var contents []any
	var systemParts []any
	compactContents := false
	if compactTextHistory {
		resolvedModel = cfg.ResolveModelName(model)
		if !isGemini36Model(resolvedModel) {
			contents, systemParts, compactContents = compactSingleTextContents(messagesRaw)
		}
	}
	if !compactContents {
		contents = make([]any, 0, len(messagesRaw))
	}
	var toolIDToName functionCallNameIndex
	var mixedTextStorage []canonicalSingleTextContent
	var compactFunctionCallStorage []canonicalFunctionCallContent
	var compactFunctionResponseStorage []canonicalFunctionResponseContent
	// Packed backing arrays let each compact turn expose an independent,
	// read-only full slice without allocating one parts array per message.
	var packedCompactFunctionCallParts canonicalFunctionCallParts
	var packedCompactFunctionResponseParts canonicalFunctionResponseParts
	compactCanonicalHistory := compactTextHistory && !isGemini36Model(resolvedModel)

	hasValidContents := compactContents
	for messageIndex, msgRaw := range messagesRaw {
		if compactContents {
			break
		}
		msg, ok := msgRaw.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("messages[%d] must be an object", messageIndex)
		}
		role, _ := msg["role"].(string)
		content := msg["content"]

		switch role {
		case "system", "developer":
			partsBefore := len(systemParts)
			switch c := content.(type) {
			case string:
				if c != "" {
					systemParts = append(systemParts, map[string]any{"text": c})
				}
			case []any:
				for partIndex, item := range c {
					if im, ok := item.(map[string]any); ok {
						if t, _ := im["type"].(string); t == "text" || t == "input_text" {
							text, textOK := im["text"].(string)
							if !textOK {
								return "", nil, fmt.Errorf(
									"messages[%d] %s content[%d].text must be a string",
									messageIndex,
									role,
									partIndex,
								)
							}
							if text != "" {
								systemParts = append(systemParts, map[string]any{"text": text})
							}
						} else {
							return "", nil, fmt.Errorf(
								"messages[%d] %s content[%d] has unsupported type %q",
								messageIndex,
								role,
								partIndex,
								t,
							)
						}
					} else if s, ok := item.(string); ok && s != "" {
						systemParts = append(systemParts, map[string]any{"text": s})
					} else if !ok {
						return "", nil, fmt.Errorf(
							"messages[%d] %s content[%d] must be a string or object",
							messageIndex,
							role,
							partIndex,
						)
					}
				}
			}
			if len(systemParts) == partsBefore && contentNeedsConversion(content) {
				return "", nil, fmt.Errorf(
					"messages[%d] %s content could not be converted",
					messageIndex,
					role,
				)
			}
		case "user":
			if compactCanonicalHistory {
				if text, textOK := compactSingleTextValue(content, false); textOK {
					if mixedTextStorage == nil {
						mixedTextStorage = make(
							[]canonicalSingleTextContent, 0, min(len(messagesRaw), 8),
						)
					}
					mixedTextStorage = append(mixedTextStorage, canonicalSingleTextContent{
						Parts: [1]canonicalSingleTextPart{{Text: text}},
						Role:  "user",
					})
					contents = append(contents, &mixedTextStorage[len(mixedTextStorage)-1])
					hasValidContents = true
					break
				}
			}
			if err := validateConvertibleMessageContent(content); err != nil {
				return "", nil, fmt.Errorf("messages[%d] user %w", messageIndex, err)
			}
			parts := convertUserContent(content)
			if convertedPartsHaveUsableContent(parts) {
				contents = append(contents, map[string]any{"role": "user", "parts": parts})
				hasValidContents = true
			} else if contentNeedsConversion(content) {
				return "", nil, fmt.Errorf("messages[%d] user content could not be converted", messageIndex)
			}
		case "assistant":
			var parts []any
			if isTruthy(content) {
				if err := validateConvertibleMessageContent(content); err != nil {
					return "", nil, fmt.Errorf("messages[%d] assistant %w", messageIndex, err)
				}
				parts = splitAssistantContent(content)
			}
			contentConverted := convertedPartsHaveUsableContent(parts)
			if !contentConverted && contentNeedsConversion(content) {
				return "", nil, fmt.Errorf("messages[%d] assistant content could not be converted", messageIndex)
			}
			if rawToolCalls, exists := msg["tool_calls"]; exists && rawToolCalls != nil {
				toolCalls, ok := rawToolCalls.([]any)
				if !ok {
					return "", nil, fmt.Errorf(
						"messages[%d] assistant tool_calls must be an array",
						messageIndex,
					)
				}
				if compactCanonicalHistory && len(parts) == 0 && len(toolCalls) > 0 {
					if packedCompactFunctionCallParts == nil {
						packedCompactFunctionCallParts = make(
							canonicalFunctionCallParts,
							0,
							min(len(messagesRaw), 8),
						)
					}
					compactPartsStart := len(packedCompactFunctionCallParts)
					canCompact := true
					for toolCallIndex, tc := range toolCalls {
						parsed, parsedOK := extractOAIToolCall(tc)
						if !parsedOK {
							return "", nil, fmt.Errorf(
								"messages[%d] assistant tool_calls[%d] must contain a function name",
								messageIndex,
								toolCallIndex,
							)
						}
						if parsed.id != "" {
							toolIDToName.Set(parsed.id, parsed.name)
						}
						if strings.TrimSpace(parsed.name) == "" ||
							!base64TreeCanSkipNormalization(parsed.args) {
							canCompact = false
						}
						packedCompactFunctionCallParts = append(
							packedCompactFunctionCallParts,
							canonicalFunctionCallPart{
								FunctionCall: canonicalFunctionCall{
									Args: parsed.args,
									ID:   parsed.id,
									Name: parsed.name,
								},
							},
						)
					}
					if canCompact {
						compactParts := packedCompactFunctionCallParts[compactPartsStart:len(packedCompactFunctionCallParts):len(packedCompactFunctionCallParts)]
						if compactFunctionCallStorage == nil {
							compactFunctionCallStorage = make(
								[]canonicalFunctionCallContent,
								0,
								min(len(messagesRaw), 8),
							)
						}
						compactFunctionCallStorage = append(
							compactFunctionCallStorage,
							canonicalFunctionCallContent{
								Parts:      compactParts,
								Role:       "model",
								normalized: true,
							},
						)
						contents = append(
							contents,
							&compactFunctionCallStorage[len(compactFunctionCallStorage)-1],
						)
						hasValidContents = true
						break
					}
				}
				for toolCallIndex, tc := range toolCalls {
					parsed, parsedOK := extractOAIToolCall(tc)
					if !parsedOK {
						return "", nil, fmt.Errorf(
							"messages[%d] assistant tool_calls[%d] must contain a function name",
							messageIndex,
							toolCallIndex,
						)
					}
					if parsed.id != "" {
						toolIDToName.Set(parsed.id, parsed.name)
					}
					fc := map[string]any{"name": parsed.name, "args": parsed.args}
					if parsed.id != "" {
						fc["id"] = parsed.id
					}
					parts = append(parts, map[string]any{"functionCall": fc})
				}
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": "model", "parts": parts})
				hasValidContents = true
			}
		case "tool":
			tcID, _ := msg["tool_call_id"].(string)
			name := firstTruthyString(msg["name"], toolIDToName.Get(tcID))
			response := coerceFunctionResponse(msg["content"])
			if compactCanonicalHistory && base64TreeCanSkipNormalization(response) {
				part := canonicalFunctionResponsePart{
					FunctionResponse: canonicalFunctionResponse{
						ID:       tcID,
						Name:     name,
						Response: response,
					},
				}
				if packedCompactFunctionResponseParts == nil {
					packedCompactFunctionResponseParts = make(
						canonicalFunctionResponseParts,
						0,
						min(len(messagesRaw), 8),
					)
				}
				if n := len(contents); n > 0 {
					if last, ok := contents[n-1].(*canonicalFunctionResponseContent); ok {
						partsStart := len(packedCompactFunctionResponseParts) - len(last.Parts)
						packedCompactFunctionResponseParts = append(
							packedCompactFunctionResponseParts,
							part,
						)
						last.Parts = packedCompactFunctionResponseParts[partsStart:len(packedCompactFunctionResponseParts):len(packedCompactFunctionResponseParts)]
						hasValidContents = true
						break
					}
				}
				partsStart := len(packedCompactFunctionResponseParts)
				packedCompactFunctionResponseParts = append(
					packedCompactFunctionResponseParts,
					part,
				)
				if compactFunctionResponseStorage == nil {
					compactFunctionResponseStorage = make(
						[]canonicalFunctionResponseContent,
						0,
						min(len(messagesRaw), 8),
					)
				}
				compactFunctionResponseStorage = append(
					compactFunctionResponseStorage,
					canonicalFunctionResponseContent{
						Parts:      packedCompactFunctionResponseParts[partsStart:len(packedCompactFunctionResponseParts):len(packedCompactFunctionResponseParts)],
						Role:       "function",
						normalized: true,
					},
				)
				contents = append(
					contents,
					&compactFunctionResponseStorage[len(compactFunctionResponseStorage)-1],
				)
				hasValidContents = true
				break
			}
			fr := map[string]any{"response": response}
			if name != "" {
				fr["name"] = name
			}
			if tcID != "" {
				fr["id"] = tcID
			}
			contents = appendFunctionResponse(contents, map[string]any{"functionResponse": fr})
			hasValidContents = true
		case "function":
			name := firstTruthyString(msg["name"])
			if name == "" {
				name = "unknown"
			}
			contents = appendFunctionResponse(contents, map[string]any{"functionResponse": map[string]any{
				"name": name, "response": coerceFunctionResponse(msg["content"]),
			}})
			hasValidContents = true
		default:
			return "", nil, fmt.Errorf("messages[%d] has unsupported role %q", messageIndex, role)
		}
	}
	if len(systemParts) == 0 && !hasValidContents {
		return "", nil, fmt.Errorf("messages must contain system instructions or valid content")
	}
	if !compactTextHistory {
		resolvedModel = cfg.ResolveModelName(model)
	}

	assistantPrefill := ""
	if isGemini36Model(resolvedModel) {
		contents, assistantPrefill = convertTrailingAssistantPrefill(contents)
	}

	geminiPayload := map[string]any{"contents": contents}
	if assistantPrefill != "" {
		geminiPayload[assistantPrefillMetadataKey] = assistantPrefill
	}
	if len(systemParts) > 0 {
		geminiPayload["systemInstruction"] = map[string]any{"parts": systemParts}
	}

	declaredToolNames, err := convertTools(body, geminiPayload)
	if err != nil {
		return "", nil, err
	}

	if err := convertToolChoice(body, geminiPayload, declaredToolNames); err != nil {
		return "", nil, err
	}

	var genCfg map[string]any
	for _, m := range []struct{ oai, gem string }{
		{"temperature", "temperature"},
		{"top_p", "topP"},
		{"top_k", "topK"},
		{"presence_penalty", "presencePenalty"},
		{"frequency_penalty", "frequencyPenalty"},
		{"seed", "seed"},
	} {
		if v, ok := body[m.oai]; ok && v != nil {
			if genCfg == nil {
				genCfg = make(map[string]any, 8)
			}
			genCfg[m.gem] = v
		}
	}

	if v, ok := body["logprobs"]; ok && v != nil {
		if genCfg == nil {
			genCfg = make(map[string]any, 8)
		}
		genCfg["responseLogprobs"] = isTruthy(v)
	}
	if v, ok := body["top_logprobs"]; ok && v != nil {
		if genCfg == nil {
			genCfg = make(map[string]any, 8)
		}
		genCfg["logprobs"] = v
	}

	var maxTokens any
	if v, ok := body["max_tokens"]; ok && v != nil {
		maxTokens = v
	} else if v, ok := body["max_completion_tokens"]; ok && v != nil {
		maxTokens = v
	}
	if maxTokens != nil {
		f, ok := maxTokens.(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f < 1 || f != math.Trunc(f) {
			return "", nil, fmt.Errorf("max_tokens/max_completion_tokens must be a finite integer >= 1")
		}
		if !cfg.DropMaxTokens() {
			if genCfg == nil {
				genCfg = make(map[string]any, 8)
			}
			genCfg["maxOutputTokens"] = maxTokens
		}
	}

	if stop, ok := body["stop"]; ok && stop != nil {
		if genCfg == nil {
			genCfg = make(map[string]any, 8)
		}
		switch s := stop.(type) {
		case string:
			genCfg["stopSequences"] = []any{s}
		case []any:
			for index, rawSequence := range s {
				if _, ok := rawSequence.(string); !ok {
					return "", nil, fmt.Errorf("stop[%d] must be a string", index)
				}
			}
			genCfg["stopSequences"] = s
		default:
			return "", nil, fmt.Errorf("stop must be a string or array of strings")
		}
	}

	if rawResponseFormat, exists := body["response_format"]; exists && rawResponseFormat != nil {
		rf, ok := rawResponseFormat.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("response_format must be an object")
		}
		responseType, _ := rf["type"].(string)
		switch responseType {
		case "", "text":
		case "json_object":
			if genCfg == nil {
				genCfg = make(map[string]any, 8)
			}
			genCfg["responseMimeType"] = "application/json"
		case "json_schema":
			jsonSchema, ok := rf["json_schema"].(map[string]any)
			if !ok {
				return "", nil, fmt.Errorf("response_format.json_schema must be an object")
			}
			schema, ok := jsonSchema["schema"].(map[string]any)
			if !ok {
				return "", nil, fmt.Errorf("response_format.json_schema.schema must be an object")
			}
			if genCfg == nil {
				genCfg = make(map[string]any, 8)
			}
			genCfg["responseMimeType"] = "application/json"
			genCfg["responseSchema"] = schema
		default:
			return "", nil, fmt.Errorf("unsupported response_format type %q", responseType)
		}
	}

	if sl, ok := body["safety_settings"].([]any); ok {
		geminiPayload["safetySettings"] = sl
	} else if sl, ok := body["safetySettings"].([]any); ok {
		geminiPayload["safetySettings"] = sl
	}

	if len(genCfg) > 0 {
		geminiPayload["generationConfig"] = genCfg
	}

	mrRaw := firstPresentRaw(body, "media_resolution", "mediaResolution")
	if mrRaw == nil {
		if extra, ok := body["extra_body"].(map[string]any); ok {
			mrRaw = firstPresentRaw(extra, "media_resolution", "mediaResolution")
		}
	}
	if mrRaw != nil {
		if mr := normalizeMediaResolution(mrRaw); mr != "" {
			ensureGenCfg(geminiPayload)["mediaResolution"] = mr
		}
	}

	if re, ok := body["reasoning_effort"].(string); ok {
		if level, ok := reasoningEffortToThinkingLevel[strings.ToLower(strings.TrimSpace(re))]; ok {
			gc := ensureGenCfg(geminiPayload)
			tc, ok := gc["thinkingConfig"].(map[string]any)
			if !ok {
				tc = map[string]any{}
				gc["thinkingConfig"] = tc
			}
			tc["thinkingLevel"] = level
		}
	}

	if thinking, ok := body["thinking"].(map[string]any); ok {
		if tt, _ := thinking["type"].(string); tt == "enabled" || tt == "disabled" {
			tc := map[string]any{"thinkingLevel": "MEDIUM"}
			if tt == "disabled" {
				tc["thinkingLevel"] = "NONE"
			}
			if budget, ok := thinking["budget_tokens"]; ok && budget != nil {
				tc["thinkingBudget"] = budget
			}
			ensureGenCfg(geminiPayload)["thinkingConfig"] = tc
		}
	}

	return model, geminiPayload, nil
}

// contentNeedsConversion reports whether a caller supplied substantive content.
// Empty/null content remains valid for assistant tool-call turns, but a non-empty
// unsupported value must not disappear silently from the prompt.
func contentNeedsConversion(content any) bool {
	switch value := content.(type) {
	case nil:
		return false
	case string:
		return value != ""
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	default:
		return true
	}
}

func convertedPartsHaveUsableContent(parts []any) bool {
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := part["text"].(string); ok && text != "" {
			return true
		}
		for _, key := range []string{
			"inlineData",
			"fileData",
			"functionCall",
			"functionResponse",
			"executableCode",
			"codeExecutionResult",
		} {
			if isTruthy(part[key]) {
				return true
			}
		}
	}
	return false
}

// appendFunctionResponse 把一个 functionResponse part 追加进 contents。
func appendFunctionResponse(contents []any, part map[string]any) []any {
	if n := len(contents); n > 0 {
		if last, ok := contents[n-1].(map[string]any); ok && last["role"] == "function" {
			parts, _ := last["parts"].([]any)
			last["parts"] = append(parts, part)
			return contents
		}
	}
	return append(contents, map[string]any{"role": "function", "parts": []any{part}})
}

// coerceFunctionResponse 把 tool/function 角色的 content 规范成 Gemini functionResponse.response。
func coerceFunctionResponse(raw any) map[string]any {
	obj := raw
	if s, ok := raw.(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			obj = parsed
		} else {
			obj = map[string]any{"result": s}
		}
	}
	if m, ok := obj.(map[string]any); ok {
		return m
	}
	return map[string]any{"result": obj}
}

// firstPresentRaw 在 map 里依次查 keys，返回第一个存在的原始值（不存在返回 nil）。
func firstPresentRaw(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

// parseModelName 解析模型名：经 models.json 的 alias_map 重映射。
func parseModelName(model string) string {
	return config.ResolveModelName(model)
}
