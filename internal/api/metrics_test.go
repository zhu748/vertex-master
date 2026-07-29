package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/cli"
	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

func TestMetricsBodyIncludesTokenCountStats(t *testing.T) {
	vc := vertex.NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	body := metricsBody(vc)
	stats, ok := body["token_count"].(vertex.TokenCountStats)
	if !ok {
		t.Fatalf("token_count stats missing: %#v", body)
	}
	if stats.CacheHits != 0 || stats.UpstreamQueries != 0 || stats.CacheEntries != 0 {
		t.Fatalf("new client token stats should be empty: %#v", stats)
	}
}

// TestWithMetrics 验证 withMetrics 中间件行为：
// 设 X-Request-Id、注入 context、跳过健康检查端点。
func TestWithMetrics(t *testing.T) {
	mw := &middleware{} //nolint:exhaustruct

	var seenReqID string
	ok := mw.withMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenReqID = vertex.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	ok.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil))
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("应设置 X-Request-Id 响应头")
	}
	if seenReqID == "" || seenReqID != rec.Header().Get("X-Request-Id") {
		t.Fatalf("context 里的 request-id 应与响应头一致，got ctx=%q header=%q", seenReqID, rec.Header().Get("X-Request-Id"))
	}

	for _, path := range []string{"/health", "/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		mw.withMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Header().Get("X-Request-Id") != "" {
			t.Fatalf("%s 不应设 X-Request-Id", path)
		}
	}
}

func TestWithMetricsFinishesTrackedRequestOnPanic(t *testing.T) {
	mw := &middleware{} //nolint:exhaustruct
	tracked := mw.withMetrics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("test panic")
	}))
	recorder := httptest.NewRecorder()

	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		tracked.ServeHTTP(
			recorder,
			httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		)
	}()
	if recovered == nil {
		t.Fatal("downstream panic was unexpectedly swallowed")
	}

	requestID := recorder.Header().Get("X-Request-Id")
	if requestID == "" {
		t.Fatal("metrics middleware did not assign a request ID")
	}
	if cli.FinishReq(requestID) {
		t.Fatal("panicking request remained in the active request tracker")
	}
}
