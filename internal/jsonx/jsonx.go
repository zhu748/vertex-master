// Package jsonx 提供关闭 HTML 转义的 JSON 序列化。
//
// Go 标准库 json.Marshal 默认会把 < > & 转义成 < > &，
// 而我们不做这种转义。为了逐字节稳定（既用于发往上游的请求体，也用于返回给客户端的响应体），
// 这里统一用关闭 HTML 转义的编码器。这是里程碑红线之一。
package jsonx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"sync"
	"unicode/utf8"
	"unsafe"
)

const (
	maxPooledMarshalBufferCapacity = 64 << 10
	maxFastJSONValueDepth          = 64
	maxStackJSONMapKeys            = 16
)

var marshalBufferPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any { return new(bytes.Buffer) },
}

var jsonValueBufferPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any {
		buffer := make([]byte, 0, 512)
		return &buffer
	},
}

// Encode 将 JSON 写入 writer，不做 HTML 转义。与 json.Encoder.Encode 一样，
// 成功时末尾包含一个换行符，适合直接嵌入流式协议缓冲。
func Encode(writer io.Writer, value any) error {
	if !jsonValueRootCanUseFastPath(value) {
		return encode(writer, value, false)
	}
	var writeErr error
	if encodeJSONValuePooled(value, func(encoded []byte) {
		written, err := writer.Write(encoded)
		if written != len(encoded) && err == nil {
			err = io.ErrShortWrite
		}
		writeErr = err
	}) {
		if writeErr != nil {
			return fmt.Errorf("序列化 JSON: %w", writeErr)
		}
		return nil
	}
	return encode(writer, value, false)
}

func jsonValueRootCanUseFastPath(value any) bool {
	switch value.(type) {
	case nil, bool, string, float64, int, int64, []any, map[string]any:
		return true
	default:
		return false
	}
}

func encode(writer io.Writer, value any, escapeHTML bool) error {
	enc := json.NewEncoder(writer)
	enc.SetEscapeHTML(escapeHTML)
	if err := enc.Encode(value); err != nil {
		return fmt.Errorf("序列化 JSON: %w", err)
	}
	return nil
}

// EncodeNoTrailingNewline writes the same bytes as Marshal directly to writer,
// without retaining a second complete encoding buffer. It is intended for
// controlled wire types whose JSON serialization cannot fail after an HTTP
// status has been committed. Unlike Encode, the final newline is withheld.
func EncodeNoTrailingNewline(writer io.Writer, value any) error {
	trimmed := trailingNewlineWriter{writer: writer}
	if err := Encode(&trimmed, value); err != nil {
		return err
	}
	if err := trimmed.finish(); err != nil {
		return fmt.Errorf("写出 JSON: %w", err)
	}
	return nil
}

type trailingNewlineWriter struct {
	writer  io.Writer
	pending [1]byte
	hasByte bool
}

func (w *trailingNewlineWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if w.hasByte {
		written, err := w.writer.Write(w.pending[:])
		if written != 1 {
			if err == nil {
				err = io.ErrShortWrite
			}
			return 0, err
		}
		if err != nil {
			return 0, err
		}
		w.hasByte = false
	}
	if len(data) > 1 {
		written, err := w.writer.Write(data[:len(data)-1])
		if written != len(data)-1 {
			if err == nil {
				err = io.ErrShortWrite
			}
			return written, err
		}
		if err != nil {
			return written, err
		}
	}
	w.pending[0] = data[len(data)-1]
	w.hasByte = true
	return len(data), nil
}

func (w *trailingNewlineWriter) finish() error {
	if !w.hasByte || w.pending[0] == '\n' {
		return nil
	}
	written, err := w.writer.Write(w.pending[:])
	if written != 1 && err == nil {
		return io.ErrShortWrite
	}
	return err
}

// Marshal 序列化为 JSON，不做 HTML 转义、不转义非 ASCII。
func Marshal(v any) ([]byte, error) {
	buf, view, err := encodeToMarshalBuffer(v, false)
	if err != nil {
		return nil, err
	}
	encoded := bytes.Clone(view)
	releaseMarshalBuffer(buf)
	return encoded, nil
}

// MarshalString 序列化为 JSON 字符串。与 Marshal 后再转换 string 相比，
// 它只复制一次编码结果，适合函数参数等最终必须是 JSON 字符串的协议字段。
func MarshalString(v any) (string, error) {
	buf, view, err := encodeToMarshalBuffer(v, false)
	if err != nil {
		return "", err
	}
	encoded := string(view)
	releaseMarshalBuffer(buf)
	return encoded, nil
}

// MarshalJSONValue serializes the finite value set produced by decoding
// arbitrary JSON into any: nil, bool, string, float64, []any and map[string]any.
// It avoids reflection and returns ok=false for custom types, non-finite numbers,
// cycles and unusually deep values so callers can fall back to Marshal.
func MarshalJSONValue(value any) (encoded []byte, ok bool) {
	buffer := make([]byte, 0, 64)
	buffer, ok = AppendJSONValue(buffer, value)
	if !ok {
		return nil, false
	}
	return buffer, true
}

// MarshalHTMLJSONValue is the standard-HTML-escaping counterpart of
// MarshalJSONValue. It keeps encoding/json.Marshal-compatible bytes while
// avoiding reflection for values decoded into generic JSON containers.
func MarshalHTMLJSONValue(value any) (encoded []byte, ok bool) {
	size, ok := jsonValueEncodedSizeMode(value, 0, true)
	if !ok {
		return nil, false
	}
	buffer := make([]byte, 0, size)
	buffer, ok = appendJSONValueMode(buffer, value, 0, true, false)
	if !ok || len(buffer) != size {
		return nil, false
	}
	return buffer, true
}

// MarshalHTMLJSONValueView is the pooled callback form of
// MarshalHTMLJSONValue. The view is read-only and valid only for the duration
// of consume. consume is not called when value is outside the supported
// decoded-JSON value set.
func MarshalHTMLJSONValueView(value any, consume func([]byte)) bool {
	size, ok := jsonValueEncodedSizeMode(value, 0, true)
	if !ok {
		return false
	}
	buf := marshalBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer releaseMarshalBuffer(buf)

	buf.Grow(size)
	view := buf.AvailableBuffer()
	view, ok = appendJSONValueMode(view, value, 0, true, false)
	if !ok || len(view) != size {
		return false
	}
	_, _ = buf.Write(view)
	consume(buf.Bytes())
	return true
}

// AppendJSONValue appends the finite decoded-JSON value set supported by
// MarshalJSONValue to dst. On failure it returns dst unchanged. Callers can
// therefore pack several independent values into one backing buffer and retain
// read-only slices for each encoded value.
func AppendJSONValue(dst []byte, value any) (encoded []byte, ok bool) {
	encoded, ok = appendJSONValue(dst, value, 0)
	if !ok {
		return dst, false
	}
	return encoded, true
}

// MarshalJSONValueString is the string-returning form of MarshalJSONValue. It
// sizes the result first so arbitrary decoded JSON values still need only one
// allocation.
func MarshalJSONValueString(value any) (encoded string, ok bool) {
	size, ok := jsonValueEncodedSize(value, 0)
	if !ok {
		return "", false
	}
	buffer := make([]byte, 0, size)
	buffer, ok = appendJSONValue(buffer, value, 0)
	if !ok || len(buffer) != size {
		return "", false
	}
	// buffer is newly allocated to its exact final size and is not exposed or
	// mutated after publication, so its backing storage can safely become the
	// immutable result string without another complete copy.
	return unsafe.String(unsafe.SliceData(buffer), len(buffer)), true
}

func jsonValueEncodedSize(value any, depth int) (int, bool) {
	return jsonValueEncodedSizeMode(value, depth, false)
}

func jsonValueEncodedSizeMode(value any, depth int, escapeHTML bool) (int, bool) {
	if depth > maxFastJSONValueDepth {
		return 0, false
	}
	switch typed := value.(type) {
	case nil:
		return len("null"), true
	case bool:
		if typed {
			return len("true"), true
		}
		return len("false"), true
	case string:
		return jsonStringEncodedSizeMode(typed, escapeHTML), true
	case float64:
		if math.IsInf(typed, 0) || math.IsNaN(typed) {
			return 0, false
		}
		var scratch [32]byte
		return len(appendJSONFloat(scratch[:0], typed)), true
	case []any:
		if typed == nil {
			return len("null"), true
		}
		size := 2
		for index, item := range typed {
			itemSize, itemOK := jsonValueEncodedSizeMode(item, depth+1, escapeHTML)
			if !itemOK || size > int(^uint(0)>>1)-itemSize {
				return 0, false
			}
			size += itemSize
			if index > 0 {
				size++
			}
		}
		return size, true
	case map[string]any:
		if typed == nil {
			return len("null"), true
		}
		size := 2
		index := 0
		for key, item := range typed {
			keySize := jsonStringEncodedSizeMode(key, escapeHTML)
			itemSize, itemOK := jsonValueEncodedSizeMode(item, depth+1, escapeHTML)
			if !itemOK || keySize > int(^uint(0)>>1)-itemSize-1 ||
				size > int(^uint(0)>>1)-keySize-itemSize-1 {
				return 0, false
			}
			size += keySize + 1 + itemSize
			if index > 0 {
				size++
			}
			index++
		}
		return size, true
	default:
		return 0, false
	}
}

func appendJSONValue(buffer []byte, value any, depth int) ([]byte, bool) {
	return appendJSONValueMode(buffer, value, depth, false, false)
}

func appendJSONValueMode(
	buffer []byte,
	value any,
	depth int,
	escapeHTML bool,
	allowIntegers bool,
) ([]byte, bool) {
	if depth > maxFastJSONValueDepth {
		return buffer, false
	}
	switch typed := value.(type) {
	case nil:
		return append(buffer, "null"...), true
	case bool:
		return strconv.AppendBool(buffer, typed), true
	case string:
		return appendJSONStringMode(buffer, typed, escapeHTML), true
	case float64:
		if math.IsInf(typed, 0) || math.IsNaN(typed) {
			return buffer, false
		}
		return appendJSONFloat(buffer, typed), true
	case int:
		if !allowIntegers {
			return buffer, false
		}
		return strconv.AppendInt(buffer, int64(typed), 10), true
	case int64:
		if !allowIntegers {
			return buffer, false
		}
		return strconv.AppendInt(buffer, typed, 10), true
	case []any:
		if typed == nil {
			return append(buffer, "null"...), true
		}
		buffer = append(buffer, '[')
		for index, item := range typed {
			if index > 0 {
				buffer = append(buffer, ',')
			}
			var ok bool
			buffer, ok = appendJSONValueMode(
				buffer,
				item,
				depth+1,
				escapeHTML,
				allowIntegers,
			)
			if !ok {
				return buffer, false
			}
		}
		return append(buffer, ']'), true
	case map[string]any:
		if typed == nil {
			return append(buffer, "null"...), true
		}
		var stackKeys [maxStackJSONMapKeys]string
		keys := stackKeys[:0]
		if len(typed) > len(stackKeys) {
			keys = make([]string, 0, len(typed))
		}
		for key := range typed {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		buffer = append(buffer, '{')
		for index, key := range keys {
			if index > 0 {
				buffer = append(buffer, ',')
			}
			buffer = appendJSONStringMode(buffer, key, escapeHTML)
			buffer = append(buffer, ':')
			var ok bool
			buffer, ok = appendJSONValueMode(
				buffer,
				typed[key],
				depth+1,
				escapeHTML,
				allowIntegers,
			)
			if !ok {
				return buffer, false
			}
		}
		return append(buffer, '}'), true
	default:
		return buffer, false
	}
}

func appendJSONFloat(buffer []byte, value float64) []byte {
	format := byte('f')
	absolute := math.Abs(value)
	if absolute != 0 && (absolute < 1e-6 || absolute >= 1e21) {
		format = 'e'
	}
	buffer = strconv.AppendFloat(buffer, value, format, -1, 64)
	if format == 'e' {
		length := len(buffer)
		if length >= 4 && buffer[length-4] == 'e' && buffer[length-3] == '-' && buffer[length-2] == '0' {
			buffer[length-2] = buffer[length-1]
			buffer = buffer[:length-1]
		}
	}
	return buffer
}

func jsonStringEncodedSize(value string) int {
	return jsonStringEncodedSizeMode(value, false)
}

func jsonStringEncodedSizeMode(value string, escapeHTML bool) int {
	size := 2
	for index := 0; index < len(value); {
		char := value[index]
		if char < utf8.RuneSelf {
			switch char {
			case '\\', '"', '\b', '\f', '\n', '\r', '\t':
				size += 2
			default:
				if char < 0x20 || escapeHTML && (char == '<' || char == '>' || char == '&') {
					size += 6
				} else {
					size++
				}
			}
			index++
			continue
		}
		runeValue, runeSize := utf8.DecodeRuneInString(value[index:])
		switch {
		case runeValue == utf8.RuneError && runeSize == 1:
			size += len(`\ufffd`)
		case runeValue == '\u2028' || runeValue == '\u2029':
			size += 6
		default:
			size += runeSize
		}
		index += runeSize
	}
	return size
}

func appendJSONString(buffer []byte, value string) []byte {
	return appendJSONStringMode(buffer, value, false)
}

func appendJSONStringMode(buffer []byte, value string, escapeHTML bool) []byte {
	buffer = append(buffer, '"')
	start := 0
	for index := 0; index < len(value); {
		char := value[index]
		if char < utf8.RuneSelf {
			if char >= 0x20 && char != '\\' && char != '"' &&
				(!escapeHTML || char != '<' && char != '>' && char != '&') {
				index++
				continue
			}
			buffer = append(buffer, value[start:index]...)
			switch char {
			case '\\', '"':
				buffer = append(buffer, '\\', char)
			case '\b':
				buffer = append(buffer, '\\', 'b')
			case '\f':
				buffer = append(buffer, '\\', 'f')
			case '\n':
				buffer = append(buffer, '\\', 'n')
			case '\r':
				buffer = append(buffer, '\\', 'r')
			case '\t':
				buffer = append(buffer, '\\', 't')
			default:
				const hex = "0123456789abcdef"
				buffer = append(buffer, '\\', 'u', '0', '0', hex[char>>4], hex[char&0xf])
			}
			index++
			start = index
			continue
		}
		runeValue, runeSize := utf8.DecodeRuneInString(value[index:])
		if runeValue == utf8.RuneError && runeSize == 1 {
			buffer = append(buffer, value[start:index]...)
			buffer = append(buffer, `\ufffd`...)
			index += runeSize
			start = index
			continue
		}
		if runeValue == '\u2028' || runeValue == '\u2029' {
			buffer = append(buffer, value[start:index]...)
			buffer = append(buffer, '\\', 'u', '2', '0', '2', byte('8'+runeValue-'\u2028'))
			index += runeSize
			start = index
			continue
		}
		index += runeSize
	}
	buffer = append(buffer, value[start:]...)
	return append(buffer, '"')
}

// MarshalView serializes v and passes a read-only view of the pooled encoding
// buffer to consume. The view is valid only for the duration of consume and
// must not be retained. Unlike Marshal, this avoids copying the complete JSON
// payload when the consumer can finish synchronously, such as an HTTP Write.
// consume is called only after serialization succeeds.
func MarshalView(v any, consume func([]byte)) error {
	buf, view, err := encodeToMarshalBuffer(v, false)
	if err != nil {
		return err
	}
	defer releaseMarshalBuffer(buf)
	consume(view)
	return nil
}

func encodeJSONValuePooled(value any, consume func([]byte)) bool {
	bufferPointer := jsonValueBufferPool.Get().(*[]byte)
	buffer := (*bufferPointer)[:0]
	encoded, ok := appendJSONValueMode(buffer, value, 0, false, true)
	if ok {
		encoded = append(encoded, '\n')
	}
	defer func() {
		if cap(encoded) <= maxPooledMarshalBufferCapacity {
			*bufferPointer = encoded[:0]
			jsonValueBufferPool.Put(bufferPointer)
		}
	}()
	if ok {
		consume(encoded)
	}
	return ok
}

// MarshalHTMLView is the zero-copy callback form of encoding/json.Marshal.
// It retains standard HTML escaping for formats whose existing canonical bytes
// depend on it. The view is valid only while consume is running.
func MarshalHTMLView(v any, consume func([]byte)) error {
	buf, view, err := encodeToMarshalBuffer(v, true)
	if err != nil {
		return err
	}
	defer releaseMarshalBuffer(buf)
	consume(view)
	return nil
}

func encodeToMarshalBuffer(v any, escapeHTML bool) (*bytes.Buffer, []byte, error) {
	buf := marshalBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	if err := encode(buf, v, escapeHTML); err != nil {
		releaseMarshalBuffer(buf)
		return nil, nil, err
	}
	// json.Encoder.Encode 会在末尾追加一个换行符，去掉以与 json.Marshal 输出一致。
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return buf, b, nil
}

func releaseMarshalBuffer(buf *bytes.Buffer) {
	if buf.Cap() > maxPooledMarshalBufferCapacity {
		return
	}
	buf.Reset()
	marshalBufferPool.Put(buf)
}

// Truthy 复刻动态语言常见的真值语义，用于判断解析出的 JSON 值是否"为真"
// （nil/false/空串/0/空数组/空对象为假，其余为真）。集中一处，避免各包重复实现导致语义漂移。
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	default:
		return true
	}
}
