package vertex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/spool"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

// 单 API Key 直连模式：把请求改发官方 Gemini API（generativelanguage.googleapis.com）。
//
// 与匿名端点模式的关键差异：
//   - 鉴权：x-goog-api-key 请求头携带用户自己的 API Key，完全跳过 reCAPTCHA；
//   - 端点：标准 REST（models/{model}:streamGenerateContent?alt=sse / :generateContent），
//     SSE 帧为 data: {JSON}，直接 extractChunk，无 GraphQL 外壳；
//   - 代理：仍然复用 race engine 与节点池 —— 每个 candidate 使用独立代理 IP。
//     上游按 IP 限速时，429 会经 recordProxyAttempt 进入 30 秒节点冷却并接力
//     下一个候选，实现"每请求轮换 IP"的分摊效果。

const (
	defaultOfficialGeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	// 官方 API 的模型名仅允许 [A-Za-z0-9-.]，避免拼接 URL 时注入路径。
	officialModelNamePattern = `^[A-Za-z0-9][A-Za-z0-9._-]*$`
)

var officialModelNameRe = regexp.MustCompile(officialModelNamePattern) //nolint:gochecknoglobals

// officialAPIKey 返回直连模式实际生效的 API Key：显式配置的 gemini_api_key 优先，
// official 模式下留空则回退内置默认密钥（见 config.ResolveGeminiAPIKey）。
func (c *VertexAIClient) officialAPIKey() string {
	return config.ResolveGeminiAPIKey(c.cfg.GeminiAPIMode(), c.cfg.GeminiAPIKey())
}

// officialAPIEnabled 报告是否启用单 API Key 直连模式。
// 仅当 gemini_api_mode 归一化后为 "official" 时启用；默认 anon 走
// 项目原有的无头 Vertex 匿名端点，与是否配置了 Key 无关。
func (c *VertexAIClient) officialAPIEnabled() bool {
	return config.NormalizeGeminiAPIMode(c.cfg.GeminiAPIMode()) == config.GeminiAPIModeOfficial
}

// officialAPIBaseURL 返回官方 API 基地址：配置了镜像地址则用之，否则用官方默认。
// 结尾统一去掉 '/'，便于拼接路径。
func (c *VertexAIClient) officialAPIBaseURL() string {
	base := strings.TrimSpace(c.cfg.GeminiAPIBaseURL())
	if base == "" {
		return defaultOfficialGeminiBaseURL
	}
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		log.Printf("[Vertex] [OfficialAPI] 非法的 gemini_api_base_url（缺少 http/https 前缀），回退官方地址")
		return defaultOfficialGeminiBaseURL
	}
	return base
}

// officialStreamURL 构造流式端点地址：{base}/models/{model}:streamGenerateContent?alt=sse。
func (c *VertexAIClient) officialStreamURL(modelName string) string {
	return c.officialAPIBaseURL() + "/models/" + modelName + ":streamGenerateContent?alt=sse"
}

// officialCountURL 构造非流式端点地址：{base}/models/{model}:generateContent。
func (c *VertexAIClient) officialGenerateURL(modelName string) string {
	return c.officialAPIBaseURL() + "/models/" + modelName + ":generateContent"
}

// officialCountTokensURL 构造计数端点地址：{base}/models/{model}:countTokens。
func (c *VertexAIClient) officialCountTokensURL(modelName string) string {
	return c.officialAPIBaseURL() + "/models/" + modelName + ":countTokens"
}

// officialPayloadFromVariables 把 buildRequestVariables 的产出裁剪成官方 API 载荷。
//
// 匿名端点的 variables 额外携带 model/region 两个端点专属字段；官方 API 的模型
// 名走 URL 路径，region 是 Cloud Console 内部概念，两者都必须剔除，否则官方
// API 直接 400 INVALID_ARGUMENT。
func officialPayloadFromVariables(preparedVariables map[string]any) map[string]any {
	payload := make(map[string]any, 8)
	for _, key := range []string{
		"contents", "generationConfig", "safetySettings",
		"systemInstruction", "toolConfig", "tools",
	} {
		if value, ok := preparedVariables[key]; ok && value != nil {
			payload[key] = value
		}
	}
	return payload
}

// sanitizeOfficialModelName 校验并裁剪模型名。别名解析后的名字理论上总是合法，
// 这里兜底防止把非法字符拼进 URL。非法时返回空串，由调用方报 invalid。
func sanitizeOfficialModelName(modelName string) string {
	modelName = strings.TrimSpace(modelName)
	modelName = strings.TrimPrefix(modelName, "models/")
	if !officialModelNameRe.MatchString(modelName) {
		return ""
	}
	return modelName
}

// officialHeaders 构造官方 API 请求头。官方接口不需要伪装 Cloud Console 来源，
// 仅保留标准 JSON 头与密钥头；TLS 指纹仍由 transport 层的 Chrome profile 提供。
func officialHeaders(apiKey string) transport.Header {
	header := transport.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("x-goog-api-key", apiKey)
	return header
}

// officialEdgeBlocked 识别 Google 边缘反滥用系统返回的 HTML 403 封锁页。
// 真正的 API 权限错误是 JSON（PERMISSION_DENIED）；部分垃圾代理 IP 会被
// Google 边缘直接以经典 HTML "Error 403 (Forbidden)" 页面拒绝。这类封锁与
// 出口 IP 强相关，换节点即可绕过，不能当作密钥权限问题熔断竞速。
func officialEdgeBlocked(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "<!doctype html") ||
		strings.Contains(lower, "<html") ||
		strings.Contains(lower, "error 403")
}

// classifyOfficialHTTPError 把官方 API 的 HTTP 状态码 + 错误体映射为 VertexError。
//
// 分类原则与 race engine 的节点归因对齐：
//   - 429 → ratelimit：节点进入 30 秒冷却并接力下一候选（每 IP 限速的核心路径）；
//   - 401/403/404 → auth/permission/notfound：密钥或模型名问题，换 IP 无用；
//   - 403 且响应体为 HTML 封锁页 → unavailable（可重试）：Google 边缘封的是
//     出口 IP 而非密钥，换节点即可解决；
//   - 400 且携带地理限制文案 → unavailable（可重试）：地理封锁取决于出口 IP，
//     换节点即可解决，绝不能按 invalid 熔断整场竞速；
//   - 其余 4xx → client（不可重试），5xx → server（可重试）。
func classifyOfficialHTTPError(status int, body string) *VertexError {
	if status == 403 && officialEdgeBlocked(body) {
		return NewUnavailableError("upstream edge-blocked this proxy IP (HTML 403)")
	}
	if parsed := parseErrorResponse(body); parsed != nil {
		// 官方错误体为 {"error":{"code":...,"status":...,"message":...}}。
		// parseErrorResponse 已按 status/code 归类，这里只补地理封锁的特判。
		if parsed.Code == 400 && officialGeoBlocked(body) {
			return NewUnavailableError("upstream geo-restricted this proxy IP: " + parsed.Message)
		}
		parsed.UpstreamResponse = body
		return parsed
	}
	return raiseForStatus(status, "", fmt.Sprintf("Upstream Error: %s", body), nil, body)
}

// officialGeoBlocked 识别官方 API 的地理封锁错误（400 FAILED_PRECONDITION）。
// 这类错误与出口 IP 强相关，按可重试的节点故障处理才能触发换 IP 接力。
func officialGeoBlocked(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "user location is not supported") ||
		strings.Contains(lower, "user_location_denied") ||
		strings.Contains(lower, "location is not supported") ||
		strings.Contains(lower, "service territory")
}

// parseRetryInfoSeconds 从官方 429 错误体里抠 RetryInfo.retryDelay（如 "27s"）。
// 没有就返回 0，由重试循环按退避策略决定等待时间。
func parseRetryInfoSeconds(body string) int {
	var parsed struct {
		Error struct {
			Details []map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return 0
	}
	for _, detail := range parsed.Error.Details {
		if typeInfo, ok := detail["@type"].(string); !ok ||
			!strings.Contains(typeInfo, "RetryInfo") {
			continue
		}
		delay, ok := detail["retryDelay"].(string)
		if !ok {
			continue
		}
		delay = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(delay), "s"))
		if seconds, err := strconv.Atoi(delay); err == nil && seconds > 0 {
			return seconds
		}
	}
	return 0
}

// executeOfficialStreamingWithPreparedVariables 单 API Key 模式的流式执行循环。
//
// 结构与匿名版 executeStreamingWithPreparedVariables 对齐（同样的 429/网络错误
// 重试语义，同样的 payloadYielded 保护），差异仅在于：无 reCAPTCHA token 生命周期、
// 无首帧 verify-fail 二次尝试、请求目标为官方 SSE 端点。
func (c *VertexAIClient) executeOfficialStreamingWithPreparedVariables(
	ctx context.Context,
	model string,
	preparedVariables map[string]any,
	proxyURI string,
	yield func(StreamChunk) bool,
) {
	cfg := c.cfg
	maxRetries := effectiveMaxRetries(
		cfg.MaxRetries(),
		cfg.ParallelPoolEnabled(),
		cfg.ParallelPoolRetryEnabled(),
	)
	apiKey := c.officialAPIKey()
	if apiKey == "" {
		log.Printf("[Vertex] [OfficialAPI] official 模式未配置 API Key，请求拒绝")
		yield(StreamChunk{Err: NewInvalidArgumentError(
			"official 模式未配置 API Key：请在管理面板填入，或设置 VPROXY_GEMINI_API_KEY / 构建时注入内置密钥")})
		return
	}
	payloadYielded := false
	var lastError *VertexError

	reqID := RequestIDFromContext(ctx)
	nodeName := nodes.GetNodeName(proxyURI)
	sess, err := c.net.CreateSession(cfg.RequestTimeout(), proxyURI, reqID)
	if err != nil {
		yield(StreamChunk{Err: NewInternalError("create session: " + err.Error())})
		return
	}
	// 429 路径会替换 sess；直接 defer sess.Close() 只会关第一个。
	defer func() { sess.Close() }()

	attempt := 0

retryLoop:
	for attempt <= maxRetries {
		log.Printf("[Vertex] [OfficialAPI] 开始尝试 (Attempt %d/%d), 模型=%s, 请求ID=%s, 代理=%s",
			attempt, maxRetries, model, reqID, nodeName)
		attemptErr := c.executeOfficialStreamingAttempt(ctx, sess, model, preparedVariables, apiKey, func(ch StreamChunk) bool {
			if streamChunkHasPayload(ch) {
				payloadYielded = true
			}
			return yield(ch)
		})

		if attemptErr == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		ve := asVertexError(attemptErr)
		switch {
		case ve != nil && ve.Kind == "ratelimit":
			lastError = ve
			if payloadYielded || attempt >= maxRetries {
				log.Printf("[Vertex] [OfficialAPI] (Attempt %d/%d) 节点 %s 触发 429 终止接力 (已产出内容=%v, 建议 %ds 后重试), 请求ID=%s, 代理=%s",
					attempt, maxRetries, nodeName, payloadYielded, ve.RetryAfter, reqID, proxyURI)
				break retryLoop
			}
			// 429：销毁旧 session 重建（新的出口连接 + cookie），换 IP 交给竞速层。
			sess.Close()
			newSess, createErr := c.net.CreateSession(cfg.RequestTimeout(), proxyURI, reqID)
			if createErr != nil {
				log.Printf("[Vertex] [OfficialAPI] 节点 %s 429 后重建会话失败: %s, 请求ID=%s", nodeName, createErr.Error(), reqID)
				yield(StreamChunk{Err: lastError})
				return
			}
			sess = newSess
			wait := ve.RetryAfter
			if wait <= 0 {
				wait = min(10, 1+attempt)
			}
			log.Printf("[Vertex] [OfficialAPI] (Attempt %d/%d) 节点 %s 触发 429 将重试 (延迟 %ds), 请求ID=%s, 代理=%s",
				attempt, maxRetries, nodeName, wait, reqID, nodeName)
			attempt++
			if sleepErr := sleepCtx(ctx, time.Duration(wait)*time.Second); sleepErr != nil {
				break retryLoop
			}

		case ve != nil:
			lastError = ve
			if !ve.IsRetryable() || payloadYielded || attempt >= maxRetries {
				log.Printf("[Vertex] [OfficialAPI] (Attempt %d/%d) 节点 %s 触发异常错误失败: [%s] %s, 请求ID=%s, 代理=%s",
					attempt, maxRetries, nodeName, ve.Kind, ve.Message, reqID, proxyURI)
				break retryLoop
			}
			log.Printf("[Vertex] [OfficialAPI] (Attempt %d/%d) 节点 %s 触发异常错误将重试: [%s] %s, 请求ID=%s, 代理=%s",
				attempt, maxRetries, nodeName, ve.Kind, ve.Message, reqID, proxyURI)
			attempt++
			if sleepErr := sleepCtx(ctx, backoff(attempt)); sleepErr != nil {
				break retryLoop
			}

		default:
			lastError = NewInternalError(attemptErr.Error())
			break retryLoop
		}
	}

	// payload 之后的传输失败属于公开流的一部分，不因已发内容而吞错。
	if lastError != nil && ctx.Err() == nil {
		yield(StreamChunk{Err: lastError})
	}
}

// executeOfficialStreamingAttempt 执行单次官方 API 流式请求：
// POST :streamGenerateContent?alt=sse → 逐 SSE 帧清洗 → emit。
//
// emit 返回 false（客户端断开）时正常停止并返回 nil。
func (c *VertexAIClient) executeOfficialStreamingAttempt(
	ctx context.Context,
	sess *transport.Session,
	model string,
	preparedVariables map[string]any,
	apiKey string,
	emit func(StreamChunk) bool,
) error {
	reqID := RequestIDFromContext(ctx)
	modelName := sanitizeOfficialModelName(toStr(preparedVariables["model"]))
	if modelName == "" {
		return NewInvalidArgumentError("invalid model name for official API: " + model)
	}
	body := officialPayloadFromVariables(preparedVariables)
	buf, err := spool.EncodeJSON(body)
	if err != nil {
		return NewInternalError("marshal payload: " + err.Error())
	}
	defer func() { _ = buf.Close() }()
	reader, err := buf.Reader()
	if err != nil {
		return NewInternalError("spool reader: " + err.Error())
	}

	log.Printf("[Vertex] [OfficialAPI] 准备发送流式请求: 模型=%s, 请求ID=%s", modelName, reqID)
	sr, err := sess.DoStream(ctx, "POST", c.officialStreamURL(modelName), officialHeaders(apiKey), reader)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return NewUnavailableError("upstream request: " + err.Error())
	}
	defer sr.Close() // 提前停止或出错时立即中断上游 body。

	if sr.StatusCode != 200 {
		errorBody, readErr := transport.ReadAllLimit(sr.Body, upstreamErrorBodyMaxBytes)
		errText := string(errorBody)
		if errors.Is(readErr, transport.ErrResponseBodyTooLarge) {
			errText += "\n[upstream error body truncated]"
		} else if readErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return NewUnavailableError("read upstream error response: " + readErr.Error())
		}
		ve := classifyOfficialHTTPError(sr.StatusCode, errText)
		if ve.Kind == "ratelimit" {
			ve.RetryAfter = parseRetryInfoSeconds(errText)
		}
		log.Printf("[Vertex] [OfficialAPI] HTTP 报错! 状态码: %d, 模型=%s, 请求ID=%s", sr.StatusCode, modelName, reqID)
		return ve
	}

	// 官方 SSE 流：data: {JSON} 帧。scanStreamRaw 按花括号配对提取 JSON 对象，
	// "data:" 前缀会被自动跳过，无需单独的 SSE 行解析器。
	scanErr := scanStreamRaw(sr.Body, func(raw []byte) (stop bool, err error) {
		return processOfficialStreamJSON(raw, emit)
	})
	if scanErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if vertexErr := asVertexError(scanErr); vertexErr != nil {
		return vertexErr
	}
	if errors.Is(scanErr, io.EOF) {
		return nil
	}
	return NewUnavailableError("read upstream stream: " + scanErr.Error())
}

// processOfficialStreamJSON 处理单个官方 API 流帧：错误帧上抛，数据帧清洗后 emit。
// 返回 stop=true 表示客户端断开（emit 返回 false 时停止扫描）。
func processOfficialStreamJSON(raw []byte, emit func(StreamChunk) bool) (bool, error) {
	object := parseJSONObject(raw)
	if object == nil {
		return false, nil
	}
	if _, hasError := object["error"]; hasError {
		return false, parseErrorResponse(object)
	}
	if chunk := extractChunk(object); chunk != nil {
		if !emit(StreamChunk{Data: chunk}) {
			log.Printf("[Stream] 客户端主动断开，导致流结束")
			return true, nil
		}
	}
	return false, nil
}

// officialCountTokenSetsUpstream 单 API Key 模式的精确计数：逐内容集并行 POST
// :countTokens，解析 {"totalTokens": N}。代理选择与匿名版保持一致。
func (c *VertexAIClient) officialCountTokenSetsUpstream(
	ctx context.Context,
	model string,
	contentSets [][]any,
) ([]int, []error) {
	counts := make([]int, len(contentSets))
	requestErrors := make([]error, len(contentSets))
	modelName := sanitizeOfficialModelName(c.cfg.ResolveModelName(model))
	if modelName == "" {
		for index := range contentSets {
			requestErrors[index] = NewInvalidArgumentError("invalid model name for official API: " + model)
		}
		return counts, requestErrors
	}

	proxyURI := c.cfg.ActiveNodeURI()
	if proxyURI == "" {
		proxyURI = c.cfg.ProxyURL()
	}
	if c.officialAPIKey() == "" {
		for index := range contentSets {
			requestErrors[index] = NewInvalidArgumentError(
				"official 模式未配置 API Key：请在管理面板填入，或设置 VPROXY_GEMINI_API_KEY / 构建时注入内置密钥")
		}
		return counts, requestErrors
	}
	sess, err := c.net.CreateSession(c.cfg.RequestTimeout(), proxyURI, RequestIDFromContext(ctx))
	if err != nil {
		for index := range contentSets {
			requestErrors[index] = fmt.Errorf("create session: %w", err)
		}
		return counts, requestErrors
	}
	defer sess.Close()

	type countResult struct {
		index int
		count int
		err   error
	}
	results := make(chan countResult, len(contentSets))
	active := 0
	for index, contents := range contentSets {
		if len(contents) == 0 {
			continue
		}
		active++
		go func(index int, contents []any) {
			count, countErr := c.executeOfficialCountTokens(ctx, sess, modelName, contents)
			results <- countResult{index: index, count: count, err: countErr}
		}(index, contents)
	}
	for range active {
		res := <-results
		counts[res.index] = res.count
		requestErrors[res.index] = res.err
	}
	return counts, requestErrors
}

// executeOfficialCountTokens 执行单次官方 countTokens 请求。
func (c *VertexAIClient) executeOfficialCountTokens(
	ctx context.Context,
	sess *transport.Session,
	modelName string,
	contents []any,
) (int, error) {
	if contents == nil {
		contents = []any{}
	}
	payload := map[string]any{"contents": contents}
	buf, err := spool.EncodeJSON(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal payload: %w", err)
	}
	defer func() { _ = buf.Close() }()
	reader, err := buf.Reader()
	if err != nil {
		return 0, fmt.Errorf("spool reader: %w", err)
	}
	status, raw, requestErr := sess.DoAndReadLimit(
		ctx, "POST", c.officialCountTokensURL(modelName),
		officialHeaders(c.officialAPIKey()), reader, countTokensResponseMaxBytes,
	)
	if requestErr != nil {
		return 0, fmt.Errorf("upstream request: %w", requestErr)
	}
	if status != 200 {
		if ve := classifyOfficialHTTPError(status, string(raw)); ve != nil {
			return 0, ve
		}
		return 0, raiseForStatus(status, "", fmt.Sprintf("upstream returned HTTP %d", status), nil, string(raw))
	}
	var parsed struct {
		TotalTokens int `json:"totalTokens"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, fmt.Errorf("decode countTokens response: %w", err)
	}
	return parsed.TotalTokens, nil
}
