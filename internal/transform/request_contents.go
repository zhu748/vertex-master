package transform

import (
	"regexp"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

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

	var parts []any
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
		case "text":
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
	s, ok := content.(string)
	if !ok {
		return []any{map[string]any{"text": content}}
	}
	locs := assistantImageMarkdownRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return []any{map[string]any{"text": s}}
	}
	var parts []any
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
	if sanitized, changed := stripGeminiIDsCopy(geminiPayload, 0); changed {
		geminiPayload = sanitized.(map[string]any)
	}
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
		if !canonicalTextContentsCanPassThrough(c) {
			c = normalizeContents(c)
			c = handleInlineDataCase(c)
			c = normalizeContents(c)
			c = HandleBase64InContents(c)
			c = filterEmptyContents(c)
			c = EncodeThoughtSignature(c, 0)
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
		content, ok := rawContent.(map[string]any)
		if !ok || len(content) != 2 {
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
		if cm, ok := c.(map[string]any); ok {
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
			var sb strings.Builder
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
					normalized = camelizeFunctionRef(vm, "args")
					changed = true
				} else if nested, nestedChanged := handleInlineDataCaseCopy(v); nestedChanged {
					normalized = nested
					changed = true
				}
			case "functionResponse":
				if vm, ok := v.(map[string]any); ok {
					normalized = camelizeFunctionRef(vm, "response")
					changed = true
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

// camelizeFunctionRef 处理 functionCall/functionResponse 分支。
func camelizeFunctionRef(v map[string]any, payloadKey string) map[string]any {
	out := map[string]any{}
	if fid := firstTruthyString(v["id"], v["tool_call_id"], v["toolCallId"]); fid != "" {
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
	return out
}

// filterEmptyContents 对每个 content 的 parts 逐个清洗。
func filterEmptyContents(contents any) any {
	list, ok := contents.([]any)
	if !ok {
		return contents
	}

	var callIDMap map[string]string
	var lastModelFunctionCalls []string
	responseIndex := 0

	var filtered []any
	ensureFiltered := func(prefixEnd int) {
		if filtered == nil {
			filtered = make([]any, 0, len(list))
			filtered = append(filtered, list[:prefixEnd]...)
		}
	}
	for contentIndex, c := range list {
		cm, ok := c.(map[string]any)
		if !ok {
			ensureFiltered(contentIndex)
			continue
		}
		role, _ := cm["role"].(string)
		parts := asAnySlice(cm["parts"])

		if role == "model" {
			lastModelFunctionCalls = nil
			responseIndex = 0
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					if fc, ok := pm["functionCall"].(map[string]any); ok {
						if name, _ := fc["name"].(string); strings.TrimSpace(name) != "" {
							lastModelFunctionCalls = append(lastModelFunctionCalls, name)
							if fid, _ := fc["id"].(string); fid != "" {
								if callIDMap == nil {
									callIDMap = make(map[string]string)
								}
								callIDMap[fid] = name
							}
						}
					}
				}
			}
		}

		var cleanedParts []any
		ensureCleanedParts := func(prefixEnd int) {
			if cleanedParts == nil {
				cleanedParts = make([]any, 0, len(parts))
				cleanedParts = append(cleanedParts, parts[:prefixEnd]...)
			}
		}
		for partIndex, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				ensureCleanedParts(partIndex)
				continue
			}
			if cleanPartCanPassThrough(pm) {
				if cleanedParts != nil {
					cleanedParts = append(cleanedParts, pm)
				}
				continue
			}
			_, isFuncResponse := pm["functionResponse"]
			idx := -1
			if isFuncResponse {
				idx = responseIndex
				responseIndex++
			}
			if cleaned, ok := cleanPartWithID(pm, lastModelFunctionCalls, idx, callIDMap); ok {
				ensureCleanedParts(partIndex)
				cleanedParts = append(cleanedParts, cleaned)
			} else {
				ensureCleanedParts(partIndex)
			}
		}
		partsChanged := cleanedParts != nil
		if !partsChanged {
			cleanedParts = parts
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

const maxGeminiIDStripDepth = 256

// stripGeminiIDsCopy 只复制包含需要清洗 ID 的祖先 map/slice。绝大多数请求
// 不包含代理追加的 -vpXXXXXXXX 后缀，因此直接返回原树，允许并发候选安全共享。
func stripGeminiIDsCopy(value any, depth int) (any, bool) {
	if depth > maxGeminiIDStripDepth {
		return value, false
	}
	switch typed := value.(type) {
	case string:
		if strings.HasPrefix(typed, "gemini-tool-call-") && len(typed) > 11 && strings.Contains(typed, "-vp") {
			index := strings.LastIndex(typed, "-vp")
			if index > 0 && len(typed)-index == 11 {
				return typed[:index], true
			}
		}
		return value, false
	case map[string]any:
		var copied map[string]any
		for key, child := range typed {
			normalized, changed := stripGeminiIDsCopy(child, depth+1)
			if !changed {
				continue
			}
			if copied == nil {
				copied = make(map[string]any, len(typed))
				for originalKey, originalValue := range typed {
					copied[originalKey] = originalValue
				}
			}
			copied[key] = normalized
		}
		if copied != nil {
			return copied, true
		}
		return value, false
	case []any:
		var copied []any
		for index, child := range typed {
			normalized, changed := stripGeminiIDsCopy(child, depth+1)
			if !changed {
				continue
			}
			if copied == nil {
				copied = append([]any(nil), typed...)
			}
			copied[index] = normalized
		}
		if copied != nil {
			return copied, true
		}
		return value, false
	default:
		return value, false
	}
}
