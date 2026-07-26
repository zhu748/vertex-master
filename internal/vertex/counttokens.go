package vertex

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

// CountTokens 统计给定 contents 在指定模型下的 token 数。
//
// 当前为本地启发式估算（estimateTokens），不发起任何上游请求，因此不消耗配额、
// 不受代理与 recaptcha 影响，但结果是近似值而非上游精确计数。
//
// 下方的 buildCountTokensPayload / countTokensHeaders / parseCountTokensResponse
// 是走匿名 batchGraphql CountTokens operation 的真实实现所需组件，目前未接线；
// 若要恢复精确计数，从这里改为调用它们即可。
func (c *VertexAIClient) CountTokens(ctx context.Context, model string, contents []any) int {
	return estimateTokens(contents)
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

// estimateTokens 递归或嵌套遍历 contents 计算估算的 token 总数。
func estimateTokens(contents []any) int {
	totalTokens := 0
	for _, contentAny := range contents {
		if contentAny == nil {
			continue
		}
		content, ok := contentAny.(map[string]any)
		if !ok {
			if s, ok := contentAny.(string); ok {
				totalTokens += estimateTextTokens(s)
			}
			continue
		}

		parts, ok := content["parts"].([]any)
		if !ok {
			continue
		}

		for _, partAny := range parts {
			if partAny == nil {
				continue
			}
			switch part := partAny.(type) {
			case string:
				totalTokens += estimateTextTokens(part)
			case map[string]any:
				totalTokens += estimatePartTokens(part)
			}
		}
	}
	return totalTokens
}

// estimatePartTokens 估算单个 part 的 token 数。
func estimatePartTokens(part map[string]any) int {
	if isImagePart(part) {
		return 1024
	}
	if textVal, ok := part["text"].(string); ok {
		return estimateTextTokens(textVal)
	}
	return 0
}

// isImagePart 判断一个 part 是否为图片。
func isImagePart(part map[string]any) bool {
	// 检查 image_url, input_image (OpenAI style)
	if _, ok := part["image_url"]; ok {
		return true
	}
	if _, ok := part["input_image"]; ok {
		return true
	}
	// 检查 inlineData / inline_data (Gemini style)
	for _, k := range []string{"inlineData", "inline_data"} {
		if m, ok := part[k].(map[string]any); ok {
			for _, mk := range []string{"mimeType", "mime_type"} {
				if mime, ok := m[mk].(string); ok && strings.Contains(strings.ToLower(mime), "image") {
					return true
				}
			}
		}
	}
	// 检查 fileData / file_data (Gemini style)
	for _, k := range []string{"fileData", "file_data"} {
		if m, ok := part[k].(map[string]any); ok {
			for _, mk := range []string{"mimeType", "mime_type"} {
				if mime, ok := m[mk].(string); ok && strings.Contains(strings.ToLower(mime), "image") {
					return true
				}
			}
		}
	}
	// 检查直接的 mimeType / mime_type
	for _, mk := range []string{"mimeType", "mime_type"} {
		if mime, ok := part[mk].(string); ok && strings.Contains(strings.ToLower(mime), "image") {
			return true
		}
	}
	return false
}

// estimateTextTokens 估算文本部分的 token 数。
// 这里的简单估算规则：
// - ASCII 字符（如英文、数字、符号、空格）算 0.25 个 token
// - 非 ASCII 字符（如中文汉字、日文、韩文、Emoji等）每个算 1.5 个 token
func estimateTextTokens(text string) int {
	var tokens float64
	for _, r := range text {
		if r < 128 {
			tokens += 0.25
		} else {
			tokens += 1.5
		}
	}
	return int(tokens + 0.99)
}
