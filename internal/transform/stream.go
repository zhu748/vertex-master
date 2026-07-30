package transform

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
)

// FinishReasonUnspecified 是匿名端点每帧携带的 protobuf 默认值。
const FinishReasonUnspecified = "FINISH_REASON_UNSPECIFIED"

var streamCounter uint64 //nolint:gochecknoglobals

// reqID 生成唯一 ID。
func reqID() string {
	var buf [12]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		now := time.Now().UnixNano()
		count := atomic.AddUint64(&streamCounter, 1)
		var fallback [12]byte
		fallback[0] = byte(now >> 56)
		fallback[1] = byte(now >> 48)
		fallback[2] = byte(now >> 40)
		fallback[3] = byte(now >> 32)
		fallback[4] = byte(now >> 24)
		fallback[5] = byte(now >> 16)
		fallback[6] = byte(now >> 8)
		fallback[7] = byte(now)
		fallback[8] = byte(count >> 24)
		fallback[9] = byte(count >> 16)
		fallback[10] = byte(count >> 8)
		fallback[11] = byte(count)
		return hex.EncodeToString(fallback[:])
	}
	return hex.EncodeToString(buf[:])
}

// sseLine 把对象序列化成一条 SSE 数据行。
func sseLine(obj any) string {
	var line strings.Builder
	line.Grow(256)
	line.WriteString("data: ")
	if err := jsonx.Encode(&line, obj); err != nil {
		return "data: {}\n\n"
	}
	line.WriteByte('\n')
	return line.String()
}

// OpenAI 流事件的形状固定，使用结构体可避免 encoding/json 在每个增量上
// 反射遍历多层 map。ToolCalls 和 Usage 保持动态类型，以兼容扩展字段。
type openAIStreamEvent struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []openAIStreamChoice `json:"choices"`
	Usage   *OpenAIUsage         `json:"usage,omitempty"`

	usage OpenAIUsage `json:"-"`
}

type openAIStreamChoice struct {
	Index        int               `json:"index"`
	Delta        openAIStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

type openAIStreamDelta struct {
	Role             string `json:"role,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Content          string `json:"content,omitempty"`
	ToolCalls        []any  `json:"tool_calls,omitempty"`
}

func setOpenAIStreamChoice(event *openAIStreamEvent, delta openAIStreamDelta, finishReason *string) {
	if cap(event.Choices) == 0 {
		event.Choices = make([]openAIStreamChoice, 1)
	} else {
		event.Choices = event.Choices[:1]
	}
	event.Choices[0] = openAIStreamChoice{
		Index:        0,
		Delta:        delta,
		FinishReason: finishReason,
	}
}

type openAIStreamPrepared struct {
	candidate  map[string]any
	usage      map[string]any
	text       string
	reasoning  string
	finish     string
	toolCalls  []any
	eventCount int
	isFirst    bool
	hasFinish  bool
	hasUsage   bool
}

// OpenAIStreamEncoder 在单个请求内复用固定事件和 choices 存储。API 层可把事件
// 直接编码到响应缓冲，避免每个正文 chunk 先创建临时 SSE 字符串。
type OpenAIStreamEncoder struct {
	event             openAIStreamEvent
	nextToolCallIndex int
	sawToolCalls      bool
}

func NewOpenAIStreamEncoder(model, requestID string) *OpenAIStreamEncoder {
	return &OpenAIStreamEncoder{event: openAIStreamEvent{
		ID:      "chatcmpl-" + requestID,
		Object:  "chat.completion.chunk",
		Model:   model,
		Choices: make([]openAIStreamChoice, 1),
	}}
}

func prepareOpenAIStreamChunk(chunk map[string]any, isFirst bool) openAIStreamPrepared {
	candidate := firstCandidate(chunk)
	parts := candidateParts(candidate)
	finish, _ := candidate["finishReason"].(string)
	text, toolCalls, reasoning := ExtractParts(parts, true)
	usageMeta, hasUsage := chunk["usageMetadata"].(map[string]any)
	hasUsage = hasUsage && len(usageMeta) > 0
	hasFinish := finish != "" && finish != FinishReasonUnspecified

	prepared := openAIStreamPrepared{
		candidate: candidate,
		usage:     usageMeta,
		text:      text,
		reasoning: reasoning,
		finish:    finish,
		toolCalls: toolCalls,
		isFirst:   isFirst,
		hasFinish: hasFinish,
		hasUsage:  hasUsage,
	}
	if isFirst {
		prepared.eventCount++
	}
	if reasoning != "" {
		prepared.eventCount++
	}
	if text != "" {
		prepared.eventCount++
	}
	if len(toolCalls) > 0 {
		prepared.eventCount++
	}
	if hasFinish {
		prepared.eventCount++
	}
	if hasUsage {
		prepared.eventCount++
	}
	return prepared
}

// Emit 把 chunk 转换为类型化事件并同步交给 emit。返回 false 表示写出方中止。
func (e *OpenAIStreamEncoder) Emit(
	chunk map[string]any,
	isFirst bool,
	emit func(payload any) bool,
) (StreamEventResult, bool) {
	if e == nil || emit == nil {
		return StreamEventResult{}, false
	}
	prepared := prepareOpenAIStreamChunk(chunk, isFirst)
	result := StreamEventResult{
		HasContent: prepared.text != "" || prepared.reasoning != "" || len(prepared.toolCalls) > 0,
		HasFinish:  prepared.hasFinish,
	}
	return result, e.emitPrepared(prepared, emit)
}

// EmitText 直接编码已经过 Vertex 严格解析的单文本帧。
func (e *OpenAIStreamEncoder) EmitText(
	text, finishReason string,
	isFirst bool,
	emit func(payload any) bool,
) (StreamEventResult, bool) {
	if e == nil || emit == nil {
		return StreamEventResult{}, false
	}
	hasFinish := finishReason != "" && finishReason != FinishReasonUnspecified
	prepared := openAIStreamPrepared{
		text:      text,
		finish:    finishReason,
		isFirst:   isFirst,
		hasFinish: hasFinish,
	}
	if isFirst {
		prepared.eventCount++
	}
	if text != "" {
		prepared.eventCount++
	}
	if hasFinish {
		prepared.eventCount++
	}
	return StreamEventResult{HasContent: text != "", HasFinish: hasFinish},
		e.emitPrepared(prepared, emit)
}

func (e *OpenAIStreamEncoder) emitPrepared(prepared openAIStreamPrepared, emit func(payload any) bool) bool {
	if prepared.eventCount == 0 {
		return true
	}
	e.event.Created = time.Now().Unix()
	e.event.Usage = nil

	if prepared.isFirst {
		setOpenAIStreamChoice(&e.event, openAIStreamDelta{Role: "assistant"}, nil)
		if !emit(&e.event) {
			return false
		}
	}

	if prepared.reasoning != "" {
		setOpenAIStreamChoice(&e.event, openAIStreamDelta{ReasoningContent: prepared.reasoning}, nil)
		if !emit(&e.event) {
			return false
		}
	}
	if prepared.text != "" {
		setOpenAIStreamChoice(&e.event, openAIStreamDelta{Content: prepared.text}, nil)
		if !emit(&e.event) {
			return false
		}
	}
	if len(prepared.toolCalls) > 0 {
		for _, rawToolCall := range prepared.toolCalls {
			if toolCall, ok := rawToolCall.(map[string]any); ok {
				toolCall["index"] = e.nextToolCallIndex
				e.nextToolCallIndex++
			}
		}
		e.sawToolCalls = true
		setOpenAIStreamChoice(&e.event, openAIStreamDelta{ToolCalls: prepared.toolCalls}, nil)
		if !emit(&e.event) {
			return false
		}
	}

	if prepared.hasFinish {
		oaiFinish := MapFinishReason(prepared.finish, e.sawToolCalls)
		setOpenAIStreamChoice(&e.event, openAIStreamDelta{}, &oaiFinish)
		if !emit(&e.event) {
			return false
		}
	}

	// Gemini 经常把 usageMetadata 放在没有 candidates/finishReason 的独立末帧。
	// OpenAI 兼容客户端期望它是 [DONE] 前 choices=[] 的独立统计块；不要把
	// usage 绑定到 finish 帧，否则 ChatBox、SillyTavern 等客户端可能看不到用量。
	if prepared.hasUsage {
		e.event.Choices = e.event.Choices[:0]
		NormalizeUsageForCandidate(prepared.usage, prepared.candidate).FillOpenAIUsage(&e.event.usage)
		e.event.Usage = &e.event.usage
		if !emit(&e.event) {
			return false
		}
	}
	return true
}

// ConvertRealtimeChunk 把单个 Gemini 增量 dict 转为 OAI SSE 事件字符串列表。
// 它保留给自定义转换器和测试；默认 HTTP 流使用 OpenAIStreamEncoder 直写。
func ConvertRealtimeChunk(chunk map[string]any, model, requestID string, isFirst bool) []string {
	prepared := prepareOpenAIStreamChunk(chunk, isFirst)
	if prepared.eventCount == 0 {
		return nil
	}
	encoder := NewOpenAIStreamEncoder(model, requestID)
	events := make([]string, 0, prepared.eventCount)
	encoder.emitPrepared(prepared, func(payload any) bool {
		events = append(events, sseLine(payload))
		return true
	})
	return events
}

type canonicalJSONString string

// CanonicalJSONStringValue reports whether value is JSON text produced by this
// package's serializer. The private dynamic type survives intermediate
// map-based protocol conversion while still encoding exactly like a string.
func CanonicalJSONStringValue(value any) (string, bool) {
	canonical, ok := value.(canonicalJSONString)
	return string(canonical), ok
}

// CanonicalOAIResponseToolCall is the allocation-light representation used by
// the non-streaming Gemini response adapter. Its field order matches the
// encoding/json order of the legacy map representation, preserving wire bytes.
type CanonicalOAIResponseToolCall struct {
	Function CanonicalOAIResponseFunctionCall `json:"function"`
	ID       string                           `json:"id"`
	Type     string                           `json:"type"`
}

type CanonicalOAIResponseFunctionCall struct {
	Arguments string `json:"arguments"`
	Name      string `json:"name"`
}

// ExtractParts 从 Gemini parts 提取 (text_content, tool_calls, reasoning_content)。
func ExtractParts(parts []any, forStream bool) (string, []any, string) {
	return extractParts(parts, forStream, false)
}

func extractResponseParts(parts []any) (string, []any, string) {
	return extractParts(parts, false, true)
}

func extractParts(parts []any, forStream, canonicalToolCalls bool) (string, []any, string) {
	// 绝大多数流帧只有一个纯文本 part；先处理这一形状，避免为两个通用
	// 累积器清零较大的内联存储。工具和图片仍按原优先级走完整路径。
	if len(parts) == 1 {
		if part, ok := parts[0].(map[string]any); ok {
			_, hasFunctionCall := namedFunctionCall(part)
			_, _, hasImage := inlineImageData(part)
			if !hasFunctionCall && !hasImage {
				if text := toString(part["text"]); text != "" {
					if isTruthy(part["thought"]) {
						return "", nil, text
					}
					return text, nil, ""
				}
			}
		}
	}

	var textParts StringAccumulator
	var thoughtParts StringAccumulator
	var toolCalls []any
	type inlineImage struct {
		mime string
		data string
	}
	var images []inlineImage
	imageBytes := 0

	for _, pRaw := range parts {
		part, ok := pRaw.(map[string]any)
		if !ok {
			continue
		}
		if fc, ok := namedFunctionCall(part); ok {
			args := fc["args"]
			if args == nil {
				args = map[string]any{}
			}
			arguments, _ := jsonx.MarshalString(args)
			id := toString(fc["id"])
			if id == "" {
				id = "call_" + reqID()
			}
			name := toString(fc["name"])
			if canonicalToolCalls {
				toolCalls = append(toolCalls, CanonicalOAIResponseToolCall{
					Function: CanonicalOAIResponseFunctionCall{
						Arguments: arguments,
						Name:      name,
					},
					ID:   id,
					Type: "function",
				})
				continue
			}
			tc := map[string]any{
				"index": len(toolCalls),
				"id":    id,
				"type":  "function",
				"function": map[string]any{
					"name":      name,
					"arguments": canonicalJSONString(arguments),
				},
			}
			if !forStream {
				delete(tc, "index")
			}
			toolCalls = append(toolCalls, tc)
			continue
		}
		if mime, data, ok := inlineImageData(part); ok {
			images = append(images, inlineImage{mime: mime, data: data})
			imageBytes += len("\n![image](data:") + len(mime) + len(";base64,") + len(data) + len(")")
			continue
		}

		text := toString(part["text"])
		if text != "" {
			if isTruthy(part["thought"]) {
				thoughtParts.WriteString(text)
			} else {
				textParts.WriteString(text)
			}
			continue
		}
		if hasKey(part, "executableCode") {
			if ec, ok := part["executableCode"].(map[string]any); ok {
				lang := strings.ToLower(toString(ec["codeLanguage"]))
				textParts.WriteString("```")
				textParts.WriteString(lang)
				textParts.WriteString("\n")
				textParts.WriteString(toString(ec["code"]))
				textParts.WriteString("\n```")
			}
		} else if hasKey(part, "codeExecutionResult") {
			if cer, ok := part["codeExecutionResult"].(map[string]any); ok {
				textParts.WriteString("```output\n")
				textParts.WriteString(toString(cer["output"]))
				textParts.WriteString("\n```")
			}
		}
	}

	textContent := ""
	if imageBytes > 0 {
		var textBuilder strings.Builder
		textBuilder.Grow(textParts.Len() + imageBytes)
		textParts.AppendTo(&textBuilder)
		for _, image := range images {
			textBuilder.WriteString("\n![image](data:")
			textBuilder.WriteString(image.mime)
			textBuilder.WriteString(";base64,")
			textBuilder.WriteString(image.data)
			textBuilder.WriteByte(')')
		}
		textContent = textBuilder.String()
	} else {
		textContent = textParts.String()
	}
	reasoning := thoughtParts.String()
	if len(toolCalls) == 0 {
		return textContent, nil, reasoning
	}
	return textContent, toolCalls, reasoning
}

// ---- 响应解析用的小工具 ----

func firstCandidate(resp map[string]any) map[string]any {
	if cands, ok := resp["candidates"].([]any); ok && len(cands) > 0 {
		if c, ok := cands[0].(map[string]any); ok {
			return c
		}
	}
	return map[string]any{}
}

func candidateParts(candidate map[string]any) []any {
	if content, ok := candidate["content"].(map[string]any); ok {
		if parts, ok := content["parts"].([]any); ok {
			return parts
		}
	}
	return nil
}

func namedFunctionCall(part map[string]any) (map[string]any, bool) {
	if fc, ok := part["functionCall"].(map[string]any); ok {
		return fc, truthyStr(fc["name"])
	}
	return nil, false
}

func inlineImageData(part map[string]any) (string, string, bool) {
	if id, ok := part["inlineData"].(map[string]any); ok {
		mime := toString(firstNonEmpty(id["mimeType"], id["mime_type"]))
		data := toString(id["data"])
		return mime, data, mime != "" && data != "" && strings.HasPrefix(mime, "image/")
	}
	return "", "", false
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

func firstNonEmpty(vals ...any) any {
	for _, v := range vals {
		if v != nil && toString(v) != "" {
			return v
		}
	}
	return ""
}

// numOf 把非负、有限且可表示的 JSON 整数转 int，其他值返回 0。
func numOf(v any) int {
	var count int
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || math.Trunc(n) != n ||
			n >= float64(uint64(1)<<(strconv.IntSize-1)) {
			return 0
		}
		count = int(n)
	case int:
		count = n
	case int64:
		if n < 0 || uint64(n) > uint64(math.MaxInt) {
			return 0
		}
		count = int(n)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0
		}
		count = parsed
	default:
		return 0
	}
	if count < 0 {
		return 0
	}
	return count
}
