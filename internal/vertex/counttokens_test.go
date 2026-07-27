package vertex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/recaptcha"
)

func TestLiveCountTokens(t *testing.T) {
	if os.Getenv("VERTEX_LIVE_COUNT_TEST") == "" {
		t.Skip("set VERTEX_LIVE_COUNT_TEST=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	input := client.CountTokens(ctx, "gemini-3.6-flash", []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "hello"}},
	}})
	output := client.CountTokens(ctx, "gemini-3.6-flash", []any{map[string]any{
		"role": "model", "parts": []any{map[string]any{"text": "hello"}},
	}})
	if input <= 0 || output <= 0 {
		t.Fatalf("live CountTokens returned zero: input=%d output=%d", input, output)
	}
	t.Logf("live CountTokens: input=%d output=%d", input, output)
}

// ---- parseCountTokensResponse：三种 unwrap 形态 + errors 跳过 + 字符串/数字 totalTokens ----

func TestParseCountTokensResponse(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{
			name: "data.ui.countTokensV2 (number)",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":42}}}}]}]`,
			want: 42,
		},
		{
			name: "data.countTokensV2 (number)",
			raw:  `[{"results":[{"data":{"countTokensV2":{"totalTokens":100}}}]}]`,
			want: 100,
		},
		{
			name: "data.countTokens (number)",
			raw:  `[{"results":[{"data":{"countTokens":{"totalTokens":7}}}]}]`,
			want: 7,
		},
		{
			name: "totalTokens as string",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":"256"}}}}]}]`,
			want: 256,
		},
		{
			name: "single object (not array)",
			raw:  `{"results":[{"data":{"countTokensV2":{"totalTokens":15}}}]}`,
			want: 15,
		},
		{
			name: "entry-level errors skipped",
			raw:  `[{"errors":[{"message":"boom"}]},{"results":[{"data":{"countTokensV2":{"totalTokens":9}}}]}]`,
			want: 9,
		},
		{
			name: "result-level errors skipped",
			raw:  `[{"results":[{"errors":[{"x":1}]},{"data":{"countTokensV2":{"totalTokens":11}}}]}]`,
			want: 11,
		},
		{
			name: "ui preferred over flat",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":1}},"countTokensV2":{"totalTokens":999}}}]}]`,
			want: 1,
		},
		{
			name: "no countData returns 0",
			raw:  `[{"results":[{"data":{"somethingElse":{}}}]}]`,
			want: 0,
		},
		{
			name: "missing totalTokens returns 0",
			raw:  `[{"results":[{"data":{"countTokensV2":{}}}]}]`,
			want: 0,
		},
		{
			name: "empty results returns 0",
			raw:  `[{"results":[]}]`,
			want: 0,
		},
		{
			name: "invalid json returns 0",
			raw:  `not json{`,
			want: 0,
		},
		{
			name: "json primitive returns 0",
			raw:  `12345`,
			want: 0,
		},
		{
			name: "empty array returns 0",
			raw:  `[]`,
			want: 0,
		},
		{
			name: "totalTokens non-numeric string returns 0",
			raw:  `[{"results":[{"data":{"countTokensV2":{"totalTokens":"abc"}}}]}]`,
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseCountTokensResponse([]byte(c.raw)); got != c.want {
				t.Errorf("parseCountTokensResponse(%s)=%d，期望 %d", c.raw, got, c.want)
			}
		})
	}
}

// ---- coerceTokenCount ----

func TestCoerceTokenCount(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"float64", float64(42), 42},
		{"float64 truncates", float64(42.9), 42},
		{"int", 7, 7},
		{"numeric string", "123", 123},
		{"trimmed not supported (atoi strict)", " 5 ", 0}, // Atoi 不 trim
		{"non-numeric string", "abc", 0},
		{"nil", nil, 0},
		{"bool", true, 0},
		{"zero float", float64(0), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := coerceTokenCount(c.in); got != c.want {
				t.Errorf("coerceTokenCount(%v)=%d，期望 %d", c.in, got, c.want)
			}
		})
	}
}
func TestCountTokensUsesUpstreamOperation(t *testing.T) {
	oldURL := batchGraphqlURL
	t.Cleanup(func() { batchGraphqlURL = oldURL })

	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":42}}}}]}]`))
	}))
	defer upstream.Close()
	batchGraphqlURL = upstream.URL

	cfg := config.DefaultConfig()
	cfg.ParallelPoolEnabled = false
	client := NewVertexAIClient(config.StaticProvider(cfg))
	client.SetTokenPool(recaptcha.NewTokenPoolCustom(func(string) (string, error) {
		return "test-recaptcha-token", nil
	}))

	contents := []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "hello"}},
	}}
	if got := client.CountTokens(context.Background(), "gemini-test", contents); got != 42 {
		t.Fatalf("CountTokens=%d, want 42", got)
	}
	if received["operationName"] != "CountTokens" {
		t.Fatalf("未调用上游 CountTokens operation: %#v", received)
	}
	variables, _ := received["variables"].(map[string]any)
	if variables["recaptchaToken"] != "test-recaptcha-token" {
		t.Fatalf("CountTokens 请求缺少 recaptchaToken: %#v", variables)
	}
}
