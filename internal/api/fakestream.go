package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
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
//
// mu 保护 sw.w / flush / refreshWriteDeadline 的并发访问。startKeepAlive 的
// ping goroutine 会在 StreamChat / CompleteChat 等待期间与主回调 goroutine
// 并发写同一 ResponseWriter；http.ResponseWriter 非并发安全，必须串行化。
// failed 仍用 atomic.Bool 让快速路径只读 1 次就能短路，无需先拿锁。
type sseWriter struct {
	w                    http.ResponseWriter
	flush                func()
	refreshWriteDeadline func()
	mu                   sync.Mutex
	failed               atomic.Bool
}

const maxPooledSSEBufferCapacity = 64 * 1024
const sseWriteTimeout = 30 * time.Second

var sseBufferPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any { return new(bytes.Buffer) },
}

// write 写一条原始字符串并 flush。返回 false 表示客户端断开。
func (sw *sseWriter) write(line string) bool {
	if sw == nil || sw.failed.Load() {
		return false
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.failed.Load() {
		return false
	}
	if sw.refreshWriteDeadline != nil {
		sw.refreshWriteDeadline()
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
	// 注意：caller 已先检查 sw.failed。这里再检查一次拿到锁后状态，避免与
	// startKeepAlive ping goroutine 的失败结果产生交错。
	if sw == nil || sw.failed.Load() {
		buffer.Reset()
		if buffer.Cap() <= maxPooledSSEBufferCapacity {
			sseBufferPool.Put(buffer)
		}
		return false
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.failed.Load() {
		buffer.Reset()
		if buffer.Cap() <= maxPooledSSEBufferCapacity {
			sseBufferPool.Put(buffer)
		}
		return false
	}
	if sw.refreshWriteDeadline != nil {
		sw.refreshWriteDeadline()
	}
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

// startKeepAlive 周期性向 SSE 通道发送注释帧（": ping\n\n"），让 CF / Render /
// SillyTavern 等中间链路看到数据流，从而避免空闲超时主动断开连接。
//
// 假流式与聚合流式场景下，CompleteChat 必须等上游完整生成（尤其开启 HIGH 思考
// 档、或上游节点 429 风暴重试时，等待可能持续数十秒）。期间不写任何字节会让
// CF free 计划的 100s Origin Response Timeout、Render 的请求超时、或客户端
// fetch 超时任意一处先触发，表现为"几十秒后突然中断"。SSE 注释帧（以 ':'
// 开头的行）会被所有合规 SSE 客户端静默丢弃，但会让反向代理与浏览器认为
// 连接活跃。
//
// 关键修复：当客户端（SillyTavern fetch 超时 / 用户主动取消）断开连接后，
// CF 经常不会立即关闭到 origin 的 TCP（尤其 HTTP/2 多路复用、CF 的"读完
// 再丢弃"缓冲策略），导致 Go net/http 长时间不取消 r.Context()。后端
// race engine 持续监听 ctx.Done() 却等不到信号，于是继续多节点 429 重试
// 直到 maxRetries——这正是用户日志中"后端还在继续重试未终止"的根因。
//
// 解决办法：调用方应传入派生 ctx 的 cancel 函数（onDisconnect）。每次写
// ping 失败（TCP RST/EPIPE/超时——说明客户端链路已断）时立即调用 cancel，
// 主动取消派生 ctx，CompleteChat / race engine 立即终止重试，不再浪费
// 上游配额。
//
// interval 应远小于链路最短超时（建议 10s）。返回的 stop 函数必须在
// 真实数据帧开始写之前调用，以避免 keep-alive 帧与正文帧交错。
func (sw *sseWriter) startKeepAlive(ctx context.Context, interval time.Duration, onDisconnect context.CancelFunc) (stop func()) {
	if interval <= 0 || sw == nil {
		return func() {}
	}
	stopped := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopped:
				return
			case <-ticker.C:
				if !sw.write(": ping\n\n") {
					// ping 写失败 = 客户端链路已断。立即取消派生 ctx，
					// 让 CompleteChat / race engine 终止 429 重试循环，
					// 不再继续多节点接力浪费上游配额。
					if onDisconnect != nil {
						onDisconnect()
					}
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stopped) })
	}
}
