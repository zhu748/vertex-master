package transform

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

// validateConvertibleMessageContent prevents one valid part from masking other
// unsupported parts in the same OpenAI message. Without this check a client can
// send [valid text, unknown prompt block] and receive 200 even though part of
// its context was silently discarded.
func validateConvertibleMessageContent(content any) error {
	switch value := content.(type) {
	case nil, string:
		return nil
	case []any:
		for index, rawItem := range value {
			if _, ok := rawItem.(string); ok {
				continue
			}
			item, ok := rawItem.(map[string]any)
			if !ok {
				return fmt.Errorf("content[%d] must be a string or object", index)
			}
			contentType, _ := item["type"].(string)
			switch contentType {
			case "text", "input_text", "output_text":
				if _, ok := item["text"].(string); !ok {
					return fmt.Errorf("content[%d].text must be a string", index)
				}
			case "image_url":
				url := imageURLString(item["image_url"])
				if strings.HasPrefix(url, "data:") {
					mime, data := parseDataURI(url)
					if mime == "" || data == "" {
						return fmt.Errorf("content[%d] contains an invalid data image", index)
					}
				} else if !hasRemotePrefix(url) {
					return fmt.Errorf("content[%d] contains an unsupported image URL", index)
				}
			case "video_url", "input_video":
				url := holderURLString(item[contentType])
				_, data := parseDataURI(url)
				if !strings.HasPrefix(url, "data:") || data == "" {
					return fmt.Errorf("content[%d] contains an unsupported video", index)
				}
			case "input_audio":
				_, data := parseInputAudio(item["input_audio"])
				if data == "" {
					return fmt.Errorf("content[%d] contains invalid input audio", index)
				}
			default:
				return fmt.Errorf("content[%d] has unsupported type %q", index, contentType)
			}
		}
		return nil
	default:
		return fmt.Errorf("content must be a string or an array")
	}
}

// assistantImageMarkdownRe 匹配 assistant 文本里嵌的 markdown data-URI 图片。
var assistantImageMarkdownRe = regexp.MustCompile(`!\[[^\]]*\]\((data:[^()\s;,]+;base64,[A-Za-z0-9+/=_\-]+)\)`)

// convertUserContent 把 OpenAI user message content 转为 Gemini parts。
func convertUserContent(content any) []any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return []any{map[string]any{"text": s}}
	}
	list, ok := content.([]any)
	if !ok {
		return nil
	}

	parts := make([]any, 0, len(list))
	for _, itemRaw := range list {
		if s, ok := itemRaw.(string); ok {
			parts = append(parts, map[string]any{"text": s})
			continue
		}
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := item["type"].(string)
		switch t {
		case "text", "input_text", "output_text":
			parts = append(parts, map[string]any{"text": item["text"]})

		case "image_url":
			url := imageURLString(item["image_url"])
			if strings.HasPrefix(url, "data:") {
				if mime, b64 := parseDataURI(url); mime != "" && b64 != "" {
					parts = append(parts, inlineDataPart(mime, b64))
				}
			} else if hasRemotePrefix(url) {
				parts = append(parts, map[string]any{"fileData": map[string]any{
					"mimeType": guessMIMEFromURL(url), "fileUri": url,
				}})
			}

		case "video_url", "input_video":
			url := holderURLString(item[t])
			if strings.HasPrefix(url, "data:") {
				if mime, b64 := parseDataURI(url); b64 != "" {
					if mime == "" || !strings.HasPrefix(mime, "video/") {
						mime = "video/mp4"
					}
					parts = append(parts, inlineDataPart(mime, b64))
				}
			}

		case "input_audio":
			mime, b64 := parseInputAudio(item["input_audio"])
			if b64 != "" {
				if mime == "" || !strings.HasPrefix(mime, "audio/") {
					mime = "audio/wav"
				}
				parts = append(parts, inlineDataPart(mime, b64))
			}
		}
	}
	return parts
}

// splitAssistantContent 把 assistant 文本切成 text / inlineData 混合 parts。
func splitAssistantContent(content any) []any {
	if _, ok := content.([]any); ok {
		// OpenAI permits assistant content as a typed content-part array. Reuse
		// the same text/media conversion as user content so pure-text arrays stay
		// plain strings and can participate in Gemini 3.6 prefill adaptation.
		// Media parts remain media, causing the prefill adapter to leave that real
		// multimodal history untouched.
		return convertUserContent(content)
	}
	s, ok := content.(string)
	if !ok {
		return []any{map[string]any{"text": content}}
	}
	if !strings.Contains(s, "data:") || !strings.Contains(s, "![") {
		return []any{map[string]any{"text": s}}
	}
	locs := assistantImageMarkdownRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return []any{map[string]any{"text": s}}
	}
	parts := make([]any, 0, len(locs)*2+1)
	last := 0
	for _, m := range locs {
		if pre := strings.TrimSpace(s[last:m[0]]); pre != "" {
			parts = append(parts, map[string]any{"text": pre})
		}
		if mime, b64 := parseDataURI(s[m[2]:m[3]]); mime != "" && b64 != "" {
			parts = append(parts, inlineDataPart(mime, b64))
		}
		last = m[1]
	}
	if post := strings.TrimSpace(s[last:]); post != "" {
		parts = append(parts, map[string]any{"text": post})
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]any{"text": ""})
	}
	return parts
}

// inlineDataPart 构造 inlineData part，data 经 NormalizeBase64 规范化。
func inlineDataPart(mime, b64 string) map[string]any {
	return map[string]any{"inlineData": map[string]any{
		"mimeType": mime, "data": NormalizeBase64(b64),
	}}
}

type canonicalSingleTextPart struct {
	Text string `json:"text"`
}

// canonicalSingleTextContent is used only by DefaultRequestConverter for a
// fully validated plain-text history. Field order matches encoding/json's
// lexical map order so the optimized representation preserves wire bytes.
type canonicalSingleTextContent struct {
	Parts [1]canonicalSingleTextPart `json:"parts"`
	Role  string                     `json:"role"`
}

// canonicalFunctionCallContent keeps the JSON shape emitted by the compatibility
// converter while avoiding dynamic maps both before and after outbound cleanup.
type canonicalFunctionCallContent struct {
	Parts canonicalFunctionCallParts `json:"parts"`
	Role  string                     `json:"role"`
	// normalized is private so encoding/json preserves the exact map wire shape.
	normalized bool
}

type canonicalFunctionCallParts []canonicalFunctionCallPart

type canonicalFunctionCallPart struct {
	FunctionCall     canonicalFunctionCall `json:"functionCall"`
	ThoughtSignature string                `json:"thoughtSignature,omitempty"`
}

type canonicalFunctionCall struct {
	Args any    `json:"args"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

// canonicalFunctionResponseContent is the response-side counterpart of
// canonicalFunctionCallContent. Response remains a read-only decoded object.
type canonicalFunctionResponseContent struct {
	Parts      canonicalFunctionResponseParts `json:"parts"`
	Role       string                         `json:"role"`
	normalized bool
}

type canonicalFunctionResponseParts []canonicalFunctionResponsePart

type canonicalFunctionResponsePart struct {
	FunctionResponse canonicalFunctionResponse `json:"functionResponse"`
}

type canonicalFunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name,omitempty"`
	Response map[string]any `json:"response"`
}

func (content *canonicalFunctionCallContent) CanonicalJSONFieldCount() (int, bool) {
	return 2, canonicalFunctionCallContentValid(content)
}

func (content *canonicalFunctionCallContent) CanonicalJSONField(index int) (string, any) {
	if index == 0 {
		return "parts", &content.Parts
	}
	return "role", content.Role
}

func (parts *canonicalFunctionCallParts) CanonicalJSONItemCount() (int, bool) {
	if parts == nil {
		return 0, false
	}
	return len(*parts), true
}

func (parts *canonicalFunctionCallParts) CanonicalJSONItem(index int) any {
	return &(*parts)[index]
}

func (part *canonicalFunctionCallPart) CanonicalJSONFieldCount() (int, bool) {
	if part == nil {
		return 0, false
	}
	if part.ThoughtSignature == "" {
		return 1, true
	}
	return 2, true
}

func (part *canonicalFunctionCallPart) CanonicalJSONField(index int) (string, any) {
	if index == 0 {
		return "functionCall", &part.FunctionCall
	}
	return "thoughtSignature", part.ThoughtSignature
}

func (call *canonicalFunctionCall) CanonicalJSONFieldCount() (int, bool) {
	if call == nil {
		return 0, false
	}
	if call.ID == "" {
		return 2, true
	}
	return 3, true
}

func (call *canonicalFunctionCall) CanonicalJSONField(index int) (string, any) {
	if index == 0 {
		return "args", call.Args
	}
	if call.ID != "" {
		if index == 1 {
			return "id", call.ID
		}
	}
	return "name", call.Name
}

func (content *canonicalFunctionResponseContent) CanonicalJSONFieldCount() (int, bool) {
	return 2, canonicalFunctionResponseContentValid(content)
}

func (content *canonicalFunctionResponseContent) CanonicalJSONField(index int) (string, any) {
	if index == 0 {
		return "parts", &content.Parts
	}
	return "role", content.Role
}

func (parts *canonicalFunctionResponseParts) CanonicalJSONItemCount() (int, bool) {
	if parts == nil {
		return 0, false
	}
	return len(*parts), true
}

func (parts *canonicalFunctionResponseParts) CanonicalJSONItem(index int) any {
	return &(*parts)[index]
}

func (part *canonicalFunctionResponsePart) CanonicalJSONFieldCount() (int, bool) {
	return 1, part != nil
}

func (part *canonicalFunctionResponsePart) CanonicalJSONField(_ int) (string, any) {
	return "functionResponse", &part.FunctionResponse
}

func (response *canonicalFunctionResponse) CanonicalJSONFieldCount() (int, bool) {
	if response == nil {
		return 0, false
	}
	count := 1
	if response.ID != "" {
		count++
	}
	if response.Name != "" {
		count++
	}
	return count, true
}

func (response *canonicalFunctionResponse) CanonicalJSONField(index int) (string, any) {
	if response.ID != "" {
		if index == 0 {
			return "id", response.ID
		}
		index--
	}
	if response.Name != "" {
		if index == 0 {
			return "name", response.Name
		}
	}
	return "response", response.Response
}

// CanonicalTextContent exposes the validated scalar fields to downstream
// budget and cache-key walkers without exporting the compact wire type.
func (content *canonicalSingleTextContent) CanonicalTextContent() (role, text string, ok bool) {
	if content == nil ||
		(content.Role != "user" && content.Role != "model") ||
		content.Parts[0].Text == "" {
		return "", "", false
	}
	return content.Role, content.Parts[0].Text, true
}

func compactSingleTextContents(messages []any) (contents, systemParts []any, ok bool) {
	contentCount := 0
	systemCount := 0
	for _, rawMessage := range messages {
		role, _, messageOK := compactSingleTextMessage(rawMessage)
		if !messageOK {
			return nil, nil, false
		}
		if role == "system" || role == "developer" {
			systemCount++
		} else {
			contentCount++
		}
	}

	storage := make([]canonicalSingleTextContent, contentCount)
	contents = make([]any, 0, contentCount)
	systemParts = make([]any, 0, systemCount)
	contentIndex := 0
	for _, rawMessage := range messages {
		role, text, _ := compactSingleTextMessage(rawMessage)
		if role == "system" || role == "developer" {
			systemParts = append(systemParts, map[string]any{"text": text})
			continue
		}
		if role == "assistant" {
			role = "model"
		}
		storage[contentIndex] = canonicalSingleTextContent{
			Parts: [1]canonicalSingleTextPart{{Text: text}},
			Role:  role,
		}
		contents = append(contents, &storage[contentIndex])
		contentIndex++
	}
	return contents, systemParts, true
}

func compactSingleTextMessage(rawMessage any) (role, text string, ok bool) {
	message, ok := rawMessage.(map[string]any)
	if !ok {
		return "", "", false
	}
	role, _ = message["role"].(string)
	if role != "system" && role != "developer" && role != "user" && role != "assistant" {
		return "", "", false
	}
	content := message["content"]
	text, ok = compactSingleTextValue(content, role == "system" || role == "developer")
	if !ok {
		return "", "", false
	}
	if role != "assistant" {
		return role, text, true
	}
	if _, isString := content.(string); isString &&
		strings.Contains(text, "data:") && strings.Contains(text, "![") {
		return "", "", false
	}
	if rawToolCalls, exists := message["tool_calls"]; exists && rawToolCalls != nil {
		toolCalls, ok := rawToolCalls.([]any)
		if !ok || len(toolCalls) > 0 {
			return "", "", false
		}
	}
	return role, text, true
}

func compactSingleTextValue(content any, system bool) (string, bool) {
	if text, ok := content.(string); ok {
		return text, text != ""
	}
	parts, ok := content.([]any)
	if !ok || len(parts) != 1 {
		return "", false
	}
	if text, ok := parts[0].(string); ok {
		return text, text != ""
	}
	part, ok := parts[0].(map[string]any)
	if !ok {
		return "", false
	}
	partType, _ := part["type"].(string)
	if partType != "text" && partType != "input_text" &&
		(system || partType != "output_text") {
		return "", false
	}
	text, ok := part["text"].(string)
	return text, ok && text != ""
}

// imageURLString 从 image_url 字段取出 url 字符串（兼容 {url} 与字符串两种形态）。
func imageURLString(v any) string {
	if m, ok := v.(map[string]any); ok {
		s, _ := m["url"].(string)
		return s
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// holderURLString 从 video_url/input_video 的 holder 取出 url（兼容 {url} 与字符串）。
func holderURLString(holder any) string {
	switch h := holder.(type) {
	case string:
		return h
	case map[string]any:
		s, _ := h["url"].(string)
		return s
	default:
		return ""
	}
}

// parseInputAudio 从 input_audio holder 解析 (mime, base64)。
func parseInputAudio(holder any) (string, string) {
	switch h := holder.(type) {
	case string:
		if strings.HasPrefix(h, "data:") {
			return parseDataURI(h)
		}
	case map[string]any:
		if rawData, ok := h["data"].(string); ok && rawData != "" {
			if strings.HasPrefix(rawData, "data:") {
				return parseDataURI(rawData)
			}
			fmtStr, _ := h["format"].(string)
			return audioFormatMIME[strings.ToLower(fmtStr)], rawData
		}
		if url, ok := h["url"].(string); ok && strings.HasPrefix(url, "data:") {
			return parseDataURI(url)
		}
	}
	return "", ""
}

// hasRemotePrefix 判断 URL 是否为远程引用（http/https/gs）。
func hasRemotePrefix(url string) bool {
	return strings.HasPrefix(url, "http://") ||
		strings.HasPrefix(url, "https://") ||
		strings.HasPrefix(url, "gs://")
}

// BuildVertexVariables 由 geminiPayload 构建发往上游的 variables。
func BuildVertexVariables(model string, geminiPayload map[string]any, cfg config.ConfigProvider) map[string]any {
	contents := geminiPayload["contents"]
	canonicalTextContents := canonicalTextContentsCanPassThrough(contents)
	canonicalToolContents := !canonicalTextContents &&
		canonicalToolContentsCanSkipNormalization(contents)
	vars := make(map[string]any, len(supportedVarFields)+2)
	resolvedModel := parseModelName(model)
	vars["model"] = resolvedModel

	for _, field := range supportedVarFields {
		if value, ok := geminiPayload[field.camel]; ok {
			vars[field.camel] = value
		} else if value, ok := geminiPayload[field.snake]; ok {
			vars[field.camel] = value
		}
	}

	handleSystemInstruction(vars)

	if c, ok := vars["contents"]; ok {
		if !canonicalTextContents {
			if !canonicalToolContents {
				c = normalizeContents(c)
				c = handleInlineDataCase(c)
				c = normalizeContents(c)
				c = HandleBase64InContents(c)
			}
			c = filterEmptyContents(c)
			if !canonicalToolContents {
				c = EncodeThoughtSignature(c, 0)
			}
		}
		vars["contents"] = c
	}

	if rawTools, ok := vars["tools"]; ok {
		normalized := normalizeToolsFormat(rawTools)
		if len(normalized) > 0 {
			vars["tools"] = normalized
		} else {
			delete(vars, "tools")
			delete(vars, "toolConfig")
		}
	}
	if tc, ok := vars["toolConfig"]; ok {
		vars["toolConfig"] = convertToolsFormat(tc)
	}

	genCfg, sharedGenCfg := buildGenerationConfig(geminiPayload)
	// 在最终出站层执行，确保 OpenAI Chat/Responses、Anthropic 以及 Gemini
	// 原生请求都一致遵守 drop_max_tokens。转换层仍负责校验兼容协议字段。
	gemini36 := isGemini36Model(resolvedModel)
	if len(genCfg) > 0 && sharedGenCfg && (cfg.DropMaxTokens() || gemini36) {
		genCfg = copyMap(genCfg)
	}
	if cfg.DropMaxTokens() {
		delete(genCfg, "maxOutputTokens")
	}
	applyModelGenerationCompatibility(resolvedModel, genCfg)
	if len(genCfg) > 0 {
		vars["generationConfig"] = genCfg
	} else {
		delete(vars, "generationConfig")
	}

	vars["safetySettings"] = normalizeSafetySettings(vars["safetySettings"], cfg)

	return vars
}

// canonicalTextContentsCanPassThrough 只接受最常见且已完全规范的纯文本形状。
// 对工具、媒体、thought、额外字段、旧角色名或空文本一律回退完整归一化，
// 因而可以安全跳过六轮递归扫描而不改变兼容行为。
func canonicalTextContentsCanPassThrough(contents any) bool {
	list, ok := contents.([]any)
	if !ok {
		return false
	}
	for _, rawContent := range list {
		switch content := rawContent.(type) {
		case *canonicalSingleTextContent:
			if _, _, ok := content.CanonicalTextContent(); !ok {
				return false
			}
		case map[string]any:
			if len(content) != 2 {
				return false
			}
			role, _ := content["role"].(string)
			if role != "user" && role != "model" {
				return false
			}
			parts, ok := content["parts"].([]any)
			if !ok || len(parts) == 0 {
				return false
			}
			for _, rawPart := range parts {
				part, ok := rawPart.(map[string]any)
				if !ok || len(part) != 1 {
					return false
				}
				text, ok := part["text"].(string)
				if !ok || text == "" {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// canonicalToolContentsCanSkipNormalization accepts only the canonical
// text/function shapes emitted by the request converters. These parts still
// pass through filterEmptyContents to strip internal IDs, resolve response
// names and add encoded thought signatures, but need none of the preceding
// role/key/base64 normalization or the final recursive signature scan.
func canonicalToolContentsCanSkipNormalization(contents any) bool {
	list, ok := contents.([]any)
	if !ok || len(list) == 0 {
		return false
	}

	hasToolPart := false
	previousFunctionTurn := false
	for _, rawContent := range list {
		switch compact := rawContent.(type) {
		case *canonicalSingleTextContent:
			if _, _, valid := compact.CanonicalTextContent(); !valid {
				return false
			}
			previousFunctionTurn = false
			continue
		case *canonicalFunctionCallContent:
			if !canonicalFunctionCallContentValid(compact) {
				return false
			}
			previousFunctionTurn = false
			hasToolPart = true
			continue
		case *canonicalFunctionResponseContent:
			if previousFunctionTurn || !canonicalFunctionResponseContentValid(compact) {
				return false
			}
			previousFunctionTurn = true
			hasToolPart = true
			continue
		}

		content, ok := rawContent.(map[string]any)
		if !ok || len(content) != 2 {
			return false
		}
		role, _ := content["role"].(string)
		if role != "user" && role != "model" && role != "function" {
			return false
		}
		functionTurn := role == "function"
		if functionTurn && previousFunctionTurn {
			return false
		}
		previousFunctionTurn = functionTurn

		parts, ok := content["parts"].([]any)
		if !ok || len(parts) == 0 {
			return false
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || len(part) != 1 {
				return false
			}
			switch role {
			case "user":
				if !canonicalPlainTextPartCanSkipNormalization(part) {
					return false
				}
			case "model":
				if canonicalPlainTextPartCanSkipNormalization(part) {
					continue
				}
				if !canonicalFunctionCallPartCanSkipNormalization(part) {
					return false
				}
				hasToolPart = true
			case "function":
				if !canonicalFunctionResponsePartCanSkipNormalization(part) {
					return false
				}
				hasToolPart = true
			}
		}
	}
	return hasToolPart
}

func canonicalFunctionCallContentValid(content *canonicalFunctionCallContent) bool {
	if content == nil ||
		!content.normalized ||
		content.Role != "model" ||
		len(content.Parts) == 0 {
		return false
	}
	for _, part := range content.Parts {
		call := part.FunctionCall
		if strings.TrimSpace(call.Name) == "" {
			return false
		}
	}
	return true
}

func canonicalFunctionResponseContentValid(content *canonicalFunctionResponseContent) bool {
	return content != nil &&
		content.normalized &&
		content.Role == "function" &&
		len(content.Parts) > 0
}

func canonicalPlainTextPartCanSkipNormalization(part map[string]any) bool {
	text, ok := part["text"].(string)
	return ok && text != ""
}

func canonicalFunctionCallPartCanSkipNormalization(part map[string]any) bool {
	call, ok := part["functionCall"].(map[string]any)
	if !ok || len(call) < 2 || len(call) > 3 || !truthyStr(call["name"]) {
		return false
	}
	if _, exists := call["args"]; !exists {
		return false
	} else if _, needsDecoding := call["args"].(string); needsDecoding {
		return false
	} else if !base64TreeCanSkipNormalization(call["args"]) {
		return false
	}
	for key, value := range call {
		switch key {
		case "name", "args":
		case "id":
			id, ok := value.(string)
			if !ok || id == "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func canonicalFunctionResponsePartCanSkipNormalization(part map[string]any) bool {
	response, ok := part["functionResponse"].(map[string]any)
	if !ok || len(response) == 0 || len(response) > 3 {
		return false
	}
	if _, ok := response["response"].(map[string]any); !ok {
		return false
	}
	if !base64TreeCanSkipNormalization(response["response"]) {
		return false
	}
	for key, value := range response {
		switch key {
		case "response":
		case "name":
			if _, ok := value.(string); !ok {
				return false
			}
		case "id":
			id, ok := value.(string)
			if !ok || id == "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func base64TreeCanSkipNormalization(value any) bool {
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			if !base64TreeCanSkipNormalization(item) {
				return false
			}
		}
	case map[string]any:
		for key, nested := range current {
			if key == "inlineData" {
				if inline, ok := nested.(map[string]any); ok {
					if data, ok := inline["data"].(string); ok {
						if NormalizeBase64(data) != data {
							return false
						}
						// HandleBase64InContents treats this inlineData node as
						// an opaque payload once it finds a string data field.
						continue
					}
				}
			}
			if !base64TreeCanSkipNormalization(nested) {
				return false
			}
		}
	}
	return true
}

// normalizeSafetySettings makes native Gemini requests behave like converted
// OpenAI requests when clients send an empty list or protobuf UNSPECIFIED
// threshold values. Explicit, meaningful thresholds are preserved.
func normalizeSafetySettings(raw any, cfg config.ConfigProvider) []vertexSafetySetting {
	configuredSettings := cfg.SafetySettings()
	settings, ok := raw.([]any)
	if !ok || len(settings) == 0 {
		return buildSafetySettingsFromMap(configuredSettings)
	}
	out := make([]vertexSafetySetting, 0, len(settings))
	for _, itemRaw := range settings {
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}
		category := strings.TrimSpace(toString(item["category"]))
		if category == "" {
			continue
		}
		threshold := strings.ToUpper(strings.TrimSpace(toString(item["threshold"])))
		if threshold == "" || strings.Contains(threshold, "UNSPECIFIED") {
			threshold = "BLOCK_NONE"
			if configured, exists := configuredSettings[category]; exists && configured != "" {
				threshold = configured
			}
		}
		out = append(out, vertexSafetySetting{Category: category, Threshold: threshold})
	}
	if len(out) == 0 {
		return buildSafetySettingsFromMap(configuredSettings)
	}
	return out
}

// handleSystemInstruction 把 systemInstruction 在无 user content 时降级为首条 user message。
func handleSystemInstruction(vars map[string]any) {
	siRaw, ok := vars["systemInstruction"]
	if !ok || !isTruthy(siRaw) {
		return
	}
	contents, _ := vars["contents"].([]any)
	for _, c := range contents {
		if compact, ok := c.(*canonicalSingleTextContent); ok {
			role, _, valid := compact.CanonicalTextContent()
			if valid && role == "user" {
				return
			}
		} else if cm, ok := c.(map[string]any); ok {
			if r, _ := cm["role"].(string); r == "user" {
				return
			}
		}
	}
	text := extractTextFromInstruction(siRaw)
	if text == "" {
		return
	}
	userMsg := map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}}
	vars["contents"] = append([]any{userMsg}, contents...)
	delete(vars, "systemInstruction")
}

func extractTextFromInstruction(instruction any) string {
	if s, ok := instruction.(string); ok {
		return s
	}
	if m, ok := instruction.(map[string]any); ok {
		if parts, ok := m["parts"].([]any); ok {
			textLength := 0
			maximumInt := int(^uint(0) >> 1)
			for _, p := range parts {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				text, ok := pm["text"].(string)
				if !ok {
					continue
				}
				if len(text) > maximumInt-textLength {
					textLength = 0
					break
				}
				textLength += len(text)
			}
			var sb strings.Builder
			if textLength > 0 {
				sb.Grow(textLength)
			}
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					if t, ok := pm["text"]; ok {
						sb.WriteString(toString(t))
					}
				}
			}
			return sb.String()
		}
	}
	return ""
}

// normalizeContents 把 contents 归一为 Gemini content 列表。
func normalizeContents(contents any) any {
	switch c := contents.(type) {
	case nil:
		return []any{}
	case string:
		return []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": c}}}}
	case map[string]any:
		return []any{normalizeContent(c)}
	case []any:
		var normalized []any
		var pendingText []any
		ensureNormalized := func(prefixEnd int) {
			if normalized == nil {
				normalized = make([]any, 0, len(c))
				normalized = append(normalized, c[:prefixEnd]...)
			}
		}
		flushPending := func() {
			if len(pendingText) == 0 {
				return
			}
			normalized = append(normalized, map[string]any{"role": "user", "parts": pendingText})
			pendingText = nil
		}
		for index, item := range c {
			if s, ok := item.(string); ok {
				ensureNormalized(index)
				pendingText = append(pendingText, map[string]any{"text": s})
			} else if compact, ok := item.(*canonicalSingleTextContent); ok {
				if _, _, valid := compact.CanonicalTextContent(); !valid {
					ensureNormalized(index)
					continue
				}
				if len(pendingText) > 0 {
					flushPending()
				}
				if normalized != nil {
					normalized = append(normalized, compact)
				}
			} else if compact, ok := item.(*canonicalFunctionCallContent); ok {
				if !canonicalFunctionCallContentValid(compact) {
					ensureNormalized(index)
					continue
				}
				if len(pendingText) > 0 {
					flushPending()
				}
				if normalized != nil {
					normalized = append(normalized, compact)
				}
			} else if compact, ok := item.(*canonicalFunctionResponseContent); ok {
				if !canonicalFunctionResponseContentValid(compact) {
					ensureNormalized(index)
					continue
				}
				if len(pendingText) > 0 {
					flushPending()
				}
				if normalized != nil {
					normalized = append(normalized, compact)
				}
			} else if m, ok := item.(map[string]any); ok {
				if len(pendingText) > 0 {
					flushPending()
				}
				normalizedContent, changed := normalizeContentCopy(m)
				if normalized != nil {
					normalized = append(normalized, normalizedContent)
				} else if changed {
					ensureNormalized(index)
					normalized = append(normalized, normalizedContent)
				}
			} else {
				ensureNormalized(index)
			}
		}
		if len(pendingText) > 0 {
			flushPending()
		}
		if normalized == nil {
			normalized = c
		}

		// 合并相邻的具有相同 normalized 角色的 content 回合，确保角色严格交替
		mergeNeeded := false
		for index := 1; index < len(normalized); index++ {
			current, currentOK := normalized[index].(map[string]any)
			previous, previousOK := normalized[index-1].(map[string]any)
			if !currentOK || !previousOK {
				continue
			}
			currentRole, _ := current["role"].(string)
			previousRole, _ := previous["role"].(string)
			if (currentRole == "function" || currentRole == "tool") &&
				(previousRole == "function" || previousRole == "tool") {
				mergeNeeded = true
				break
			}
		}
		if !mergeNeeded {
			return normalized
		}

		merged := make([]any, 0, len(normalized))
		for _, item := range normalized {
			m, ok := item.(map[string]any)
			if !ok {
				merged = append(merged, item)
				continue
			}
			role, _ := m["role"].(string)
			if role == "function" || role == "tool" {
				if len(merged) > 0 {
					if last, ok := merged[len(merged)-1].(map[string]any); ok {
						lastRole, _ := last["role"].(string)
						if lastRole == "function" || lastRole == "tool" {
							lastParts, _ := last["parts"].([]any)
							currentParts, _ := m["parts"].([]any)
							combinedParts := make([]any, 0, len(lastParts)+len(currentParts))
							combinedParts = append(combinedParts, lastParts...)
							combinedParts = append(combinedParts, currentParts...)
							combined := copyMap(last)
							combined["parts"] = combinedParts
							merged[len(merged)-1] = combined
							continue
						}
					}
				}
			}
			merged = append(merged, m)
		}
		return merged
	default:
		return contents
	}
}

// normalizeContent 归一单个 content（role 映射 + content→parts + str→text）。
func normalizeContent(content map[string]any) map[string]any {
	normalized, _ := normalizeContentCopy(content)
	return normalized
}

func normalizeContentCopy(content map[string]any) (map[string]any, bool) {
	n := content
	changed := false
	ensureCopy := func() {
		if !changed {
			n = copyMap(content)
			changed = true
		}
	}
	_, hasContent := content["content"]
	_, hasParts := content["parts"]
	switch {
	case hasContent && !hasParts:
		ensureCopy()
		n["parts"] = normalizeParts(content["content"])
		delete(n, "content")
	case hasParts:
		parts, partsChanged := normalizePartsCopy(content["parts"])
		if partsChanged {
			ensureCopy()
			n["parts"] = parts
		}
	default:
		ensureCopy()
		if t, hasText := content["text"]; hasText {
			n["parts"] = []any{map[string]any{"text": toString(t)}}
			delete(n, "text")
		} else {
			n["parts"] = []any{}
		}
	}
	switch role, _ := content["role"].(string); role {
	case "assistant":
		ensureCopy()
		n["role"] = "model"
	case "tool":
		ensureCopy()
		n["role"] = "function"
	case "":
		ensureCopy()
		n["role"] = "user"
	}
	return n, changed
}

// normalizeParts 把 parts 归一为 part 列表。
func normalizeParts(parts any) []any {
	normalized, _ := normalizePartsCopy(parts)
	return normalized
}

func normalizePartsCopy(parts any) ([]any, bool) {
	switch p := parts.(type) {
	case nil:
		return []any{}, true
	case string:
		return []any{map[string]any{"text": p}}, true
	case map[string]any:
		return []any{normalizePart(p)}, true
	case []any:
		var out []any
		ensureOut := func(prefixEnd int) {
			if out == nil {
				out = make([]any, 0, len(p))
				out = append(out, p[:prefixEnd]...)
			}
		}
		for index, item := range p {
			if s, ok := item.(string); ok {
				ensureOut(index)
				out = append(out, map[string]any{"text": s})
			} else if m, ok := item.(map[string]any); ok {
				normalizedPart, changed := normalizePartCopy(m)
				if len(normalizedPart) == 0 {
					ensureOut(index)
					continue
				}
				if out != nil {
					out = append(out, normalizedPart)
				} else if changed {
					ensureOut(index)
					out = append(out, normalizedPart)
				}
			} else {
				ensureOut(index)
			}
		}
		if out != nil {
			return out, true
		}
		return p, false
	default:
		return []any{map[string]any{"text": toString(parts)}}, true
	}
}

// normalizePart 把 OpenAI 风格 part 归一为 Gemini part。
func normalizePart(part map[string]any) map[string]any {
	normalized, _ := normalizePartCopy(part)
	return normalized
}

func normalizePartCopy(part map[string]any) (map[string]any, bool) {
	pt, _ := part["type"].(string)
	switch pt {
	case "text", "input_text":
		return map[string]any{"text": toString(part["text"])}, true

	case "image_url", "input_image":
		var url string
		switch u := firstNonEmpty(part["image_url"], part["input_image"]).(type) {
		case map[string]any:
			url = toString(u["url"])
		case string:
			url = u
		}
		if strings.HasPrefix(url, "data:") {
			if mime, data := parseDataURI(url); mime != "" && data != "" {
				return map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}}, true
			}
		}
		if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "gs://") {
			return map[string]any{"fileData": map[string]any{"mimeType": guessMIMEFromURI(url), "fileUri": url}}, true
		}

	case "media", "file", "file_data":
		fileURI := toString(firstNonEmpty(part["fileUri"], part["file_uri"], part["uri"], part["url"]))
		if fileURI != "" {
			mime := firstTruthyString(part["mimeType"], part["mime_type"])
			if mime == "" {
				mime = guessMIMEFromURI(fileURI)
			}
			return map[string]any{"fileData": map[string]any{"mimeType": mime, "fileUri": fileURI}}, true
		}

	case "inline_data", "inlineData":
		inline := part
		if m, ok := part["inlineData"].(map[string]any); ok {
			inline = m
		} else if m, ok := part["inline_data"].(map[string]any); ok {
			inline = m
		}
		data := toString(inline["data"])
		mime := firstTruthyString(inline["mimeType"], inline["mime_type"], part["mimeType"], part["mime_type"])
		if data != "" && mime != "" {
			return map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}}, true
		}
	}

	var out map[string]any
	for k, v := range part {
		if k == "type" {
			if out == nil {
				out = copyMap(part)
			}
			delete(out, k)
			continue
		}
		camelKey := SnakeToCamel(k)
		if camelKey == k {
			continue
		}
		if out == nil {
			out = copyMap(part)
		}
		delete(out, k)
		out[camelKey] = v
	}
	if out != nil {
		return out, true
	}
	return part, false
}

// handleInlineDataCase 递归把键 camelCase 化。
func handleInlineDataCase(contents any) any {
	normalized, _ := handleInlineDataCaseCopy(contents)
	return normalized
}

func handleInlineDataCaseCopy(contents any) (any, bool) {
	switch c := contents.(type) {
	case []any:
		var out []any
		for i, item := range c {
			normalized, changed := handleInlineDataCaseCopy(item)
			if !changed {
				continue
			}
			if out == nil {
				out = append([]any(nil), c...)
			}
			out[i] = normalized
		}
		if out != nil {
			return out, true
		}
		return contents, false
	case map[string]any:
		var out map[string]any
		for k, v := range c {
			camelK := SnakeToCamel(k)
			normalized := v
			changed := camelK != k
			switch camelK {
			case "inlineData":
				if vm, ok := v.(map[string]any); ok {
					var normalizedInline map[string]any
					for ik, iv := range vm {
						camelInlineKey := SnakeToCamel(ik)
						if camelInlineKey == ik {
							continue
						}
						if normalizedInline == nil {
							normalizedInline = copyMap(vm)
						}
						delete(normalizedInline, ik)
						normalizedInline[camelInlineKey] = iv
					}
					if normalizedInline != nil {
						normalized = normalizedInline
						changed = true
					}
				}
			case "functionCall":
				if vm, ok := v.(map[string]any); ok {
					var nestedChanged bool
					normalized, nestedChanged = camelizeFunctionRefCopy(vm, "args")
					changed = changed || nestedChanged
				} else if nested, nestedChanged := handleInlineDataCaseCopy(v); nestedChanged {
					normalized = nested
					changed = true
				}
			case "functionResponse":
				if vm, ok := v.(map[string]any); ok {
					var nestedChanged bool
					normalized, nestedChanged = camelizeFunctionRefCopy(vm, "response")
					changed = changed || nestedChanged
				} else if nested, nestedChanged := handleInlineDataCaseCopy(v); nestedChanged {
					normalized = nested
					changed = true
				}
			default:
				if nested, nestedChanged := handleInlineDataCaseCopy(v); nestedChanged {
					normalized = nested
					changed = true
				}
			}
			if !changed {
				continue
			}
			if out == nil {
				out = copyMap(c)
			}
			if camelK != k {
				delete(out, k)
			}
			out[camelK] = normalized
		}
		if out != nil {
			return out, true
		}
		return contents, false
	default:
		return contents, false
	}
}

// camelizeFunctionRefCopy 处理 functionCall/functionResponse 分支。已经使用
// camelCase 且无需删除别名的引用保持原只读 map，避免规范工具历史重复复制。
func camelizeFunctionRefCopy(v map[string]any, payloadKey string) (map[string]any, bool) {
	if functionRefCanPassThrough(v, payloadKey) {
		return v, false
	}

	fid := firstTruthyString(v["id"], v["tool_call_id"], v["toolCallId"])
	out := map[string]any{}
	if fid != "" {
		out["id"] = fid
	}
	for ik, iv := range v {
		cik := SnakeToCamel(ik)
		switch cik {
		case payloadKey:
			out[cik] = iv
		case "id", "toolCallId":
		default:
			out[cik] = handleInlineDataCase(iv)
		}
	}
	return out, true
}

func functionRefCanPassThrough(v map[string]any, payloadKey string) bool {
	fid := firstTruthyString(v["id"], v["tool_call_id"], v["toolCallId"])
	for key, value := range v {
		if strings.IndexByte(key, '_') >= 0 {
			return false
		}
		switch key {
		case "id":
			id, ok := value.(string)
			if !ok || id == "" || id != fid {
				return false
			}
		case "toolCallId":
			return false
		case payloadKey:
		default:
			if !inlineDataCaseCanPassThrough(value) {
				return false
			}
		}
	}
	return true
}

func inlineDataCaseCanPassThrough(contents any) bool {
	switch value := contents.(type) {
	case []any:
		for _, item := range value {
			if !inlineDataCaseCanPassThrough(item) {
				return false
			}
		}
	case map[string]any:
		for key, nested := range value {
			if strings.IndexByte(key, '_') >= 0 {
				return false
			}
			switch key {
			case "inlineData":
				if inline, ok := nested.(map[string]any); ok {
					for inlineKey := range inline {
						if strings.IndexByte(inlineKey, '_') >= 0 {
							return false
						}
					}
				}
			case "functionCall":
				if functionCall, ok := nested.(map[string]any); ok {
					if !functionRefCanPassThrough(functionCall, "args") {
						return false
					}
				} else if !inlineDataCaseCanPassThrough(nested) {
					return false
				}
			case "functionResponse":
				if functionResponse, ok := nested.(map[string]any); ok {
					if !functionRefCanPassThrough(functionResponse, "response") {
						return false
					}
				} else if !inlineDataCaseCanPassThrough(nested) {
					return false
				}
			default:
				if !inlineDataCaseCanPassThrough(nested) {
					return false
				}
			}
		}
	}
	return true
}

// filterEmptyContents 对每个 content 的 parts 逐个清洗。
func filterEmptyContents(contents any) any {
	list, ok := contents.([]any)
	if !ok {
		return contents
	}

	var callIDIndex functionCallNameIndex
	var lastModelFunctionCallsInline [8]string
	var lastModelFunctionCalls []string
	responseIndex := 0

	var filtered []any
	var packedCleanedParts []any
	ensureFiltered := func(prefixEnd int) {
		if filtered == nil {
			filtered = make([]any, 0, len(list))
			filtered = append(filtered, list[:prefixEnd]...)
		}
	}
	for contentIndex, c := range list {
		switch compact := c.(type) {
		case *canonicalSingleTextContent:
			if _, _, valid := compact.CanonicalTextContent(); valid {
				if filtered != nil {
					filtered = append(filtered, compact)
				}
			} else {
				ensureFiltered(contentIndex)
			}
			continue
		case *canonicalFunctionCallContent:
			if !canonicalFunctionCallContentValid(compact) {
				ensureFiltered(contentIndex)
				continue
			}
			lastModelFunctionCalls = lastModelFunctionCallsInline[:0]
			responseIndex = 0
			for _, part := range compact.Parts {
				call := part.FunctionCall
				lastModelFunctionCalls = append(lastModelFunctionCalls, call.Name)
				if call.ID != "" {
					callIDIndex.Set(normalizeGeminiToolCallID(call.ID), call.Name)
				}
			}
			ensureFiltered(contentIndex)
			filtered = append(filtered, cleanCanonicalFunctionCallContent(compact))
			continue
		case *canonicalFunctionResponseContent:
			if !canonicalFunctionResponseContentValid(compact) {
				ensureFiltered(contentIndex)
				continue
			}
			ensureFiltered(contentIndex)
			filtered = append(
				filtered,
				cleanCanonicalFunctionResponseContent(
					compact,
					lastModelFunctionCalls,
					&responseIndex,
					&callIDIndex,
				),
			)
			continue
		}
		cm, ok := c.(map[string]any)
		if !ok {
			ensureFiltered(contentIndex)
			continue
		}
		role, _ := cm["role"].(string)
		parts := asAnySlice(cm["parts"])

		if role == "model" {
			lastModelFunctionCalls = lastModelFunctionCallsInline[:0]
			responseIndex = 0
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					if fc, ok := pm["functionCall"].(map[string]any); ok {
						if name, _ := fc["name"].(string); strings.TrimSpace(name) != "" {
							lastModelFunctionCalls = append(lastModelFunctionCalls, name)
							if fid, _ := fc["id"].(string); fid != "" {
								callIDIndex.Set(normalizeGeminiToolCallID(fid), name)
							}
						}
					}
				}
			}
		}

		cleanedPartsStart := -1
		ensureCleanedParts := func(prefixEnd int) {
			if cleanedPartsStart < 0 {
				if packedCleanedParts == nil {
					packedCleanedParts = make([]any, 0, len(list))
				}
				cleanedPartsStart = len(packedCleanedParts)
				packedCleanedParts = append(packedCleanedParts, parts[:prefixEnd]...)
			}
		}
		for partIndex, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				ensureCleanedParts(partIndex)
				continue
			}
			if cleanPartCanPassThrough(pm) {
				if cleanedPartsStart >= 0 {
					packedCleanedParts = append(packedCleanedParts, pm)
				}
				continue
			}
			_, isFuncResponse := pm["functionResponse"]
			idx := -1
			if isFuncResponse {
				idx = responseIndex
				responseIndex++
			}
			if cleaned, ok := cleanPartWithID(pm, lastModelFunctionCalls, idx, &callIDIndex); ok {
				ensureCleanedParts(partIndex)
				packedCleanedParts = append(packedCleanedParts, cleaned)
			} else {
				ensureCleanedParts(partIndex)
			}
		}
		partsChanged := cleanedPartsStart >= 0
		var cleanedParts []any
		if !partsChanged {
			cleanedParts = parts
		} else {
			cleanedParts = packedCleanedParts[cleanedPartsStart:len(packedCleanedParts):len(packedCleanedParts)]
		}
		if len(cleanedParts) > 0 {
			if partsChanged {
				ensureFiltered(contentIndex)
				nc := copyMap(cm)
				nc["parts"] = cleanedParts
				filtered = append(filtered, nc)
			} else if filtered != nil {
				filtered = append(filtered, cm)
			}
		} else {
			ensureFiltered(contentIndex)
		}
	}
	if filtered != nil {
		return filtered
	}
	return list
}

func cleanCanonicalFunctionCallContent(
	content *canonicalFunctionCallContent,
) *canonicalFunctionCallContent {
	parts := make(canonicalFunctionCallParts, len(content.Parts))
	for index, part := range content.Parts {
		parts[index] = part
		parts[index].FunctionCall.ID = ""
		parts[index].ThoughtSignature = encodedSkipThoughtSentinel
	}
	return &canonicalFunctionCallContent{
		Parts: parts, Role: "model", normalized: true,
	}
}

func cleanCanonicalFunctionResponseContent(
	content *canonicalFunctionResponseContent,
	functionCallNames []string,
	responseIndex *int,
	callIDIndex *functionCallNameIndex,
) *canonicalFunctionResponseContent {
	parts := make(canonicalFunctionResponseParts, len(content.Parts))
	for index, part := range content.Parts {
		response := part.FunctionResponse
		name := strings.TrimSpace(response.Name)
		if name == "" && response.ID != "" {
			name = callIDIndex.Get(normalizeGeminiToolCallID(response.ID))
		}
		if name == "" &&
			*responseIndex >= 0 &&
			*responseIndex < len(functionCallNames) {
			name = functionCallNames[*responseIndex]
		}
		if name == "" {
			name = "unknown"
		}
		response.ID = ""
		response.Name = name
		parts[index] = canonicalFunctionResponsePart{FunctionResponse: response}
		*responseIndex++
	}
	return &canonicalFunctionResponseContent{
		Parts: parts, Role: "function", normalized: true,
	}
}

func normalizeGeminiToolCallID(value string) string {
	if !strings.HasPrefix(value, "gemini-tool-call-") {
		return value
	}
	index := strings.LastIndex(value, "-vp")
	if index > 0 && len(value)-index == 11 {
		return value[:index]
	}
	return value
}
