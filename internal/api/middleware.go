package api

import (
	"context"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/cli"
	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

type middleware struct {
	cfg      config.ConfigProvider
	keys     *APIKeyManager
	gate     requestConcurrencyGate
	requests *requestMetrics
}

type requestConcurrencyGate struct {
	active atomic.Int64
}

func (g *requestConcurrencyGate) tryAcquire(limit int) bool {
	if limit < 1 {
		limit = 1
	}
	for {
		current := g.active.Load()
		if current >= int64(limit) {
			return false
		}
		if g.active.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (g *requestConcurrencyGate) release() {
	for {
		current := g.active.Load()
		if current <= 0 {
			return
		}
		if g.active.CompareAndSwap(current, current-1) {
			return
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
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

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (m *middleware) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if vertex.RequestIDFromContext(r.Context()) != "" {
					m.requests.recordPanic()
				}
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
			"Authorization, Content-Type, X-API-Key, X-Goog-Api-Key, Anthropic-Version, Anthropic-Beta, OpenAI-Beta",
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
		// script-src 不含 'unsafe-inline'：管理面板的全部交互都通过
		// data-*-action 事件委派绑定（assets/utils.js），没有内联事件处理器或
		// 内联 <script>，因此严格策略可以真正拦下注入的脚本。
		// style-src 仍保留 'unsafe-inline'：面板有动态背景/字体色与内联 style 属性。
		w.Header().Set(
			"Content-Security-Policy",
			"default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; "+
				"form-action 'self'; connect-src 'self'; img-src 'self' data: blob: https:; "+
				"style-src 'self' 'unsafe-inline'; script-src 'self'",
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
	case "/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens",
		"/v1/images/generations", "/v1/images/edits",
		"/v1/images/variations", "/v1/audio/speech":
		return true
	default:
		return strings.HasPrefix(path, "/v1beta/models/") || strings.HasPrefix(path, "/v1/models/")
	}
}

func (m *middleware) withMetrics(next http.Handler) http.Handler {
	skip := map[string]bool{
		"/": true, "/health": true, "/healthz": true, "/readyz": true, "/api/hello": true,
		"/favicon.ico": true,
	}
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
		m.requests.begin()
		// Server.Handler places a recovery layer immediately inside this metrics
		// wrapper, so panics become an observable 500 before accounting completes.
		defer func() {
			elapsed := time.Since(start)
			m.requests.finish(sw.status, elapsed)
			_ = cli.FinishReq(reqID)
			log.Printf(
				"[Server] %s %s - %d (%.3fs) 请求ID=%s",
				r.Method,
				r.URL.Path,
				sw.status,
				elapsed.Seconds(),
				reqID,
			)
		}()
		next.ServeHTTP(sw, r.WithContext(ctx))
	})
}

func (m *middleware) withAPIKey(next http.Handler) http.Handler {
	excluded := map[string]bool{
		"/": true, "/health": true, "/healthz": true, "/readyz": true,
		"/api/hello": true, "/favicon.ico": true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if excluded[r.URL.Path] || isAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		key := extractAPIKey(r)
		if key == "" {
			if isAnthropicPath(r.URL.Path) {
				writeAnthropicMiddlewareError(w, http.StatusUnauthorized, "authentication_error", "missing API key")
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code": 401, "message": "缺少 API 密钥 (missing API key)", "status": "UNAUTHENTICATED",
			}})
			return
		}
		if key == "sk-your-key-here" {
			if isAnthropicPath(r.URL.Path) {
				writeAnthropicMiddlewareError(w, http.StatusUnauthorized, "authentication_error", "example API key is not allowed")
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code": 401, "message": "示例密钥禁止调用，请新建密钥。", "status": "UNAUTHENTICATED",
			}})
			return
		}
		if !m.keys.ValidateKey(key) {
			if isAnthropicPath(r.URL.Path) {
				writeAnthropicMiddlewareError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code": 401, "message": "API 密钥无效 (invalid API key)", "status": "UNAUTHENTICATED",
			}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isAnthropicPath(path string) bool {
	return path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/")
}

func writeAnthropicMiddlewareError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error", "error": map[string]any{"type": typ, "message": message},
		"request_id": reqID24WithPrefix("req_"),
	})
}

func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/") || strings.HasPrefix(path, "/api/admin/") || strings.HasPrefix(path, "/assets/")
}
