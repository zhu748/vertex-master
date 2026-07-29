package vertex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/spool"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

// StreamChunk 是真流式中 yield 的单个增量。要么是 Gemini 数据 chunk，要么是错误。
//
// 正常 yield Gemini dict，所有重试耗尽时 yield {"error": {...}}（routes 层据此
// 发 OAI error 事件 + [DONE]）。
type StreamChunk struct {
	// Data 是清洗后的 Gemini 增量（candidates/usageMetadata/...），Err==nil 时有效。
	// 所有权随 chunk 转移给单一消费者；回调可以就地修改，发送方不会再复用该 map。
	Data map[string]any
	// Err 非 nil 表示重试耗尽、对外报错（yield error dict）。
	Err *VertexError
}

const (
	upstreamErrorBodyMaxBytes = 1 << 20
	maxStreamObjectBytes      = 64 << 20
	maxRetainedStreamBuffer   = 4 << 20
	initialStreamBufferBytes  = 16 << 10
	maxPooledStreamBuffer     = 64 << 10
)

var errStreamObjectTooLarge = errors.New("upstream stream object too large")

// StreamChat 真流式入口。
//
// 通过 yield 回调推送增量：回调返回 false 表示客户端断开/上层要求停止，立即终止。
// 单 session 复用；提前停止时立即关闭 response。重试逻辑与非流式对齐，但 content_yielded 后
// 不再重试（已发出的内容不能重来）。ctx 取消（客户端断开/关闭）时干净结束流：
// 重试退避被打断、上游流连接中断，不再空转。
func (c *VertexAIClient) StreamChat(ctx context.Context, model string, geminiPayload map[string]any, yield func(StreamChunk) bool) {
	preparedVariables := buildRequestVariables(model, geminiPayload, c.cfg)
	op := func(ctx context.Context, proxyURI string) <-chan StreamChunk {
		ch := make(chan StreamChunk, 64)
		// 候选共享请求级只读 variables；每次 attempt 只复制顶层并注入 token。
		go func() {
			defer close(ch)
			c.executeStreamingWithPreparedVariables(ctx, model, preparedVariables, proxyURI, func(chunk StreamChunk) bool {
				select {
				case ch <- chunk:
					return true
				case <-ctx.Done():
					return false
				}
			})
		}()
		return ch
	}
	StreamParallel(ctx, c.cfg, op, yield)
}

func (c *VertexAIClient) executeStreamingWithRetries(ctx context.Context, model string, geminiPayload map[string]any, proxyURI string, yield func(StreamChunk) bool) {
	c.executeStreamingWithPreparedVariables(
		ctx,
		model,
		buildRequestVariables(model, geminiPayload, c.cfg),
		proxyURI,
		yield,
	)
}

func (c *VertexAIClient) executeStreamingWithPreparedVariables(ctx context.Context, model string, preparedVariables map[string]any, proxyURI string, yield func(StreamChunk) bool) {
	cfg := c.cfg
	maxRetries := effectiveMaxRetries(
		cfg.MaxRetries(),
		cfg.ParallelPoolEnabled(),
		cfg.ParallelPoolRetryEnabled(),
	)
	contentYielded := false
	var lastError *VertexError

	reqID := RequestIDFromContext(ctx)
	nodeName := nodes.GetNodeName(proxyURI)
	sess, err := c.net.CreateSession(cfg.RequestTimeout(), proxyURI, reqID)
	if err != nil {
		yield(StreamChunk{Err: NewInternalError("create session: " + err.Error())})
		return
	}
	// Capture sess at return time. The 429 path replaces this variable, while a
	// direct defer sess.Close() would only close the first session.
	defer func() { sess.Close() }()

	recaptchaToken := ""
	isFirstAuth := true
	attempt := 0

retryLoop:
	for attempt <= maxRetries {
		log.Printf("[Vertex] [StreamChat] 开始尝试 (Attempt %d/%d), 模型=%s, 请求ID=%s, 代理=%s", attempt, maxRetries, model, reqID, nodeName)
		if recaptchaToken == "" {
			tok, tokenErr := c.pool.GetTokenWithProxyContext(ctx, proxyURI)
			if tokenErr != nil && ctx.Err() != nil {
				return
			}
			recaptchaToken = tok
			isFirstAuth = true
		}
		if recaptchaToken == "" {
			if attempt == maxRetries {
				lastError = NewAuthenticationError("Could not fetch recaptcha token.")
				break retryLoop
			}
			attempt++
			if err := sleepCtx(ctx, time.Second); err != nil {
				break retryLoop // ctx 取消：客户端已断开，停止重试
			}
			continue
		}

		// 单次流式尝试：把增量 yield 给上层，统计本次 attempt yield 的 chunk 数。
		// 与 c6f6b65 行为对齐：所有上游 chunk（包括带 promptFeedback.blockReason
		// 的）都直接 yield 给客户端，不再做"语义重试"。
		// 之前的语义重试逻辑会误把匿名 Gemini 在正常响应里附带的
		// BLOCKED_REASON_UNSPECIFIED 字段当成真正拦截，提前 return false 中断流，
		// 导致后续真正的内容 chunk 永远收不到 —— 客户端表现为 200 OK 但无内容。
		chunkCount := 0
		attemptErr := c.executeStreamingAttempt(ctx, sess, model, nodeName, preparedVariables, recaptchaToken, isFirstAuth, func(ch map[string]any) bool {
			chunkCount++
			contentYielded = true
			return yield(StreamChunk{Data: ch})
		})

		if attemptErr == nil {
			// 本次尝试无错误。若 0 chunk 且仍是首帧 → 认证重试（同 token 再打一次）。
			if chunkCount == 0 && isFirstAuth {
				isFirstAuth = false
				if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
					break retryLoop
				}
				continue
			}
			return
		}

		ve := asVertexError(attemptErr)
		switch {
		case ve != nil && ve.Kind == "auth":
			isVerifyFail := strings.Contains(ve.Message, "Failed to verify action") ||
				strings.Contains(ve.Message, "The caller does not have permission")
			if isFirstAuth && isVerifyFail {
				// 首次认证重试：token 不清空，同一 token 再打一次（匿名端点首帧预期 verify-fail）。
				isFirstAuth = false
				if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
					break retryLoop
				}
				continue
			}
			recaptchaToken = ""
			isFirstAuth = true
			lastError = ve
			if contentYielded || attempt >= maxRetries {
				break retryLoop
			}
			attempt++
			if err := sleepCtx(ctx, time.Second); err != nil {
				break retryLoop
			}

		case ve != nil && ve.Kind == "ratelimit":
			lastError = ve
			if contentYielded || attempt >= maxRetries {
				log.Printf("[Vertex] [StreamChat] (Attempt %d/%d) 节点 %s 触发 429 失败, 请求ID=%s, 代理=%s", attempt, maxRetries, model, reqID, nodeName)
				break retryLoop
			}
			// 429：销毁旧 session 重建新的，换 token。
			sess.Close()
			newSess, e := c.net.CreateSession(cfg.RequestTimeout(), proxyURI, reqID)
			if e != nil {
				yield(StreamChunk{Err: NewInternalError("recreate session: " + e.Error())})
				return
			}
			sess = newSess
			recaptchaToken = ""

			// 避免过快重试 429 导致 token 浪费 and 节点持续封禁
			wait := ve.RetryAfter
			if wait <= 0 {
				wait = min(10, 1+attempt)
			}
			log.Printf("[Vertex] [StreamChat] (Attempt %d/%d) 节点 %s 触发 429 将重试 (延迟 %ds), 请求ID=%s, 代理=%s", attempt, maxRetries, model, wait, reqID, nodeName)
			attempt++
			if err := sleepCtx(ctx, time.Duration(wait)*time.Second); err != nil {
				break retryLoop
			}

		case ve != nil:
			lastError = ve
			// 【关键改动】：如果是网络不通等内部错误，直接熔断并停止重试。
			if ve.Kind == "internal" || !ve.IsRetryable() || contentYielded || attempt >= maxRetries {
				log.Printf("[Vertex] [StreamChat] (Attempt %d/%d) 节点 %s 触发异常错误失败: [%s] %s, 请求ID=%s, 代理=%s", attempt, maxRetries, model, ve.Kind, ve.Message, reqID, nodeName)
				break retryLoop
			}
			log.Printf("[Vertex] [StreamChat] (Attempt %d/%d) 节点 %s 触发异常错误将重试: [%s] %s, 请求ID=%s, 代理=%s", attempt, maxRetries, model, ve.Kind, ve.Message, reqID, nodeName)
			attempt++
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				break retryLoop
			}

		default:
			// 【关键改动】：直接终止未知原生错误，移除了多余的 attempt 重新入圈重试。
			lastError = NewInternalError(attemptErr.Error())
			break retryLoop
		}
	}

	// 所有重试耗尽且没发出过任何内容 → yield 一个 error chunk（末尾 yield error dict）。
	if !contentYielded && lastError != nil {
		yield(StreamChunk{Err: lastError})
	}
}

func effectiveMaxRetries(configured int, parallelPoolEnabled, parallelPoolRetryEnabled bool) int {
	if configured < 0 || (parallelPoolEnabled && !parallelPoolRetryEnabled) {
		return 0
	}
	return configured
}

// executeStreamingAttempt 执行单次流式请求：发请求 → 增量扫描 JSON → 提取 chunk。
//
// emit 回调把清洗后的 Gemini chunk 推给上层；
// emit 返回 false（客户端断开）时扫描正常停止、返回 nil（StreamChat 据 chunkCount>0 收尾，不重试）。
// ctx 绑定 to 上游流连接：ctx 取消时 Body.Read 报错，scanStream 干净结束（返回 nil，不 panic）。
func (c *VertexAIClient) executeStreamingAttempt(ctx context.Context, sess *transport.Session, model, nodeName string, preparedVariables map[string]any, recaptchaToken string, _ bool, emit func(map[string]any) bool) error {
	reqID := RequestIDFromContext(ctx)
	log.Printf("[Vertex] [executeStreamingAttempt] 准备发送流式请求: 模型=%s, 请求ID=%s, 代理=%s", model, reqID, nodeName)
	cfg := c.cfg
	newBody := buildRequestPayloadFromVariables(preparedVariables, recaptchaToken)
	// 上游请求 payload 序列化到 spool 缓冲（大媒体自动落盘）。流式：请求体在 DoStream 发送期被读取，
	// 缓冲存活到本函数返回（整个流消费完）后由 defer Close 删除临时文件。
	buf, err := spool.EncodeJSON(newBody)
	if err != nil {
		return NewInternalError("marshal payload: " + err.Error())
	}
	defer func() { _ = buf.Close() }()
	reader, err := buf.Reader()
	if err != nil {
		return NewInternalError("spool reader: " + err.Error())
	}
	header := transport.XHRHeaders(
		"application/json", "*/*",
		"https://console.cloud.google.com", "https://console.cloud.google.com/", "cross-site",
	)

	sr, err := sess.DoStream(ctx, "POST", c.getBatchGraphqlURL(), header, reader)
	if err != nil {
		return NewInternalError("upstream request: " + err.Error())
	}
	defer sr.Close() // 提前停止或出错时立即中断上游 body。

	// HTTP 错误：读完 error body 后按状态映射（与非流式 executeCompleteRequest 一致）。
	if sr.StatusCode != 200 {
		errorBody, readErr := transport.ReadAllLimit(sr.Body, upstreamErrorBodyMaxBytes)
		errText := string(errorBody)
		if errors.Is(readErr, transport.ErrResponseBodyTooLarge) {
			sr.Close()
			errText += "\n[upstream error body truncated]"
		} else if readErr != nil {
			return NewInternalError("read upstream error response: " + readErr.Error())
		}
		if cfg.DebugMode() {
			debugReq, _ := json.Marshal(newBody)
			log.Printf("[DEBUG] [StreamChat] HTTP 报错! 状态码: %d", sr.StatusCode)
			log.Printf("[DEBUG] [StreamChat] 完整请求体: %s", string(debugReq))
			log.Printf("[DEBUG] [StreamChat] 上游回复: %s", errText)
		} else if sr.StatusCode == 400 {
			debugBody, _ := json.Marshal(newBody.Variables)
			log.Printf("[Vertex] [Stream] 收到 400 Bad Request, Variables Payload: %s", string(debugBody))
		}

		if sr.StatusCode == 401 || sr.StatusCode == 403 ||
			strings.Contains(errText, "Failed to verify action") ||
			strings.Contains(errText, "The caller does not have permission") {
			return NewAuthenticationError("Authentication/Recaptcha failed: " + errText)
		}
		if parsed := parseErrorResponse(errText); parsed != nil {
			parsed.UpstreamResponse = errText
			return parsed
		}
		return raiseForStatus(sr.StatusCode, "", "Upstream Error: "+errText, nil, errText)
	}

	// 增量扫描上游流，逐个完整 JSON 对象提取 chunk。不能在真实 finishReason 后立即
	// 停止：Gemini 可能把 usageMetadata 放在随后的独立末帧，因此正常路径仍读到 EOF。
	scanErr := scanStreamRaw(sr.Body, func(raw []byte) (stop bool, err error) {
		// 从单个上游对象提取（可能多个）chunk，逐个 emit。
		return processStreamingJSON(raw, emit)
	})

	if scanErr != nil && cfg.DebugMode() && !errors.Is(scanErr, context.Canceled) {
		debugReq, _ := json.Marshal(newBody)
		log.Printf("[DEBUG] [StreamChat] 扫描流数据报错! error: %v", scanErr)
		log.Printf("[DEBUG] [StreamChat] 完整请求体: %s", string(debugReq))
	}
	if errors.Is(scanErr, errStreamObjectTooLarge) {
		sr.Close()
	}

	if errors.Is(scanErr, context.Canceled) {
		return scanErr
	}

	return scanErr
}

// scanStream 跨 chunk 增量扫描花括号配对，逐个完整 JSON 对象回调 onObject（O(n)）。
//
// M27 增量扫描：
// 跨网络 chunk 维护 scanPos/braceCount/inString/escape 状态，下个 chunk 从上次扫到的位置
// 续扫，而非每来一个 chunk 都从 startIdx 重扫整个 buffer（旧逻辑 O(n²）。逐字节逻辑等价。
//
// onObject 返回 (stop, err)：stop=true（客户端断开）即正常结束扫描；
// err 非 nil 即中断并上抛（上游错误）。
var scanBufPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any {
		buf := make([]byte, initialStreamBufferBytes)
		return &buf
	},
}

var streamObjectBufferPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any {
		buf := make([]byte, 0, initialStreamBufferBytes)
		return &buf
	},
}

func scanStream(body io.Reader, onObject func(map[string]any) (bool, error)) error {
	return scanStreamWithLimit(body, maxStreamObjectBytes, onObject)
}

func scanStreamWithLimit(
	body io.Reader,
	maxObjectBytes int,
	onObject func(map[string]any) (bool, error),
) error {
	return scanStreamRawWithLimit(body, maxObjectBytes, func(raw []byte) (bool, error) {
		object := parseJSONObject(raw)
		if object == nil {
			return false, nil
		}
		return onObject(object)
	})
}

func scanStreamRaw(body io.Reader, onObject func([]byte) (bool, error)) error {
	return scanStreamRawWithLimit(body, maxStreamObjectBytes, onObject)
}

func scanStreamRawWithLimit(
	body io.Reader,
	maxObjectBytes int,
	onObject func([]byte) (bool, error),
) error {
	readBufPtr := scanBufPool.Get().(*[]byte)
	defer scanBufPool.Put(readBufPtr)
	readBuf := *readBufPtr

	bufferPtr := streamObjectBufferPool.Get().(*[]byte)
	buffer := (*bufferPtr)[:0]
	defer func() {
		if cap(buffer) >= initialStreamBufferBytes && cap(buffer) <= maxPooledStreamBuffer {
			*bufferPtr = buffer[:0]
			streamObjectBufferPool.Put(bufferPtr)
		}
	}()

	scanPos := 0   // 已扫到的位置（buffer 内），下个网络 chunk 从这里续扫。
	startIdx := -1 // 当前对象的起始 '{' 位置；-1 表示正在寻找新对象。
	braceCount := 0
	inString := false
	escape := false

	for {
		n, readErr := body.Read(readBuf)
		if n > 0 {
			buffer = append(buffer, readBuf[:n]...)

			for {
				if startIdx < 0 {
					// 找下一个对象的起始 '{'。
					relativeStart := bytes.IndexByte(buffer[scanPos:], '{')
					if relativeStart == -1 {
						buffer = buffer[:0]
						scanPos = 0
						break
					}
					startIdx = scanPos + relativeStart
					scanPos = startIdx
					braceCount = 0
					inString = false
					escape = false
				}

				endIdx := -1
				for i := scanPos; i < len(buffer); i++ {
					ch := buffer[i]
					if escape {
						escape = false
						continue
					}
					if ch == '\\' {
						escape = true
						continue
					}
					if ch == '"' {
						inString = !inString
						continue
					}
					if !inString {
						if ch == '{' {
							braceCount++
						} else if ch == '}' {
							braceCount--
							if braceCount == 0 {
								endIdx = i
								break
							}
						}
					}
				}

				if endIdx != -1 {
					if maxObjectBytes > 0 && endIdx-startIdx+1 > maxObjectBytes {
						return fmt.Errorf("%w: exceeds %d byte limit", errStreamObjectTooLarge, maxObjectBytes)
					}
					jsonObject := buffer[startIdx : endIdx+1]

					nextObject := endIdx + 1
					remaining := len(buffer) - nextObject
					if cap(buffer) > maxRetainedStreamBuffer && remaining < maxRetainedStreamBuffer/2 {
						// 大型单对象处理完后释放膨胀的 backing array，避免后续小 chunk
						// 在整个流生命周期内继续占用几十 MiB。
						trimmed := make([]byte, remaining)
						copy(trimmed, buffer[nextObject:])
						buffer = trimmed
						scanPos = 0
					} else {
						// 同一网络读取中可能包含多个对象。只推进游标，不为每个对象
						// 搬移剩余数据；否则一批 N 帧会产生 O(N²) 的内存复制。
						scanPos = nextObject
					}
					startIdx = -1
					braceCount = 0
					inString = false
					escape = false

					stop, err := onObject(jsonObject)
					if err != nil {
						return err
					}
					if stop {
						return nil
					}
					if remaining == 0 {
						buffer = buffer[:0]
						scanPos = 0
						break
					}
				} else {
					// 未扫到完整对象：记下已扫位置，下个 chunk 续扫，不重扫前缀。
					scanPos = len(buffer)
					if maxObjectBytes > 0 && braceCount > 0 && len(buffer)-startIdx > maxObjectBytes {
						return fmt.Errorf("%w: exceeds %d byte limit", errStreamObjectTooLarge, maxObjectBytes)
					}
					if startIdx > 0 {
						// 每次网络读取最多压缩一次尚未完成的对象，既释放前导垃圾/
						// 已处理帧，也避免逐对象搬移剩余字节。
						copy(buffer, buffer[startIdx:])
						buffer = buffer[:len(buffer)-startIdx]
						scanPos -= startIdx
						startIdx = 0
					}
					break
				}
			}
		}

		if readErr != nil {
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				return fmt.Errorf("读取上游流被中断: %w", readErr)
			}
			if errors.Is(readErr, io.EOF) {
				if startIdx >= 0 && braceCount > 0 {
					return fmt.Errorf("读取上游流遇到截断 JSON: %w", io.ErrUnexpectedEOF)
				}
				return nil
			}
			// 首帧前的真实网络错误必须交给竞速层，使当前节点记为失败并接力；
			// 已输出内容后的错误也要保留诊断，调用方会避免重放部分响应。
			return fmt.Errorf("读取上游流失败: %w", readErr)
		}
	}
}

// parseJSONObject 把单个 JSON 对象字符串解析为 map，失败返回 nil（解析失败跳过）。
func parseJSONObject(b []byte) map[string]any {
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil
	}
	return obj
}

var canonicalAnonymousStreamPrefix = []byte( //nolint:gochecknoglobals
	`{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":`,
)
var canonicalAnonymousStreamSuffix = []byte(`}}}]}`) //nolint:gochecknoglobals

var canonicalCleanTextPrefix = []byte( //nolint:gochecknoglobals
	`{"candidates":[{"content":{"parts":[{"text":`,
)

var canonicalDirtyTextPrefix = []byte( //nolint:gochecknoglobals
	`{"candidates":[{"content":{"parts":[{"data":"text","text":`,
)

var canonicalDirtyTextSuffix = []byte( //nolint:gochecknoglobals
	`,"thought":false,"thoughtSignature":"","fileData":{},"functionCall":{},"functionResponse":{},"inlineData":{}}`,
)

var canonicalTextContentSuffix = []byte(`],"role":"model"}`) //nolint:gochecknoglobals
var canonicalFinishReasonPrefix = []byte(`,"finishReason":`) //nolint:gochecknoglobals
var canonicalCandidateIndexZero = []byte(`,"index":0`)       //nolint:gochecknoglobals
var canonicalTextChunkSuffix = []byte(`}]}`)                 //nolint:gochecknoglobals

var canonicalFinishReasons = [...]struct { //nolint:gochecknoglobals
	encoded []byte
	value   string
}{
	{[]byte(`"FINISH_REASON_UNSPECIFIED"`), "FINISH_REASON_UNSPECIFIED"},
	{[]byte(`"STOP"`), "STOP"},
	{[]byte(`"MAX_TOKENS"`), "MAX_TOKENS"},
	{[]byte(`"SAFETY"`), "SAFETY"},
	{[]byte(`"RECITATION"`), "RECITATION"},
	{[]byte(`"LANGUAGE"`), "LANGUAGE"},
	{[]byte(`"BLOCKLIST"`), "BLOCKLIST"},
	{[]byte(`"PROHIBITED_CONTENT"`), "PROHIBITED_CONTENT"},
	{[]byte(`"SPII"`), "SPII"},
	{[]byte(`"MALFORMED_FUNCTION_CALL"`), "MALFORMED_FUNCTION_CALL"},
	{[]byte(`"OTHER"`), "OTHER"},
}

// processStreamingJSON 对匿名 batchGraphql 的常见单结果外壳走严格快路径：外壳
// 完全匹配时只解码内部 Gemini 对象。结构、字段顺序、错误或多结果有任何变化时，
// 回退完整动态解析，保持兼容性和错误语义。
func processStreamingJSON(raw []byte, emit func(map[string]any) bool) (bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if bytes.HasPrefix(trimmed, canonicalAnonymousStreamPrefix) &&
		bytes.HasSuffix(trimmed, canonicalAnonymousStreamSuffix) {
		start := len(canonicalAnonymousStreamPrefix)
		end := len(trimmed) - len(canonicalAnonymousStreamSuffix)
		inner := bytes.TrimSpace(trimmed[start:end])
		if len(inner) >= 2 && inner[0] == '{' && inner[len(inner)-1] == '}' {
			if chunk, ok := parseCanonicalTextStreamChunk(inner); ok {
				if !emit(chunk) {
					log.Printf("[Stream] 客户端主动断开，导致流结束")
					return true, nil
				}
				return false, nil
			}
			if data := parseJSONObject(inner); data != nil {
				if chunk := extractChunk(data); chunk != nil && !emit(chunk) {
					log.Printf("[Stream] 客户端主动断开，导致流结束")
					return true, nil
				}
				return false, nil
			}
		}
	}

	object := parseJSONObject(trimmed)
	if object == nil {
		return false, nil
	}
	return processStreamingObject(object, emit)
}

// parseCanonicalTextStreamChunk 处理匿名端点最常见的单 candidate 文本帧。
// 这里只接受字段顺序和值都完全匹配的两种 protobuf JSON 形状；任何扩展字段、
// 非零 index、思考块或格式变化都会回退通用 json.Unmarshal，避免快路径吞字段。
func parseCanonicalTextStreamChunk(raw []byte) (map[string]any, bool) {
	dirty := false
	switch {
	case bytes.HasPrefix(raw, canonicalCleanTextPrefix):
		raw = raw[len(canonicalCleanTextPrefix):]
	case bytes.HasPrefix(raw, canonicalDirtyTextPrefix):
		raw = raw[len(canonicalDirtyTextPrefix):]
		dirty = true
	default:
		return nil, false
	}

	text, rest, ok := takeCanonicalJSONString(raw)
	if !ok {
		return nil, false
	}
	if dirty {
		if !bytes.HasPrefix(rest, canonicalDirtyTextSuffix) {
			return nil, false
		}
		rest = rest[len(canonicalDirtyTextSuffix):]
	} else {
		if len(rest) == 0 || rest[0] != '}' {
			return nil, false
		}
		rest = rest[1:]
	}
	if !bytes.HasPrefix(rest, canonicalTextContentSuffix) {
		return nil, false
	}
	rest = rest[len(canonicalTextContentSuffix):]

	finishReason := ""
	hasFinishReason := false
	if bytes.HasPrefix(rest, canonicalFinishReasonPrefix) {
		rest = rest[len(canonicalFinishReasonPrefix):]
		finishReason, rest, ok = takeCanonicalFinishReason(rest)
		if !ok {
			return nil, false
		}
		hasFinishReason = true
	}
	hasIndex := bytes.HasPrefix(rest, canonicalCandidateIndexZero)
	if hasIndex {
		rest = rest[len(canonicalCandidateIndexZero):]
	}
	if !bytes.Equal(rest, canonicalTextChunkSuffix) {
		return nil, false
	}

	part := map[string]any{"text": text}
	if dirty {
		// 与通用 cleanPart 保持一致：移除 protobuf 空对象，但保留显式思考标记。
		part["thought"] = false
		part["thoughtSignature"] = ""
	}
	content := map[string]any{"parts": []any{part}, "role": "model"}
	candidate := map[string]any{"content": content}
	if hasFinishReason {
		candidate["finishReason"] = finishReason
	}
	if hasIndex {
		candidate["index"] = float64(0)
	}
	return map[string]any{"candidates": []any{candidate}}, true
}

// takeCanonicalFinishReason 对协议中已知枚举返回共享字符串，避免每个流式帧
// 都从扫描缓冲区复制相同值。未知枚举仍走通用字符串解析以保持向前兼容。
func takeCanonicalFinishReason(raw []byte) (string, []byte, bool) {
	for _, reason := range canonicalFinishReasons {
		if bytes.HasPrefix(raw, reason.encoded) {
			return reason.value, raw[len(reason.encoded):], true
		}
	}
	return takeCanonicalJSONString(raw)
}

func takeCanonicalJSONString(raw []byte) (string, []byte, bool) {
	if len(raw) < 2 || raw[0] != '"' {
		return "", raw, false
	}
	escaped := false
	hasEscape := false
	for index := 1; index < len(raw); index++ {
		ch := raw[index]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			hasEscape = true
			continue
		}
		if ch == '"' {
			encoded := raw[:index+1]
			if hasEscape {
				var value string
				if err := json.Unmarshal(encoded, &value); err != nil {
					return "", raw, false
				}
				return value, raw[index+1:], true
			}
			value := raw[1:index]
			if !utf8.Valid(value) {
				return "", raw, false
			}
			return string(value), raw[index+1:], true
		}
		if ch < 0x20 {
			return "", raw, false
		}
	}
	return "", raw, false
}

// processStreamingObject 从单个上游 JSON 对象提取增量 chunk。
//
// 先识别 results 内的错误（"Failed to verify action" → AuthenticationError 触发重试），
// 再 unwrap data.ui.streamGenerateContentAnonymous，最后 _extract_chunk 清洗后 emit。
// 返回 (stop, err)：客户端断开即 stop=true（结束扫描）；上游错误即 err 非 nil。
// 真实 finishReason 只表示内容生成结束，后面仍可能有独立 usageMetadata 统计帧。
func processStreamingObject(obj map[string]any, emit func(map[string]any) bool) (bool, error) {
	results, _ := obj["results"].([]any)
	for _, rRaw := range results {
		result, ok := rRaw.(map[string]any)
		if !ok {
			continue
		}

		// results 内的错误处理。
		if errs, ok := result["errors"].([]any); ok && len(errs) > 0 {
			errMsg := ""
			if first, ok := errs[0].(map[string]any); ok {
				errMsg = toStr(first["message"])
			} else {
				errMsg = toStr(errs[0])
			}
			if strings.Contains(errMsg, "Failed to verify action") ||
				strings.Contains(errMsg, "The caller does not have permission") {
				return false, NewAuthenticationError(errMsg)
			}
			if parsed := parseErrorResponse(map[string]any{"errors": errs}); parsed != nil {
				return false, parsed
			}
		}

		data, ok := result["data"].(map[string]any)
		if !ok {
			continue
		}

		// unwrap data.ui.streamGenerateContentAnonymous（匿名端点把载荷包在这里面）。
		if ui, ok := data["ui"].(map[string]any); ok {
			if innerRaw, exists := ui["streamGenerateContentAnonymous"]; exists {
				switch inner := innerRaw.(type) {
				case map[string]any:
					data = inner
				case []any:
					outerMeta := map[string]any{}
					for _, key := range []string{"usageMetadata", "modelVersion", "responseId", "promptFeedback"} {
						if v, ok := data[key]; ok && isTruthyAny(v) {
							outerMeta[key] = v
						}
					}
					// 极少数情况 inner 是 list：逐项 extract+emit，本 result 处理完跳过下方。
					for _, itemRaw := range inner {
						if item, ok := itemRaw.(map[string]any); ok {
							for k, v := range outerMeta {
								if _, exists := item[k]; !exists {
									item[k] = v
								}
							}
							if chunk := extractChunk(item); chunk != nil {
								if !emit(chunk) {
									log.Printf("[Stream] 客户端主动断开，导致流结束")
									return true, nil
								}
							}
						}
					}
					continue
				default:
					continue
				}
			}
		}

		if chunk := extractChunk(data); chunk != nil {
			if !emit(chunk) {
				log.Printf("[Stream] 客户端主动断开，导致流结束")
				return true, nil
			}
		}
	}
	return false, nil
}

// extractChunk 从 Gemini 数据中提取标准化 chunk，清洗畸形嵌套。
//
// 对齐 Python _process_streaming_object：candidates key 存在且非 nil 时保留
// （即使空列表），总是保留 metadata 字段，仅当 chunk 完全无字段时返回 nil。
// data 来自当前 JSON 帧并由本函数取得唯一所有权，因此原位清洗以避免每帧重复复制
// candidate/content/part map；返回后所有权继续转移给单一 StreamChunk 消费者。
func extractChunk(data map[string]any) map[string]any {
	if raw, ok := data["candidates"]; ok && raw != nil {
		candidatesRaw, candidatesOK := raw.([]any)
		cleaned := cleanStreamCandidates(candidatesRaw)
		// A canonical frame keeps the same slice header and backing array; all
		// nested cleanup is already in place, so avoid re-boxing []any into the
		// interface map on every token. Invalid types or compaction still write.
		if !candidatesOK || len(cleaned) != len(candidatesRaw) {
			data["candidates"] = cleaned
		}
	} else {
		delete(data, "candidates")
	}

	for key, value := range data {
		switch key {
		case "candidates":
		case "usageMetadata", "modelVersion", "responseId", "promptFeedback", "createTime":
			if !isTruthyAny(value) {
				delete(data, key)
			}
		default:
			delete(data, key)
		}
	}

	if len(data) == 0 {
		return nil
	}
	return data
}

func cleanStreamCandidates(candidates []any) []any {
	valid := 0
	for _, candidateRaw := range candidates {
		candidate, ok := candidateRaw.(map[string]any)
		if !ok {
			continue
		}
		if content, ok := candidate["content"].(map[string]any); ok {
			if parts, ok := content["parts"].([]any); ok {
				cleanedParts := cleanStreamParts(parts)
				if len(cleanedParts) != len(parts) {
					content["parts"] = cleanedParts
				}
				role, roleIsString := content["role"].(string)
				if !roleIsString || role == "" {
					role = toStr(content["role"])
					if role == "" {
						role = "model"
					}
					content["role"] = role
				}
				for key := range content {
					if key != "role" && key != "parts" {
						delete(content, key)
					}
				}
			}
		}
		candidates[valid] = candidate
		valid++
	}
	if valid == 0 {
		// 保持旧行为：完全没有合法 map 时原样透传列表。
		return candidates
	}
	clear(candidates[valid:])
	return candidates[:valid]
}

// cleanStreamParts 原位清洗 parts 列表，展开畸形嵌套并移除 protobuf 空默认字段。
func cleanStreamParts(parts []any) []any {
	valid := 0
	for _, pRaw := range parts {
		part, ok := pRaw.(map[string]any)
		if !ok {
			continue
		}
		textVal, hasText := part["text"]
		if hasText {
			if _, isStr := textVal.(string); !isStr {
				// 畸形嵌套：text 值是 list/dict 而非字符串，递归提取真正文本。
				extracted := extractTextRecursive(textVal, 0)
				if extracted != "" {
					newPart := cleanPart(part)
					if newPart != nil {
						newPart["text"] = extracted
						parts[valid] = newPart
						valid++
					}
				}
				continue
			}
		}
		if cp := cleanPart(part); cp != nil {
			parts[valid] = cp
			valid++
		}
	}
	clear(parts[valid:])
	return parts[:valid]
}

// cleanPart 原位清洗单个 Gemini part，移除内部 protobuf 空默认字段，仅保留真实内容字段。
func cleanPart(part map[string]any) map[string]any {
	// 移除内部 protobuf oneof 指示器（always "text" / "inlineData" / "functionCall" / "functionResponse"）
	delete(part, "data")

	// fileData：仅在 uri 为空时移除
	if fd, ok := part["fileData"].(map[string]any); ok {
		if toStr(fd["fileUri"]) == "" && toStr(fd["mimeType"]) == "" {
			delete(part, "fileData")
		}
	}

	// functionCall：name 和 args 都为空/无意义时移除
	if fc, ok := part["functionCall"].(map[string]any); ok {
		hasName := toStr(fc["name"]) != ""
		hasArgs := false
		if args, ok := fc["args"]; ok && args != nil {
			if m, ok := args.(map[string]any); ok && len(m) > 0 {
				hasArgs = true
			}
		}
		if !hasName && !hasArgs {
			delete(part, "functionCall")
		} else if name, ok := fc["name"].(string); ok && name != "" {
			if argStr, ok := fc["args"].(string); ok && argStr != "" {
				var parsed any
				if err := json.Unmarshal([]byte(argStr), &parsed); err == nil {
					fc["args"] = parsed
				}
			}
		}
	}

	// functionResponse：name 和 response 都为空时移除
	if fr, ok := part["functionResponse"].(map[string]any); ok {
		hasName := toStr(fr["name"]) != ""
		hasResp := false
		if resp, ok := fr["response"]; ok && resp != nil {
			if m, ok := resp.(map[string]any); ok && len(m) > 0 {
				hasResp = true
			}
		}
		if !hasName && !hasResp {
			delete(part, "functionResponse")
		} else if respStr, ok := fr["response"].(string); ok && respStr != "" {
			fr["response"] = map[string]any{"result": respStr}
		}
	}

	// inlineData：data 为空时移除
	if id, ok := part["inlineData"].(map[string]any); ok {
		if toStr(id["data"]) == "" {
			delete(part, "inlineData")
		}
	}

	// 支持代码块、代码执行结果透传
	for _, key := range []string{"executableCode", "codeExecutionResult"} {
		if v, ok := part[key]; ok && isTruthyAny(v) {
			return part
		}
	}

	// 如果只剩 thought/thoughtSignature 等非内容标记，返回 nil
	for k := range part {
		switch k {
		case "thought", "thoughtSignature":
			continue
		default:
			return part
		}
	}
	return nil
}

// extractTextRecursive 从嵌套结构中递归提取纯文本，防止无限递归（depth>20 截断）。
func extractTextRecursive(val any, depth int) string {
	if depth > 20 {
		s := toStr(val)
		if len(s) > 500 {
			return s[:500]
		}
		return s
	}
	switch v := val.(type) {
	case string:
		return v
	case map[string]any:
		if t, ok := v["text"]; ok {
			return extractTextRecursive(t, depth+1)
		}
		return ""
	case []any:
		var sb strings.Builder
		for _, item := range v {
			if t := extractTextRecursive(item, depth+1); t != "" {
				sb.WriteString(t)
			}
		}
		return sb.String()
	default:
		return ""
	}
}
