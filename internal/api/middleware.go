package api

import (
	"context"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/cli"
	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

type middleware struct {
	cfg  config.ConfigProvider
	keys *APIKeyManager
	gate requestConcurrencyGate
}

type requestConcurrencyGate struct {
	mu     sync.Mutex
	active int
}

func (g *requestConcurrencyGate) tryAcquire(limit int) bool {
	if limit < 1 {
		limit = 1
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active >= limit {
		return false
	}
	g.active++
	return true
}

func (g *requestConcurrencyGate) release() {
	g.mu.Lock()
	if g.active > 0 {
		g.active--
	}
	g.mu.Unlock()
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (m *middleware) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[Server] panic recovered: %v\n%s", rec, debug.Stack())
				oaiError(w, http.StatusInternalServerError, "服务内部错误，请联系开发者 (internal error)", "server_error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (m *middleware) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAdminPath(r.URL.Path) {
			if r.Method == http.MethodOptions {
				writeJSON(w, http.StatusForbidden, adminErr("管理接口不允许跨域访问"))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set(
			"Access-Control-Allow-Headers",
			"Authorization, Content-Type, X-API-Key, X-Goog-Api-Key",
		)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *middleware) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set(
			"Content-Security-Policy",
			"default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; "+
				"form-action 'self'; connect-src 'self'; img-src 'self' data: blob: https:; "+
				"style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'",
		)
		if requestIsHTTPS(r) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") ||
			strings.HasPrefix(r.URL.Path, "/api/admin/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (m *middleware) withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(m.cfg.MaxRequestMB()) << 20
		if limit <= 0 {
			limit = 64 << 20
		}
		if r.ContentLength > limit {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error": map[string]any{
					"code":    http.StatusRequestEntityTooLarge,
					"message": "请求体过大 (request body too large)",
					"status":  "RESOURCE_EXHAUSTED",
				},
			})
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func (m *middleware) withConcurrencyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isUpstreamWorkloadPath(r.URL.Path) || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if !m.gate.tryAcquire(m.cfg.MaxConcurrentRequests()) {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{
				"code":    http.StatusServiceUnavailable,
				"message": "服务繁忙，请稍后重试 (server overloaded)",
				"status":  "UNAVAILABLE",
			}})
			return
		}
		defer m.gate.release()
		next.ServeHTTP(w, r)
	})
}

func isUpstreamWorkloadPath(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/images/generations", "/v1/images/edits",
		"/v1/images/variations", "/v1/audio/speech":
		return true
	default:
		return strings.HasPrefix(path, "/v1beta/models/") || strings.HasPrefix(path, "/v1/models/")
	}
}

func (m *middleware) withMetrics(next http.Handler) http.Handler {
	skip := map[string]bool{"/": true, "/health": true, "/healthz": true, "/readyz": true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip[r.URL.Path] || isAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		reqID := reqID24()
		w.Header().Set("X-Request-Id", reqID)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		ctx := context.WithValue(r.Context(), vertex.RequestIDKey{}, reqID)
		cli.StartReq(reqID)
		start := time.Now()
		next.ServeHTTP(sw, r.WithContext(ctx))
		elapsed := time.Since(start)
		cli.FinishReq(reqID)
		log.Printf("[Server] %s %s - %d (%.3fs) 请求ID=%s", r.Method, r.URL.Path, sw.status, elapsed.Seconds(), reqID)
	})
}

func (m *middleware) withAPIKey(next http.Handler) http.Handler {
	excluded := map[string]bool{"/": true, "/health": true, "/healthz": true, "/readyz": true, "/favicon.ico": true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if excluded[r.URL.Path] || isAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		key := extractAPIKey(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code": 401, "message": "缺少 API 密钥 (missing API key)", "status": "UNAUTHENTICATED",
			}})
			return
		}
		if key == "sk-your-key-here" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code": 401, "message": "示例密钥禁止调用，请新建密钥。", "status": "UNAUTHENTICATED",
			}})
			return
		}
		if !m.keys.ValidateKey(key) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code": 401, "message": "API 密钥无效 (invalid API key)", "status": "UNAUTHENTICATED",
			}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/") || strings.HasPrefix(path, "/api/admin/") || strings.HasPrefix(path, "/assets/")
}
