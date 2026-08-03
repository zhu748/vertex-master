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
	body := metricsBody(vc, nil)
	stats, ok := body["token_count"].(vertex.TokenCountStats)
	if !ok {
		t.Fatalf("token_count stats missing: %#v", body)
	}
	if stats.CacheHits != 0 || stats.UpstreamQueries != 0 || stats.CacheEntries != 0 {
		t.Fatalf("new client token stats should be empty: %#v", stats)
	}
	requests, ok := body["requests"].(requestMetricsSnapshot)
	if !ok || requests.Total != 0 || requests.Active != 0 {
		t.Fatalf("new request metrics should be empty: %#v", body["requests"])
	}
}

// TestWithMetrics 验证 withMetrics 中间件行为：
// 设 X-Request-Id、注入 context、跳过健康检查端点。
func TestWithMetrics(t *testing.T) {
	requests := &requestMetrics{}
	mw := &middleware{requests: requests} //nolint:exhaustruct

	var seenReqID string
	ok := mw.withMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenReqID = vertex.RequestIDFromContext(r.Context())
		if active := requests.snapshot().Active; active != 1 {
			t.Errorf("active requests during handler = %d, want 1", active)
		}
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
	rejected := mw.withMetrics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	rejectedRec := httptest.NewRecorder()
	rejected.ServeHTTP(rejectedRec, httptest.NewRequest("POST", "/v1/responses", nil))

	stats := requests.snapshot()
	if stats.Total != 2 || stats.Active != 0 || stats.Status.Successful != 1 ||
		stats.Status.ClientError != 1 || stats.Errors != 1 {
		t.Fatalf("unexpected request metrics after successful request: %#v", stats)
	}

	for _, path := range []string{"/health", "/healthz", "/readyz", "/favicon.ico"} {
		rec := httptest.NewRecorder()
		mw.withMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Header().Get("X-Request-Id") != "" {
			t.Fatalf("%s 不应设 X-Request-Id", path)
		}
	}
	if stats := requests.snapshot(); stats.Total != 2 {
		t.Fatalf("health endpoints must not affect request metrics: %#v", stats)
	}
}

func TestWithMetricsFinishesTrackedRequestOnPanic(t *testing.T) {
	requests := &requestMetrics{}
	mw := &middleware{requests: requests} //nolint:exhaustruct
	tracked := mw.withMetrics(mw.withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("test panic")
	})))
	recorder := httptest.NewRecorder()

	tracked.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
	)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("panicking request status = %d, want 500", recorder.Code)
	}

	requestID := recorder.Header().Get("X-Request-Id")
	if requestID == "" {
		t.Fatal("metrics middleware did not assign a request ID")
	}
	if cli.FinishReq(requestID) {
		t.Fatal("panicking request remained in the active request tracker")
	}
	stats := requests.snapshot()
	if stats.Total != 1 || stats.Active != 0 || stats.Panics != 1 || stats.Status.ServerError != 1 {
		t.Fatalf("unexpected request metrics after panic: %#v", stats)
	}
}
