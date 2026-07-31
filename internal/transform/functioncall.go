package transform

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"unsafe"
)

const skipThoughtSentinel = "skip_thought_signature_validator"

var encodedSkipThoughtSentinel = base64.StdEncoding.EncodeToString( //nolint:gochecknoglobals
	[]byte(skipThoughtSentinel),
)

var base64StandardAlphabet = func() [256]byte { //nolint:gochecknoglobals
	var replacements [256]byte
	for index := range replacements {
		replacements[index] = byte(index)
	}
	replacements['-'] = '+'
	replacements['_'] = '/'
	return replacements
}()

// NormalizeBase64 规范化 base64：剥离 data URI 前缀、URL-safe 字符还原、补 padding。
func NormalizeBase64(data string) string {
	value := strings.TrimSpace(data)
	if strings.Contains(value, ",") && strings.HasPrefix(value, "data:") {
		if idx := strings.Index(value, ","); idx >= 0 {
			value = value[idx+1:]
		}
	}
	padding := 0
	if remainder := len(value) % 4; remainder != 0 {
		padding = 4 - remainder
	}
	if strings.IndexByte(value, '-') < 0 && strings.IndexByte(value, '_') < 0 {
		if padding == 0 {
			return value
		}
		return value + strings.Repeat("=", padding)
	}

	normalized := make([]byte, len(value)+padding)
	index := 0
	for ; index+7 < len(value); index += 8 {
		normalized[index] = base64StandardAlphabet[value[index]]
		normalized[index+1] = base64StandardAlphabet[value[index+1]]
		normalized[index+2] = base64StandardAlphabet[value[index+2]]
		normalized[index+3] = base64StandardAlphabet[value[index+3]]
		normalized[index+4] = base64StandardAlphabet[value[index+4]]
		normalized[index+5] = base64StandardAlphabet[value[index+5]]
		normalized[index+6] = base64StandardAlphabet[value[index+6]]
		normalized[index+7] = base64StandardAlphabet[value[index+7]]
	}
	for ; index < len(value); index++ {
		normalized[index] = base64StandardAlphabet[value[index]]
	}
	for index := len(value); index < len(normalized); index++ {
		normalized[index] = '='
	}
	// normalized escapes with the returned immutable string and is never mutated
	// again, so reusing its backing storage avoids a second multi-megabyte copy.
	return unsafe.String(unsafe.SliceData(normalized), len(normalized))
}

// FcNameTracker 按出现顺序追踪 functionCall 名称。
type FcNameTracker struct {
	names []string
	idx   int
}

type functionCallNameEntry struct {
	id   string
	name string
}

// functionCallNameIndex keeps the common short tool-call history entirely
// inline. Once it grows beyond the inline capacity, all entries are promoted
// to a map so lookup semantics remain unchanged for large or duplicate-ID
// histories.
type functionCallNameIndex struct {
	inline   [8]functionCallNameEntry
	overflow map[string]string
	count    int
}

func (index *functionCallNameIndex) Set(id, name string) {
	if index == nil || id == "" {
		return
	}
	if index.overflow != nil {
		index.overflow[id] = name
		return
	}
	for entryIndex := range index.count {
		if index.inline[entryIndex].id == id {
			index.inline[entryIndex].name = name
			return
		}
	}
	if index.count < len(index.inline) {
		index.inline[index.count] = functionCallNameEntry{id: id, name: name}
		index.count++
		return
	}

	index.overflow = make(map[string]string, len(index.inline)+1)
	for _, entry := range index.inline {
		index.overflow[entry.id] = entry.name
	}
	index.overflow[id] = name
}

func (index *functionCallNameIndex) Get(id string) string {
	if index == nil || id == "" {
		return ""
	}
	if index.overflow != nil {
		return index.overflow[id]
	}
	for entryIndex := index.count - 1; entryIndex >= 0; entryIndex-- {
		if index.inline[entryIndex].id == id {
			return index.inline[entryIndex].name
		}
	}
	return ""
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
func cleanPartWithID(
	part map[string]any,
	functionCallNames []string,
	responseIndex int,
	callIDIndex *functionCallNameIndex,
) (map[string]any, bool) {
	if len(part) == 1 {
		if fcRaw, ok := part["functionCall"]; ok {
			fcMap, valid := fcRaw.(map[string]any)
			if !valid || !truthyStr(fcMap["name"]) {
				return nil, false
			}
			fixed := fixFunctionCallArgs(fcMap)
			delete(fixed, "id")
			return map[string]any{
				"functionCall":     fixed,
				"thoughtSignature": encodedSkipThoughtSentinel,
			}, true
		}
		if frRaw, ok := part["functionResponse"]; ok {
			frMap, valid := frRaw.(map[string]any)
			if !valid {
				return nil, false
			}
			return map[string]any{
				"functionResponse": cleanFunctionResponseWithID(
					frMap,
					functionCallNames,
					responseIndex,
					callIDIndex,
				),
			}, true
		}
	}

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
			cleaned["functionResponse"] = cleanFunctionResponseWithID(
				frMap,
				functionCallNames,
				responseIndex,
				callIDIndex,
			)
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

	finalizeCleanedPart(cleaned, true)

	if hasValid {
		return cleaned, true
	}
	return nil, false
}

func cleanFunctionResponseWithID(
	fr map[string]any,
	functionCallNames []string,
	responseIndex int,
	callIDIndex *functionCallNameIndex,
) map[string]any {
	name := strings.TrimSpace(toString(fr["name"]))
	if name == "" {
		if fid, _ := fr["id"].(string); fid != "" {
			name = callIDIndex.Get(normalizeGeminiToolCallID(fid))
		}
		if name == "" && responseIndex >= 0 && responseIndex < len(functionCallNames) {
			name = functionCallNames[responseIndex]
		}
		if name == "" {
			name = "unknown"
		}
	}
	fixed := copyMap(fr)
	fixed["name"] = name
	delete(fixed, "id")
	normalizeFunctionResponseBody(fixed)
	return fixed
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

// finalizeCleanedPart 对清洗后的 part 做收尾归一。encodeSignature 用于
// BuildVertexVariables 的最终出站路径，避免先写哨兵再递归复制整段 contents。
func finalizeCleanedPart(cleaned map[string]any, encodeSignature bool) {
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
			signature := skipThoughtSentinel
			if encodeSignature {
				signature = encodedSkipThoughtSentinel
			}
			cleaned["thoughtSignature"] = signature
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
	merged           []map[string]any
	currentPart      map[string]any
	currentText      string
	currentSignature any
	text             StringAccumulator
	currentCount     int
	currentThought   bool
	hasSignature     bool
}

// NewContentBlockMerger 创建增量合并器。capacityHint 仅用于预分配最终块切片，
// 上限与 MergeContentBlocks 原有策略一致，避免不可信输入触发过量预分配。
func NewContentBlockMerger(capacityHint int) *ContentBlockMerger {
	capacityHint = min(max(capacityHint, 0), 32)
	return &ContentBlockMerger{merged: make([]map[string]any, 0, capacityHint)}
}

// Add 加入一个内容块；输入 map 不会被修改。已经是规范形状且无需合并的
// 单个文本块会按只读引用复用，其余形状在 flush 时才延迟创建结果 map。
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
	text := toString(part["text"])
	if m.currentCount > 0 && m.currentThought == isThought {
		if m.currentCount == 1 {
			m.text.WriteString(m.currentText)
		}
		m.text.WriteString(text)
		m.currentCount++
		if isThought && !m.hasSignature {
			if signature, ok := part["thoughtSignature"]; ok {
				m.currentSignature = signature
				m.hasSignature = true
			}
		}
		return
	}

	m.flushText()
	m.currentPart = part
	m.currentText = text
	m.currentCount = 1
	m.currentThought = isThought
	if isThought {
		if signature, ok := part["thoughtSignature"]; ok {
			m.currentSignature = signature
			m.hasSignature = true
		}
	}
}

// AddOwned 加入所有权已经转移给合并器的内容块。标准文本块会先原地清成
// 最终规范形状，使显式 thought:false 等常见上游字段也能走单块复用路径。
// 调用方不得在返回后继续读取或修改 part。
func (m *ContentBlockMerger) AddOwned(part map[string]any) {
	if m == nil {
		return
	}
	if truthyStr(part["text"]) {
		normalizeOwnedTextPart(part)
	}
	m.Add(part)
}

// AddPlainText 加入已经过协议验证的普通文本，不为每个流帧构造临时 part map。
func (m *ContentBlockMerger) AddPlainText(text string) {
	if m == nil || text == "" {
		return
	}
	if m.currentCount > 0 && !m.currentThought {
		if m.currentCount == 1 {
			m.text.WriteString(m.currentText)
		}
		m.text.WriteString(text)
		m.currentCount++
		return
	}
	m.flushText()
	m.currentPart = nil
	m.currentText = text
	m.currentCount = 1
	m.currentThought = false
}

func normalizeOwnedTextPart(part map[string]any) {
	thought := isTruthy(part["thought"])
	for key := range part {
		if key == "text" || (thought && (key == "thought" || key == "thoughtSignature")) {
			continue
		}
		delete(part, key)
	}
	if thought {
		part["thought"] = true
	}
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
	if m.currentCount == 0 {
		return
	}
	if m.currentCount == 1 && canonicalTextPart(m.currentPart, m.currentText, m.currentThought, m.hasSignature) {
		m.merged = append(m.merged, m.currentPart)
	} else {
		current := make(map[string]any, 3)
		if m.currentCount == 1 {
			current["text"] = m.currentText
		} else {
			current["text"] = m.text.String()
		}
		if m.currentThought {
			current["thought"] = true
			if m.hasSignature {
				current["thoughtSignature"] = m.currentSignature
			}
		}
		m.merged = append(m.merged, current)
	}
	m.currentPart = nil
	m.currentText = ""
	m.currentSignature = nil
	m.currentCount = 0
	m.currentThought = false
	m.hasSignature = false
	m.text.Reset()
}

func canonicalTextPart(part map[string]any, text string, thought, hasSignature bool) bool {
	if part == nil {
		return false
	}
	expectedFields := 1
	if thought {
		expectedFields++
		if hasSignature {
			expectedFields++
		}
	}
	if len(part) != expectedFields {
		return false
	}
	partText, ok := part["text"].(string)
	if !ok || partText != text {
		return false
	}
	if !thought {
		return true
	}
	partThought, ok := part["thought"].(bool)
	if !ok || !partThought {
		return false
	}
	_, partHasSignature := part["thoughtSignature"]
	return partHasSignature == hasSignature
}

// MergeContentBlocks 合并相邻同类型文本块（thought+thought、text+text）。
func MergeContentBlocks(parts []map[string]any) []map[string]any {
	merger := NewContentBlockMerger(len(parts))
	for _, part := range parts {
		merger.Add(part)
	}
	return merger.Result()
}
