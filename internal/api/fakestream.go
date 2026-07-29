package api

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
)

// 本文件实现假流式：模型名带 "假流式-"/"fake-" 前缀时，先完整非流式生成、再切片按 SSE 推。
// OpenAI 端点与 Gemini 端点（use_fake 分支）共用此机制。

const fakeStreamTargetChunks = 8

// splitIntoRuneChunks 把文本切成若干分片用于假流式推送，分片数不超过 8。
//
// 必须按 rune（完整字符）切分，不能按字节：多字节 UTF-8 字符（如汉字、emoji）若在字节
// 边界被截断，半个字符经 JSON 序列化会被替换成 U+FFFD（），客户端收到的就是乱码。
// 空文本返回 nil。
func splitIntoRuneChunks(text string) []string {
	if text == "" {
		return nil
	}
	chunkBytes := (len(text)-1)/fakeStreamTargetChunks + 1
	chunks := make([]string, 0, fakeStreamTargetChunks)
	for start := 0; start < len(text); {
		end := min(start+chunkBytes, len(text))
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end++
		}
		chunks = append(chunks, text[start:end])
		start = end
	}
	return chunks
}

// sseWriter 是一个带 flush 的 SSE 行写出器；write 返回 false 表示客户端断开。
type sseWriter struct {
	w      http.ResponseWriter
	flush  func()
	failed atomic.Bool
}

const maxPooledSSEBufferCapacity = 64 * 1024

var sseBufferPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any { return new(bytes.Buffer) },
}

// write 写一条原始字符串并 flush。返回 false 表示客户端断开。
func (sw *sseWriter) write(line string) bool {
	if sw == nil || sw.failed.Load() {
		return false
	}
	if _, err := io.WriteString(sw.w, line); err != nil {
		sw.failed.Store(true)
		return false
	}
	if sw.flush != nil {
		sw.flush()
	}
	return true
}

// writeNamed 先在复用缓冲中完整序列化命名 SSE，再一次性写出。这样既能在
// JSON 失败时保持原有的完整错误帧，也不会为每个流式增量创建临时大字符串。
func (sw *sseWriter) writeNamed(event string, payload any) bool {
	if sw == nil || sw.failed.Load() {
		return false
	}
	buffer := sseBufferPool.Get().(*bytes.Buffer)
	buffer.Reset()
	buffer.Grow(len(event) + 256)
	buffer.WriteString("event: ")
	buffer.WriteString(event)
	buffer.WriteString("\ndata: ")
	if err := jsonx.Encode(buffer, payload); err != nil {
		buffer.Reset()
		buffer.WriteString("event: ")
		buffer.WriteString(event)
		buffer.WriteString("\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"serialization failed\"}}\n")
	}
	buffer.WriteByte('\n')
	return sw.writeBuffer(buffer)
}

// writeData 写出没有 event 字段的标准 SSE data 帧，供 Gemini/OpenAI 兼容
// 流复用。payload 会在任何网络写入发生前完成序列化。
func (sw *sseWriter) writeData(payload any) bool {
	if sw == nil || sw.failed.Load() {
		return false
	}
	buffer := sseBufferPool.Get().(*bytes.Buffer)
	buffer.Reset()
	buffer.Grow(256)
	buffer.WriteString("data: ")
	if err := jsonx.Encode(buffer, payload); err != nil {
		buffer.Reset()
		buffer.WriteString("data: {}\n")
	}
	buffer.WriteByte('\n')
	return sw.writeBuffer(buffer)
}

func (sw *sseWriter) writeBuffer(buffer *bytes.Buffer) bool {
	_, err := sw.w.Write(buffer.Bytes())
	if buffer.Cap() <= maxPooledSSEBufferCapacity {
		buffer.Reset()
		sseBufferPool.Put(buffer)
	}
	if err != nil {
		sw.failed.Store(true)
		return false
	}
	if sw.flush != nil {
		sw.flush()
	}
	return true
}
