package api

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

type handler struct {
	vc   *vertex.VertexAIClient
	keys *APIKeyManager
	cfg  config.ConfigProvider
}

func isRequestBodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}

func (h *handler) decodeAdminBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		writeJSON(w, http.StatusBadRequest, adminErr("请求体为空 (empty body)"))
		return false
	}
	if err := decodeJSONValue(r.Body, dst); err != nil {
		if isRequestBodyTooLarge(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, adminErr("请求体过大 (request body too large)"))
			return false
		}
		var trailingError trailingJSONValueError
		if errors.As(err, &trailingError) {
			writeJSON(w, http.StatusBadRequest, adminErr("请求格式错误：只能包含一个 JSON 值"))
			return false
		}
		writeJSON(w, http.StatusBadRequest, adminErr("请求格式错误 (invalid JSON)"))
		return false
	}
	return true
}

func (h *handler) adminUnauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, adminErr("未登录或会话已过期 (unauthorized)"))
}

func (h *handler) adminMethodNotAllowed(w http.ResponseWriter) {
	writeJSON(w, http.StatusMethodNotAllowed, adminErr("方法不允许 (method not allowed)"))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	if status < 100 || status > 599 {
		status = http.StatusInternalServerError
	}
	if err := jsonx.MarshalView(body, func(data []byte) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(data)
	}); err == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"error":{"message":"序列化失败 (internal error)","type":"server_error","code":500}}`))
}

func oaiError(w http.ResponseWriter, status int, msg, errType string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": msg, "type": errType, "code": status,
	}})
}

func sseEvent(obj any) string {
	var event strings.Builder
	event.Grow(256)
	event.WriteString("data: ")
	if err := jsonx.Encode(&event, obj); err != nil {
		return "data: {}\n\n"
	}
	event.WriteByte('\n')
	return event.String()
}

func streamChunkBase(model, requestID string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-" + requestID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
	}
}

func newSSEWriter(w http.ResponseWriter, contentType string) *sseWriter {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := &sseWriter{w: w}
	if flusher != nil {
		sw.flush = flusher.Flush
	}
	return sw
}

var reqCounter uint64 //nolint:gochecknoglobals

func reqID24() string {
	var buf [12]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		now := time.Now().UnixNano()
		count := atomic.AddUint64(&reqCounter, 1)
		var fallback [12]byte
		fallback[0] = byte(now >> 56)
		fallback[1] = byte(now >> 48)
		fallback[2] = byte(now >> 40)
		fallback[3] = byte(now >> 32)
		fallback[4] = byte(now >> 24)
		fallback[5] = byte(now >> 16)
		fallback[6] = byte(now >> 8)
		fallback[7] = byte(now)
		fallback[8] = byte(count >> 24)
		fallback[9] = byte(count >> 16)
		fallback[10] = byte(count >> 8)
		fallback[11] = byte(count)
		return hex.EncodeToString(fallback[:])
	}
	return hex.EncodeToString(buf[:])
}

func vertexErrorToOAI(e *vertex.VertexError) map[string]any {
	var errType string
	switch e.Kind {
	case "invalid":
		errType = "invalid_request_error"
	case "ratelimit":
		errType = "rate_limit_error"
	case "auth":
		errType = "server_error"
	case "notfound", "permission":
		errType = "invalid_request_error"
	default:
		errType = "server_error"
	}
	return map[string]any{"error": map[string]any{
		"message": withUpstreamDetail(vertex.FriendlyErrorMessage(e), e),
		"type":    errType,
		"code":    e.Code,
	}}
}

func withUpstreamDetail(friendly string, e *vertex.VertexError) string {
	detail := strings.TrimSpace(e.Message)
	if detail == "" {
		detail = strings.TrimSpace(e.UpstreamResponse)
	}
	if detail == "" || strings.Contains(friendly, detail) {
		return friendly
	}
	if r := []rune(detail); len(r) > 400 {
		detail = string(r[:400]) + "…"
	}
	return friendly + "（上游原因：" + detail + "）"
}

// toVertexError 把任意错误归一为 *vertex.VertexError 以便映射成对外响应。
// 用 errors.As 而非裸断言：被包装过的上游错误仍需保留其状态码与限流语义，
// 否则 429/404 会被一律降级成 500。
func toVertexError(err error) *vertex.VertexError {
	var ve *vertex.VertexError
	if errors.As(err, &ve) {
		return ve
	}
	return vertex.NewInternalError(err.Error())
}

func isSafetyBlock(e *vertex.VertexError) bool {
	msg := strings.ToLower(e.Message)
	status := strings.ToLower(e.Status)
	for _, k := range []string{"safety", "block_reason", "content_filter", "finish_reason_safety"} {
		if strings.Contains(msg, k) || strings.Contains(status, k) {
			return true
		}
	}
	return false
}

func oaiSafetyResponse(model string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-" + reqID24(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": nil},
			"finish_reason": "content_filter",
		}},
	}
}

func resolveN(raw any, maxN int) (int, string) {
	if maxN <= 0 {
		maxN = 8
	}
	if raw == nil {
		return 1, ""
	}
	var n int
	switch v := raw.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, "请求参数有误: n 必须是整数 (n must be an integer)"
		}
		n = int(v)
	case int:
		n = v
	default:
		return 0, "请求参数有误: n 必须是整数 (n must be an integer)"
	}
	if n < 1 {
		return 0, "请求参数有误: n 必须 >= 1 (n must be >= 1)"
	}
	if n > maxN {
		return 0, "请求参数有误: n 超过上限 " + strconv.Itoa(maxN) + " (n exceeds maximum " + strconv.Itoa(maxN) + ")"
	}
	return n, ""
}

func randomDigits(n int) string {
	const digits = "0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[rand.IntN(len(digits))]
	}
	return string(b)
}

func adminErr(msg string) map[string]any {
	return map[string]any{"error": map[string]any{"message": msg}}
}

func maskKey(key string) string {
	if len(key) <= 4 {
		return "····"
	}
	return "····" + key[len(key)-4:]
}

func generateAPIKey() string {
	b := make([]byte, 24)
	_, _ = cryptorand.Read(b)
	return "sk-" + hex.EncodeToString(b)
}
