package vertex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/spool"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

const countTokensResponseMaxBytes = 4 << 20

// CountTokens 统计给定 contents 在指定模型下的 token 数。
// 通过匿名 Vertex batchGraphql CountTokens operation 获取模型侧精确计数；查询
// 失败时返回 0，调用方据此保持 usage 缺失，而不是退回本地估算。
func (c *VertexAIClient) CountTokens(ctx context.Context, model string, contents []any) int {
	counts := c.CountTokenSets(ctx, model, contents)
	if len(counts) == 0 {
		return 0
	}
	return counts[0]
}

// CountTokensExact 用于显式计数接口：与后台 usage 补齐不同，上游错误必须
// 返回给调用方，避免把无效模型或权限错误伪装成成功的 0 token 响应。
func (c *VertexAIClient) CountTokensExact(
	ctx context.Context,
	model string,
	contents []any,
) (int, error) {
	counts, err := c.countTokenSets(ctx, model, contents)
	if len(counts) == 0 {
		return 0, err
	}
	return counts[0], err
}

// CountTokenSets 使用一个 reCAPTCHA token 和一个 TLS session，并行统计多组
// contents。它用于同时补齐输入/输出 usage，避免为同一生成结果重复获取验证码。
// 返回值与 contentSets 一一对应；空内容或查询失败的项保持为 0。
func (c *VertexAIClient) CountTokenSets(ctx context.Context, model string, contentSets ...[]any) []int {
	counts, err := c.countTokenSets(ctx, model, contentSets...)
	if err != nil {
		log.Printf("[Vertex] [CountTokens] 上游精确计数失败: 模型=%s, 请求ID=%s, 原因=%v", model, RequestIDFromContext(ctx), err)
	}
	return counts
}

func (c *VertexAIClient) countTokenSets(
	ctx context.Context,
	model string,
	contentSets ...[]any,
) ([]int, error) {
	type pendingCount struct {
		originalIndex int
		contents      []any
		key           tokenCountCacheKey
		flight        *tokenCountFlight
		tracked       bool
	}

	counts := make([]int, len(contentSets))
	var countErrors []error
	upstream := make([]pendingCount, 0, len(contentSets))
	waiting := make([]pendingCount, 0, len(contentSets))
	for index, contents := range contentSets {
		if len(contents) == 0 {
			continue
		}
		if key, ok := makeTokenCountCacheKey(model, contents); ok {
			count, hit, flight, owner := c.lookupOrClaimTokenCount(key)
			if hit {
				counts[index] = count
				continue
			}
			pending := pendingCount{
				originalIndex: index, contents: contents, key: key, flight: flight, tracked: true,
			}
			if owner {
				upstream = append(upstream, pending)
			} else {
				waiting = append(waiting, pending)
			}
			continue
		}
		c.countMetrics.cacheBypasses.Add(1)
		upstream = append(upstream, pendingCount{originalIndex: index, contents: contents})
	}

	if len(upstream) > 0 {
		upstreamSets := make([][]any, len(upstream))
		for index := range upstream {
			upstreamSets[index] = upstream[index].contents
		}
		c.countMetrics.upstreamQueries.Add(uint64(len(upstreamSets)))
		upstreamCounts, upstreamErrors := c.countTokenSetsUpstream(ctx, model, upstreamSets)
		for index, pending := range upstream {
			count := 0
			if index < len(upstreamCounts) {
				count = upstreamCounts[index]
			}
			var countErr error
			if index < len(upstreamErrors) {
				countErr = upstreamErrors[index]
			}
			counts[pending.originalIndex] = count
			if countErr != nil {
				c.countMetrics.failures.Add(1)
				countErrors = append(countErrors, fmt.Errorf(
					"content set %d: %w", pending.originalIndex, countErr,
				))
			}
			if pending.tracked {
				c.completeTokenCountFlight(pending.key, pending.flight, count, countErr)
			}
		}
	}

	// 先执行本调用拥有的 flight，再等待其他调用拥有的 flight，避免两个批量
	// 调用分别持有一半 key 时互相等待。
	for _, pending := range waiting {
		select {
		case <-pending.flight.done:
			counts[pending.originalIndex] = pending.flight.count
			if pending.flight.err != nil {
				countErrors = append(countErrors, fmt.Errorf(
					"content set %d: %w", pending.originalIndex, pending.flight.err,
				))
			}
		case <-ctx.Done():
			return counts, ctx.Err()
		}
	}
	return counts, errors.Join(countErrors...)
}

func (c *VertexAIClient) countTokenSetsUpstream(
	ctx context.Context,
	model string,
	contentSets [][]any,
) ([]int, []error) {
	counts := make([]int, len(contentSets))
	requestErrors := make([]error, len(contentSets))
	active := make([]int, 0, len(contentSets))
	for index, contents := range contentSets {
		if len(contents) > 0 {
			active = append(active, index)
		}
	}
	if len(active) == 0 {
		return counts, requestErrors
	}
	setActiveError := func(err error) {
		for _, index := range active {
			requestErrors[index] = err
		}
	}

	proxyURI := c.cfg.ActiveNodeURI()
	if proxyURI == "" {
		proxyURI = c.cfg.ProxyURL()
	}
	recaptchaToken, err := c.pool.GetTokenWithProxyContext(ctx, proxyURI)
	if err != nil {
		setActiveError(fmt.Errorf("fetch recaptcha token: %w", err))
		return counts, requestErrors
	}
	if recaptchaToken == "" {
		setActiveError(fmt.Errorf("empty recaptcha token"))
		return counts, requestErrors
	}

	sess, err := c.net.CreateSession(c.cfg.RequestTimeout(), proxyURI, RequestIDFromContext(ctx))
	if err != nil {
		setActiveError(fmt.Errorf("create session: %w", err))
		return counts, requestErrors
	}
	defer sess.Close()

	var wg sync.WaitGroup
	for _, index := range active {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := buildCountTokensPayload(model, contentSets[index], recaptchaToken, c.cfg)
			counts[index], requestErrors[index] = c.executeCountTokensRequest(ctx, sess, payload)
		}()
	}
	wg.Wait()
	return counts, requestErrors
}

func (c *VertexAIClient) executeCountTokensRequest(
	ctx context.Context,
	sess *transport.Session,
	payload map[string]any,
) (int, error) {
	// 匿名端点偶尔会在同一个 token 的首个请求返回 verify-fail，与生成接口行为一致，
	// 因此允许在同一 session 上重试一次。
	for attempt := 0; attempt < 2; attempt++ {
		buf, encodeErr := spool.EncodeJSON(payload)
		if encodeErr != nil {
			return 0, fmt.Errorf("marshal payload: %w", encodeErr)
		}
		reader, readerErr := buf.Reader()
		if readerErr != nil {
			_ = buf.Close()
			return 0, fmt.Errorf("spool reader: %w", readerErr)
		}
		c.countMetrics.httpRequests.Add(1)
		status, raw, requestErr := sess.DoAndReadLimit(
			ctx,
			"POST",
			c.getBatchGraphqlURL(),
			countTokensHeaders(),
			reader,
			countTokensResponseMaxBytes,
		)
		_ = buf.Close()
		if requestErr != nil {
			return 0, fmt.Errorf("upstream request: %w", requestErr)
		}
		if status == 200 {
			count, found, responseErr := parseCountTokensResult(raw)
			if found {
				return count, nil
			}
			if attempt == 0 && stringsContainAny(
				string(raw), "Failed to verify action", "The caller does not have permission",
			) {
				continue
			}
			if responseErr != nil {
				return 0, responseErr
			}
			return 0, fmt.Errorf("upstream response did not contain totalTokens")
		}
		if attempt == 0 && (status == 401 || status == 403 ||
			stringsContainAny(string(raw), "Failed to verify action", "The caller does not have permission")) {
			continue
		}
		if parsed := parseErrorResponse(string(raw)); parsed != nil {
			return 0, parsed
		}
		return 0, raiseForStatus(status, "", fmt.Sprintf("upstream returned HTTP %d", status), nil, string(raw))
	}
	return 0, fmt.Errorf("recaptcha verification failed")
}

func stringsContainAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

// buildCountTokensPayload 构建 CountTokens 的 batchGraphql 请求体。
func buildCountTokensPayload(model string, contents []any, recaptchaToken string, cfg config.ConfigProvider) map[string]any {
	if contents == nil {
		contents = []any{}
	}
	querySig := cfg.CountTokensQuerySignature()
	if querySig == "" {
		querySig = "2/mENOSldfC+HZM+tGhVuJLrl8M6gEyK3HRjUKuA5AM58="
	}
	return map[string]any{
		"requestContext": map[string]any{
			"clientVersion": "boq_cloud-boq-clientweb-vertexaistudio_20260402.09_p0",
			"pagePath":      "/vertex-ai/studio/multimodal",
			"jurisdiction":  "global",
			"localizationData": map[string]any{
				"locale":   "zh_CN",
				"timezone": "Asia/Shanghai",
			},
		},
		"querySignature": querySig,
		"operationName":  "CountTokens",
		"variables": map[string]any{
			"contents":       contents,
			"endpoint":       "",
			"model":          model,
			"region":         "global",
			"recaptchaToken": recaptchaToken,
		},
	}
}

// countTokensHeaders 构造 CountTokens 上游请求头（逐字节保持既定 headers）。
func countTokensHeaders() transport.Header {
	h := transport.XHRHeaders(
		"application/json", "*/*",
		"https://console.cloud.google.com",
		"https://console.cloud.google.com/vertex-ai/studio/multimodal",
		"cross-site",
	)
	h["x-goog-authuser"] = []string{"0"}
	return h
}

// parseCountTokensResponse 从 CountTokens 响应里抠 totalTokens。
//
// 上游可能是单对象或数组；逐层 results → data.ui.countTokensV2 / data.countTokensV2 / data.countTokens，
// 命中 totalTokens 即返回。任何错误/缺字段返回 0。
func parseCountTokensResponse(raw []byte) int {
	count, found, _ := parseCountTokensResult(raw)
	if !found {
		return 0
	}
	return count
}

func parseCountTokensResult(raw []byte) (int, bool, error) {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, false, fmt.Errorf("decode upstream CountTokens response: %w", err)
	}
	var items []any
	switch v := parsed.(type) {
	case []any:
		items = v
	case map[string]any:
		items = []any{v}
	default:
		return 0, false, nil
	}

	var responseErr error
	for _, entryRaw := range items {
		entry, ok := entryRaw.(map[string]any)
		if !ok {
			continue
		}
		if parsedErr := parseErrorResponse(entry); parsedErr != nil {
			if responseErr == nil {
				responseErr = parsedErr
			}
			continue
		}
		results, _ := entry["results"].([]any)
		for _, rRaw := range results {
			result, ok := rRaw.(map[string]any)
			if !ok {
				continue
			}
			if parsedErr := parseErrorResponse(result); parsedErr != nil {
				if responseErr == nil {
					responseErr = parsedErr
				}
				continue
			}
			data, ok := result["data"].(map[string]any)
			if !ok {
				continue
			}
			var countData map[string]any
			if ui, ok := data["ui"].(map[string]any); ok {
				if cd, ok := ui["countTokensV2"].(map[string]any); ok {
					countData = cd
				}
			}
			if countData == nil {
				if cd, ok := data["countTokensV2"].(map[string]any); ok {
					countData = cd
				} else if cd, ok := data["countTokens"].(map[string]any); ok {
					countData = cd
				}
			}
			if countData != nil {
				if tt, exists := countData["totalTokens"]; exists {
					if count, valid := tokenCountValue(tt); valid {
						return count, true, nil
					}
				}
			}
		}
	}
	return 0, false, responseErr
}

// coerceTokenCount 把 totalTokens（数字或数字字符串）转 int。
func coerceTokenCount(v any) int {
	count, _ := tokenCountValue(v)
	return count
}

func tokenCountValue(v any) (int, bool) {
	var count int
	switch n := v.(type) {
	case float64:
		count = int(n)
	case int:
		count = n
	case string:
		if x, err := strconv.Atoi(n); err == nil {
			count = x
		} else {
			return 0, false
		}
	default:
		return 0, false
	}
	return count, count >= 0
}
