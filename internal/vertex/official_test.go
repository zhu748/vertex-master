package vertex

import (
	"context"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

func officialTestClient(t *testing.T, mutate func(*config.AppConfig)) *VertexAIClient {
	t.Helper()
	cfg := config.DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	return NewVertexAIClient(config.StaticProvider(cfg))
}

func TestOfficialAPIEnabled(t *testing.T) {
	// 默认（未设置 gemini_api_mode）：无论 Key 是否存在都走匿名模式。
	client := officialTestClient(t, nil)
	if client.officialAPIEnabled() {
		t.Fatal("default mode must stay anonymous (headless vertex)")
	}
	client = officialTestClient(t, func(c *config.AppConfig) { c.GeminiAPIKey = " AQ.test-key " })
	if client.officialAPIEnabled() {
		t.Fatal("key without official mode must stay anonymous")
	}
	// anon 显式声明：即使配了 Key 也强制匿名（Key 仅保存待切换用）。
	client = officialTestClient(t, func(c *config.AppConfig) {
		c.GeminiAPIMode = config.GeminiAPIModeAnon
		c.GeminiAPIKey = "AQ.test-key"
	})
	if client.officialAPIEnabled() {
		t.Fatal("explicit anon mode must stay anonymous")
	}
	// official 模式：显式 Key 生效且去除首尾空白。
	client = officialTestClient(t, func(c *config.AppConfig) {
		c.GeminiAPIMode = config.GeminiAPIModeOfficial
		c.GeminiAPIKey = " AQ.test-key "
	})
	if !client.officialAPIEnabled() {
		t.Fatal("official mode must enable official API")
	}
	if got := client.officialAPIKey(); got != "AQ.test-key" {
		t.Fatalf("key must be trimmed, got %q", got)
	}
	// official 模式且 Key 留空（默认未构建时注入）：模式开启但解析结果为空，
	// 由执行层守卫给出明确的配置错误，而非上游 401。
	client = officialTestClient(t, func(c *config.AppConfig) {
		c.GeminiAPIMode = config.GeminiAPIModeOfficial
	})
	if !client.officialAPIEnabled() {
		t.Fatal("official mode must be enabled regardless of key presence")
	}
	if got := client.officialAPIKey(); got != "" {
		t.Fatalf("official mode with empty key and no injected builtin must resolve empty, got %q", got)
	}
}

func TestOfficialAPIBaseURL(t *testing.T) {
	client := officialTestClient(t, nil)
	if got := client.officialAPIBaseURL(); got != defaultOfficialGeminiBaseURL {
		t.Fatalf("default base url = %q", got)
	}
	client = officialTestClient(t, func(c *config.AppConfig) {
		c.GeminiAPIBaseURL = "https://mirror.example.com/v1beta/"
	})
	if got := client.officialAPIBaseURL(); got != "https://mirror.example.com/v1beta" {
		t.Fatalf("custom base url trailing slash not trimmed: %q", got)
	}
	client = officialTestClient(t, func(c *config.AppConfig) { c.GeminiAPIBaseURL = "ftp://bad" })
	if got := client.officialAPIBaseURL(); got != defaultOfficialGeminiBaseURL {
		t.Fatalf("invalid scheme must fall back to default, got %q", got)
	}
}

func TestOfficialURLs(t *testing.T) {
	client := officialTestClient(t, func(c *config.AppConfig) {
		c.GeminiAPIMode = config.GeminiAPIModeOfficial
		c.GeminiAPIKey = "K"
		c.GeminiAPIBaseURL = "https://mirror.example.com"
	})
	if got := client.officialStreamURL("gemini-flash"); got != "https://mirror.example.com/models/gemini-flash:streamGenerateContent?alt=sse" {
		t.Fatalf("stream url = %q", got)
	}
	if got := client.officialGenerateURL("gemini-flash"); got != "https://mirror.example.com/models/gemini-flash:generateContent" {
		t.Fatalf("generate url = %q", got)
	}
	if got := client.officialCountTokensURL("gemini-flash"); got != "https://mirror.example.com/models/gemini-flash:countTokens" {
		t.Fatalf("count url = %q", got)
	}
}

func TestSanitizeOfficialModelName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "gemini-3.6-flash", want: "gemini-3.6-flash"},
		{in: " models/gemini-flash ", want: "gemini-flash"},
		{in: "", wantErr: true},
		{in: "../secret", wantErr: true},
		{in: "gemini flash", wantErr: true},
		{in: "gemini$x", wantErr: true},
	}
	for _, tc := range cases {
		got := sanitizeOfficialModelName(tc.in)
		if tc.wantErr {
			if got != "" {
				t.Fatalf("sanitize(%q) = %q, want reject", tc.in, got)
			}
			continue
		}
		if got != tc.want {
			t.Fatalf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOfficialPayloadFromVariables(t *testing.T) {
	prepared := map[string]any{
		"model":             "gemini-3.6-flash",
		"region":            "global",
		"contents":          []any{map[string]any{"role": "user"}},
		"generationConfig":  map[string]any{"temperature": 0.5},
		"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "sys"}}},
		"tools":             []any{},
		"toolConfig":        map[string]any{"mode": "AUTO"},
		"safetySettings":    []any{},
	}
	payload := officialPayloadFromVariables(prepared)
	for _, forbidden := range []string{"model", "region"} {
		if _, ok := payload[forbidden]; ok {
			t.Fatalf("official payload must not contain %q", forbidden)
		}
	}
	for _, required := range []string{"contents", "generationConfig", "systemInstruction", "tools", "toolConfig", "safetySettings"} {
		if _, ok := payload[required]; !ok {
			t.Fatalf("official payload missing %q", required)
		}
	}
}

func TestClassifyOfficialHTTPError(t *testing.T) {
	// 标准 429 RESOURCE_EXHAUSTED → ratelimit。
	quotaBody := `{"error":{"code":429,"message":"Resource exhausted","status":"RESOURCE_EXHAUSTED"}}`
	ve := classifyOfficialHTTPError(429, quotaBody)
	if ve.Kind != "ratelimit" || ve.Code != 429 {
		t.Fatalf("429 classified as [%s/%d]", ve.Kind, ve.Code)
	}

	// 400 地理封锁 → 可重试的 unavailable（换节点可解）。
	geoBody := `{"error":{"code":400,"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}`
	ve = classifyOfficialHTTPError(400, geoBody)
	if ve.Kind != "unavailable" || !ve.IsRetryable() {
		t.Fatalf("geo-block classified as [%s] retryable=%v", ve.Kind, ve.IsRetryable())
	}

	// 400 普通参数错误 → invalid（不可重试）。
	invalidBody := `{"error":{"code":400,"message":"Invalid JSON payload","status":"INVALID_ARGUMENT"}}`
	ve = classifyOfficialHTTPError(400, invalidBody)
	if ve.Kind != "invalid" || ve.IsRetryable() {
		t.Fatalf("invalid classified as [%s] retryable=%v", ve.Kind, ve.IsRetryable())
	}

	// 401 → auth（密钥问题）。
	authBody := `{"error":{"code":401,"message":"API key not valid","status":"UNAUTHENTICATED"}}`
	ve = classifyOfficialHTTPError(401, authBody)
	if ve.Kind != "auth" {
		t.Fatalf("401 classified as [%s]", ve.Kind)
	}

	// 403 HTML 边缘封锁页 → 可重试的 unavailable（Google 封的是出口 IP，换节点可解）。
	html403 := "<!DOCTYPE html>\n<html lang=en>\n  <meta charset=utf-8>\n  <title>Error 403 (Forbidden)!!1</title>"
	ve = classifyOfficialHTTPError(403, html403)
	if ve.Kind != "unavailable" || !ve.IsRetryable() {
		t.Fatalf("HTML 403 edge-block classified as [%s] retryable=%v", ve.Kind, ve.IsRetryable())
	}

	// 403 JSON PERMISSION_DENIED → permission（密钥权限问题，不可重试）。
	permBody := `{"error":{"code":403,"message":"Permission denied","status":"PERMISSION_DENIED"}}`
	ve = classifyOfficialHTTPError(403, permBody)
	if ve.Kind != "permission" || ve.IsRetryable() {
		t.Fatalf("403 permission classified as [%s] retryable=%v", ve.Kind, ve.IsRetryable())
	}

	// 无错误体 → 纯状态码归类。
	ve = classifyOfficialHTTPError(503, "")
	if ve.Code != 503 || !ve.IsRetryable() {
		t.Fatalf("503 classified as [%s/%d]", ve.Kind, ve.Code)
	}
}

func TestParseRetryInfoSeconds(t *testing.T) {
	body := `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED",
                "details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"27s"}]}}`
	if got := parseRetryInfoSeconds(body); got != 27 {
		t.Fatalf("retryDelay = %d, want 27", got)
	}
	if got := parseRetryInfoSeconds(`{"error":{"message":"no details"}}`); got != 0 {
		t.Fatalf("missing RetryInfo must be 0, got %d", got)
	}
	if got := parseRetryInfoSeconds("not json"); got != 0 {
		t.Fatalf("invalid body must be 0, got %d", got)
	}
}

func TestProcessOfficialStreamJSON(t *testing.T) {
	var chunks []StreamChunk
	emit := func(ch StreamChunk) bool {
		chunks = append(chunks, ch)
		return true
	}

	// 普通数据帧。
	dataFrame := []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"index":0}],"usageMetadata":{"totalTokenCount":5}}`)
	stop, err := processOfficialStreamJSON(dataFrame, emit)
	if err != nil || stop {
		t.Fatalf("data frame: stop=%v err=%v", stop, err)
	}
	if len(chunks) != 1 || chunks[0].Data == nil {
		t.Fatalf("expected one data chunk, got %d", len(chunks))
	}

	// 错误帧上抛。
	_, err = processOfficialStreamJSON([]byte(`{"error":{"code":429,"message":"x","status":"RESOURCE_EXHAUSTED"}}`), emit)
	if err == nil {
		t.Fatal("error frame must surface an error")
	}
	if ve := asVertexError(err); ve == nil || ve.Kind != "ratelimit" {
		t.Fatalf("error frame classified as %v", err)
	}

	// usage-only 帧（无 candidates）也保留 metadata。
	chunks = nil
	_, _ = processOfficialStreamJSON([]byte(`{"usageMetadata":{"totalTokenCount":9}}`), emit)
	if len(chunks) != 1 || chunks[0].Data == nil {
		t.Fatal("metadata-only frame must be emitted")
	}
}

func TestOfficialSSEStreamScan(t *testing.T) {
	// 模拟官方 SSE 响应体：data: 行 + 空行分隔。
	body := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"你\"}],\"role\":\"model\"}}]}\r\n\r\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"好\"}],\"role\":\"model\"}}]}\r\n\r\n" +
		"data: {\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"totalTokenCount\":3}}\r\n\r\n"

	var texts []string
	finish := ""
	err := scanStreamRaw(strings.NewReader(body), func(raw []byte) (bool, error) {
		stop, scanErr := processOfficialStreamJSON(raw, func(ch StreamChunk) bool {
			if ch.Data == nil {
				return true
			}
			cands, _ := ch.Data["candidates"].([]any)
			if len(cands) == 0 {
				return true
			}
			cand, _ := cands[0].(map[string]any)
			if reason, ok := cand["finishReason"].(string); ok && reason != "" {
				finish = reason
				return true
			}
			content, _ := cand["content"].(map[string]any)
			parts, _ := content["parts"].([]any)
			for _, p := range parts {
				part, _ := p.(map[string]any)
				texts = append(texts, part["text"].(string))
			}
			return true
		})
		return stop, scanErr
	})
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if strings.Join(texts, "") != "你好" {
		t.Fatalf("texts = %v", texts)
	}
	if finish != "STOP" {
		t.Fatalf("finishReason = %q", finish)
	}
}

func TestOfficialTextWithBracesSurvivesSSEScan(t *testing.T) {
	// 文本中的花括号与引号转义不能破坏帧扫描。
	body := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"say {\\\"hi\\\"} ok\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}]}\r\n\r\n"
	var got string
	err := scanStreamRaw(strings.NewReader(body), func(raw []byte) (bool, error) {
		_, err := processOfficialStreamJSON(raw, func(ch StreamChunk) bool {
			cands := ch.Data["candidates"].([]any)
			cand := cands[0].(map[string]any)
			content := cand["content"].(map[string]any)
			parts := content["parts"].([]any)
			part := parts[0].(map[string]any)
			got = part["text"].(string)
			return true
		})
		return false, err
	})
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if got != `say {"hi"} ok` {
		t.Fatalf("text = %q", got)
	}
}

func TestOfficialStreamingBranchCompiles(t *testing.T) {
	// 不发真实网络请求：仅验证分支接入后匿名路径未被破坏（key 为空时走原逻辑）。
	client := officialTestClient(t, nil)
	if client.officialAPIEnabled() {
		t.Fatal("anonymous mode expected")
	}
	// 官方模式执行函数存在且签名正确（编译期校验；用空白标识符避免
	// staticcheck SA4031 对"函数值永不为 nil"的误报式 nil 检查）。
	var _ func(context.Context, string, map[string]any, string, func(StreamChunk) bool) = client.executeOfficialStreamingWithPreparedVariables
}
