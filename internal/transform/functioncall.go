package transform

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

const skipThoughtSentinel = "skip_thought_signature_validator"

var encodedSkipThoughtSentinel = base64.StdEncoding.EncodeToString( //nolint:gochecknoglobals
	[]byte(skipThoughtSentinel),
)

// NormalizeBase64 规范化 base64：剥离 data URI 前缀、URL-safe 字符还原、补 padding。
func NormalizeBase64(data string) string {
	value := strings.TrimSpace(data)
	if strings.Contains(value, ",") && strings.HasPrefix(value, "data:") {
		if idx := strings.Index(value, ","); idx >= 0 {
			value = value[idx+1:]
		}
	}
	value = strings.NewReplacer("-", "+", "_", "/").Replace(value)
	if pad := len(value) % 4; pad != 0 {
		value += strings.Repeat("=", 4-pad)
	}
	return value
}

// FcNameTracker 按出现顺序追踪 functionCall 名称。
type FcNameTracker struct {
	names []string
	idx   int
}

// NewFcNameTracker 过滤掉空名后构造追踪器。
func NewFcNameTracker(names []string) *FcNameTracker {
	filtered := make([]string, 0, len(names))
	for _, n := range names {
		if strings.TrimSpace(n) != "" {
			filtered = append(filtered, n)
		}
	}
	return &FcNameTracker{names: filtered}
}

// NextName 返回下一个未用的名称，用尽返回 ("", false)。
func (t *FcNameTracker) NextName() (string, bool) {
	if t.idx < len(t.names) {
		name := strings.TrimSpace(t.names[t.idx])
		t.idx++
		if name != "" {
			return name, true
		}
	}
	return "", false
}

// cleanPartWithID 是 CleanPart 的 id 锚点版本。
func cleanPartWithID(part map[string]any, functionCallNames []string, responseIndex int, callIDMap map[string]string) (map[string]any, bool) {
	hasValid := false
	cleaned := map[string]any{}

	if v, ok := part["text"]; ok {
		if v != nil && toString(v) != "" {
			cleaned["text"] = v
			hasValid = true
		}
	}

	if v, ok := part["thought"]; ok {
		cleaned["thought"] = v
	}

	if fcRaw, ok := part["functionCall"]; ok {
		if fcMap, ok := fcRaw.(map[string]any); ok {
			if truthyStr(fcMap["name"]) {
				fixed := fixFunctionCallArgs(fcMap)
				delete(fixed, "id")
				cleaned["functionCall"] = fixed
				hasValid = true
			}
		}
	}

	if frRaw, ok := part["functionResponse"]; ok {
		if frMap, ok := frRaw.(map[string]any); ok {
			name := strings.TrimSpace(toString(frMap["name"]))
			if name == "" {
				if fid, _ := frMap["id"].(string); fid != "" && callIDMap != nil {
					name = callIDMap[fid]
				}
				if name == "" && responseIndex >= 0 && responseIndex < len(functionCallNames) {
					name = functionCallNames[responseIndex]
				}
				if name == "" {
					name = "unknown"
				}
			}
			fixed := copyMap(frMap)
			fixed["name"] = name
			delete(fixed, "id")
			normalizeFunctionResponseBody(fixed)
			cleaned["functionResponse"] = fixed
			hasValid = true
		}
	}

	if idRaw, ok := part["inlineData"]; ok {
		if id, ok := idRaw.(map[string]any); ok {
			if truthyStr(id["data"]) && truthyStr(id["mimeType"]) {
				cleaned["inlineData"] = idRaw
				hasValid = true
			}
		}
	}

	if fdRaw, ok := part["fileData"]; ok {
		if fd, ok := fdRaw.(map[string]any); ok {
			if truthyStr(fd["fileUri"]) && truthyStr(fd["mimeType"]) {
				cleaned["fileData"] = fdRaw
				hasValid = true
			}
		}
	}

	for _, key := range []string{"executableCode", "codeExecutionResult"} {
		if v, ok := part[key]; ok && isTruthy(v) {
			cleaned[key] = v
			hasValid = true
		}
	}

	if v, ok := part["thoughtSignature"]; ok {
		cleaned["thoughtSignature"] = v
	}

	for _, key := range []string{"videoMetadata", "mediaResolution"} {
		if v, ok := part[key]; ok && isTruthy(v) {
			cleaned[key] = v
		}
	}

	finalizeCleanedPart(cleaned)

	if hasValid {
		return cleaned, true
	}
	return nil, false
}

// cleanPartCanPassThrough 识别无需删字段、补名称或写 thoughtSignature 的标准
// Gemini part。返回原只读 map 可避免普通文本/媒体 part 每次请求都重新分配。
func cleanPartCanPassThrough(part map[string]any) bool {
	hasValid := false
	for key, value := range part {
		switch key {
		case "text":
			if value == nil || toString(value) == "" {
				return false
			}
			hasValid = true
		case "inlineData":
			inline, ok := value.(map[string]any)
			if !ok || !truthyStr(inline["data"]) || !truthyStr(inline["mimeType"]) {
				return false
			}
			hasValid = true
		case "fileData":
			file, ok := value.(map[string]any)
			if !ok || !truthyStr(file["fileUri"]) || !truthyStr(file["mimeType"]) {
				return false
			}
			hasValid = true
		case "executableCode", "codeExecutionResult":
			if !isTruthy(value) {
				return false
			}
			hasValid = true
		case "videoMetadata", "mediaResolution":
			if !isTruthy(value) {
				return false
			}
		default:
			return false
		}
	}
	return hasValid
}

// fixFunctionCallArgs 拷贝 functionCall 并把字符串 args 解析为对象。
func fixFunctionCallArgs(fc map[string]any) map[string]any {
	fixed := copyMap(fc)
	if argStr, ok := fixed["args"].(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(argStr), &parsed); err == nil {
			fixed["args"] = parsed
		} else {
			fixed["args"] = map[string]any{"raw": argStr}
		}
	}
	return fixed
}

// finalizeCleanedPart 对清洗后的 part 做收尾归一。
func finalizeCleanedPart(cleaned map[string]any) {
	if tv, ok := cleaned["thought"]; ok {
		if _, isStr := tv.(string); !isStr {
			if _, isBool := tv.(bool); !isBool {
				cleaned["thought"] = ""
			}
		}
	}

	if _, ok := cleaned["functionResponse"]; ok {
		delete(cleaned, "thought")
		delete(cleaned, "thoughtSignature")
	} else {
		_, hasFC := cleaned["functionCall"]
		_, hasThought := cleaned["thought"]
		_, hasSig := cleaned["thoughtSignature"]
		if hasFC || hasThought || hasSig {
			cleaned["thoughtSignature"] = skipThoughtSentinel
		}
	}

	if truthyStr(cleaned["text"]) && !isTruthy(cleaned["thought"]) {
		delete(cleaned, "thought")
		delete(cleaned, "thoughtSignature")
	}
}

// EncodeThoughtSignature 递归把 thoughtSignature 的 sentinel 值 base64 编码。
func EncodeThoughtSignature(contents any, depth int) any {
	encoded, _ := encodeThoughtSignatureCopy(contents, depth)
	return encoded
}

func encodeThoughtSignatureCopy(contents any, depth int) (any, bool) {
	const maxDepth = 64
	if depth > maxDepth {
		return contents, false
	}
	switch v := contents.(type) {
	case []any:
		var out []any
		for i, item := range v {
			encoded, changed := encodeThoughtSignatureCopy(item, depth+1)
			if !changed {
				continue
			}
			if out == nil {
				out = append([]any(nil), v...)
			}
			out[i] = encoded
		}
		if out != nil {
			return out, true
		}
		return contents, false
	case map[string]any:
		var out map[string]any
		for k, val := range v {
			if k == "parts" {
				if parts, ok := val.([]any); ok {
					var newParts []any
					for i, p := range parts {
						if pm, ok := p.(map[string]any); ok {
							if sig, ok := pm["thoughtSignature"].(string); ok && sig == skipThoughtSentinel {
								if newParts == nil {
									newParts = append([]any(nil), parts...)
								}
								np := copyMap(pm)
								np["thoughtSignature"] = encodedSkipThoughtSentinel
								newParts[i] = np
							}
						}
					}
					if newParts != nil {
						if out == nil {
							out = copyMap(v)
						}
						out[k] = newParts
					}
					continue
				}
			}
			if encoded, changed := encodeThoughtSignatureCopy(val, depth+1); changed {
				if out == nil {
					out = copyMap(v)
				}
				out[k] = encoded
			}
		}
		if out != nil {
			return out, true
		}
		return contents, false
	default:
		return contents, false
	}
}

// HandleBase64InContents 递归规范化 contents 中 inlineData 的 base64 数据。
func HandleBase64InContents(contents any) any {
	normalized, _ := handleBase64InContentsCopy(contents)
	return normalized
}

func handleBase64InContentsCopy(contents any) (any, bool) {
	switch v := contents.(type) {
	case []any:
		var out []any
		for i, item := range v {
			normalized, changed := handleBase64InContentsCopy(item)
			if !changed {
				continue
			}
			if out == nil {
				out = append([]any(nil), v...)
			}
			out[i] = normalized
		}
		if out != nil {
			return out, true
		}
		return contents, false
	case map[string]any:
		var out map[string]any
		for k, val := range v {
			normalized := val
			changed := false
			handledInlineData := false
			if k == "inlineData" {
				if id, ok := val.(map[string]any); ok {
					if data, ok := id["data"].(string); ok {
						handledInlineData = true
						normalizedData := NormalizeBase64(data)
						if normalizedData != data {
							ni := copyMap(id)
							ni["data"] = normalizedData
							normalized = ni
							changed = true
						}
					}
				}
			}
			if !handledInlineData {
				if nested, nestedChanged := handleBase64InContentsCopy(val); nestedChanged {
					normalized = nested
					changed = true
				}
			}
			if !changed {
				continue
			}
			if out == nil {
				out = copyMap(v)
			}
			out[k] = normalized
		}
		if out != nil {
			return out, true
		}
		return contents, false
	default:
		return contents, false
	}
}

// ContentBlockMerger 增量合并相邻同类型文本块。它让流式调用方无需先保留
// 全部 part，再在响应结束时做第二遍扫描。
type ContentBlockMerger struct {
	merged  []map[string]any
	current map[string]any
	text    StringAccumulator
}

// NewContentBlockMerger 创建增量合并器。capacityHint 仅用于预分配最终块切片，
// 上限与 MergeContentBlocks 原有策略一致，避免不可信输入触发过量预分配。
func NewContentBlockMerger(capacityHint int) *ContentBlockMerger {
	capacityHint = min(max(capacityHint, 0), 32)
	return &ContentBlockMerger{merged: make([]map[string]any, 0, capacityHint)}
}

// Add 加入一个内容块；输入 map 不会被修改。
func (m *ContentBlockMerger) Add(part map[string]any) {
	if m == nil {
		return
	}
	if !truthyStr(part["text"]) {
		cleaned := cleanSimple(part)
		if cleaned == nil {
			return
		}
		m.flushText()
		m.merged = append(m.merged, cleaned)
		return
	}

	isThought := isTruthy(part["thought"])
	if m.current != nil && isTruthy(m.current["thought"]) == isThought {
		m.text.WriteString(toString(part["text"]))
		if sig, ok := part["thoughtSignature"]; ok {
			if _, exists := m.current["thoughtSignature"]; !exists {
				m.current["thoughtSignature"] = sig
			}
		}
		return
	}

	m.flushText()
	m.current = make(map[string]any, 3)
	if isThought {
		m.current["thought"] = true
		if sig, ok := part["thoughtSignature"]; ok {
			m.current["thoughtSignature"] = sig
		}
	}
	m.text.WriteString(toString(part["text"]))
}

// Result 刷新最后一个文本块并返回合并结果。重复调用是安全的。
func (m *ContentBlockMerger) Result() []map[string]any {
	if m == nil {
		return nil
	}
	m.flushText()
	return m.merged
}

func (m *ContentBlockMerger) flushText() {
	if m.current == nil {
		return
	}
	m.current["text"] = m.text.String()
	m.merged = append(m.merged, m.current)
	m.current = nil
	m.text.Reset()
}

// MergeContentBlocks 合并相邻同类型文本块（thought+thought、text+text）。
func MergeContentBlocks(parts []map[string]any) []map[string]any {
	merger := NewContentBlockMerger(len(parts))
	for _, part := range parts {
		merger.Add(part)
	}
	return merger.Result()
}
