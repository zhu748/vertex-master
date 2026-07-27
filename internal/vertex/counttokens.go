package vertex

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/spool"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

// CountTokens 统计给定 contents 在指定模型下的 token 数。
// 通过匿名 Vertex batchGraphql CountTokens operation 获取模型侧精确计数；查询
// 失败时返回 0，调用方据此保持 usage 缺失，而不是退回本地估算。
func (c *VertexAIClient) CountTokens(ctx context.Context, model string, contents []any) int {
	if len(contents) == 0 {
		return 0
	}
	count, err := c.countTokensUpstream(ctx, model, contents)
	if err != nil {
		log.Printf("[Vertex] [CountTokens] 上游精确计数失败: 模型=%s, 请求ID=%s, 原因=%v", model, RequestIDFromContext(ctx), err)
		return 0
	}
	return count
}

func (c *VertexAIClient) countTokensUpstream(ctx context.Context, model string, contents []any) (int, error) {
	proxyURI := c.cfg.ActiveNodeURI()
	if proxyURI == "" {
		proxyURI = c.cfg.ProxyURL()
	}
	recaptchaToken, err := c.pool.GetTokenWithProxy(proxyURI)
	if err != nil {
		return 0, fmt.Errorf("fetch recaptcha token: %w", err)
	}
	if recaptchaToken == "" {
		return 0, fmt.Errorf("empty recaptcha token")
	}

	sess, err := c.net.CreateSession(c.cfg.RequestTimeout(), proxyURI, RequestIDFromContext(ctx))
	if err != nil {
		return 0, fmt.Errorf("create session: %w", err)
	}
	defer sess.Close()

	payload := buildCountTokensPayload(model, contents, recaptchaToken, c.cfg)
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
		status, raw, requestErr := sess.DoAndRead(ctx, "POST", c.getBatchGraphqlURL(), countTokensHeaders(), reader)
		_ = buf.Close()
		if requestErr != nil {
			return 0, fmt.Errorf("upstream request: %w", requestErr)
		}
		if status == 200 {
			if count := parseCountTokensResponse(raw); count > 0 {
				return count, nil
			}
			if attempt == 0 && stringsContainAny(
				string(raw), "Failed to verify action", "The caller does not have permission",
			) {
				continue
			}
			return 0, fmt.Errorf("upstream response did not contain totalTokens")
		}
		if attempt == 0 && (status == 401 || status == 403 ||
			stringsContainAny(string(raw), "Failed to verify action", "The caller does not have permission")) {
			continue
		}
		return 0, fmt.Errorf("upstream returned HTTP %d", status)
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
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0
	}
	var items []any
	switch v := parsed.(type) {
	case []any:
		items = v
	case map[string]any:
		items = []any{v}
	default:
		return 0
	}

	for _, entryRaw := range items {
		entry, ok := entryRaw.(map[string]any)
		if !ok {
			continue
		}
		// entry 级别 errors → 跳过。
		if _, hasErr := entry["errors"]; hasErr {
			continue
		}
		results, _ := entry["results"].([]any)
		for _, rRaw := range results {
			result, ok := rRaw.(map[string]any)
			if !ok {
				continue
			}
			if _, hasErr := result["errors"]; hasErr {
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
				if tt, ok := countData["totalTokens"]; ok {
					return coerceTokenCount(tt)
				}
			}
		}
	}
	return 0
}

// coerceTokenCount 把 totalTokens（数字或数字字符串）转 int。
func coerceTokenCount(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if x, err := strconv.Atoi(n); err == nil {
			return x
		}
	}
	return 0
}
