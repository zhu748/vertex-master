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
	"sync"
)

const maxPooledMarshalBufferCapacity = 64 << 10

var marshalBufferPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any { return new(bytes.Buffer) },
}

// Encode 将 JSON 写入 writer，不做 HTML 转义。与 json.Encoder.Encode 一样，
// 成功时末尾包含一个换行符，适合直接嵌入流式协议缓冲。
func Encode(writer io.Writer, value any) error {
	return encode(writer, value, false)
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
