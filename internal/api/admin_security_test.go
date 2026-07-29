package api

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

func TestValidateBackgroundImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.White)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	if extension, err := validateBackgroundImage(encoded.Bytes()); err != nil || extension != ".png" {
		t.Fatalf("valid PNG = extension:%q err:%v", extension, err)
	}
	if _, err := validateBackgroundImage([]byte("<script>alert(1)</script>")); err == nil {
		t.Fatal("non-image upload should be rejected")
	}
}

func TestBackgroundAssetFilename(t *testing.T) {
	for _, name := range []string{
		"background.jpg", "background-user.png", "background1.jpeg", "Background1.PNG",
		"background123.jpg", "background999.gif",
	} {
		if !backgroundAssetFilename(name) {
			t.Errorf("background asset %q was not recognized", name)
		}
	}
	for _, name := range []string{
		"background1.txt", "other.jpg", "../background1.png", "background/evil.png",
	} {
		if backgroundAssetFilename(name) {
			t.Errorf("unsupported asset %q was accepted", name)
		}
	}
}

func TestAdminUploadBackgroundSerializesAtomicPublishingAndActiveDeletion(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "config.json")
	t.Setenv("VPROXY_CONFIG", configPath)
	config.InvalidateCache()
	t.Cleanup(config.InvalidateCache)
	if err := config.WriteSettings(map[string]any{"background_image": "url('background.jpg')"}); err != nil {
		t.Fatal(err)
	}

	assetsDir := filepath.Join(root, "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"background100.png", "background.jpg", "background-user.png"} {
		if err := os.WriteFile(filepath.Join(assetsDir, name), []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.White)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	const uploadCount = 8
	requests := make([]*http.Request, uploadCount)
	for index := range requests {
		requests[index] = newBackgroundUploadRequest(t, encoded.Bytes())
	}

	adm := &AdminHandler{handler: handler{cfg: config.GetProvider()}} //nolint:exhaustruct
	recorders := make([]*httptest.ResponseRecorder, uploadCount)
	var wg sync.WaitGroup
	for index := range requests {
		recorders[index] = httptest.NewRecorder()
		wg.Add(1)
		go func() {
			defer wg.Done()
			adm.adminUploadBg(recorders[index], requests[index])
		}()
	}
	wg.Wait()
	for index, recorder := range recorders {
		if recorder.Code != http.StatusOK {
			t.Fatalf("upload[%d] status=%d body=%s", index, recorder.Code, recorder.Body.String())
		}
	}

	entries, err := os.ReadDir(assetsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".background-upload-") {
			t.Fatalf("temporary upload leaked after successful publish: %q", entry.Name())
		}
	}
	if len(entries) != uploadCount+3 {
		t.Fatalf("asset count=%d, want upload history plus existing files", len(entries))
	}
	for _, preserved := range []string{"background100.png", "background.jpg", "background-user.png"} {
		if _, err := os.Stat(filepath.Join(assetsDir, preserved)); err != nil {
			t.Fatalf("existing asset %q was removed: %v", preserved, err)
		}
	}
	current := config.Load().BackgroundImage
	const urlPrefix = "url('/assets/"
	if !strings.HasPrefix(current, urlPrefix) || !strings.HasSuffix(current, "')") {
		t.Fatalf("unexpected background setting %q", current)
	}
	currentFilename := strings.TrimSuffix(strings.TrimPrefix(current, urlPrefix), "')")
	if _, err := os.Stat(filepath.Join(assetsDir, currentFilename)); err != nil {
		t.Fatalf("configured background was not published: %v", err)
	}
	if err := config.WriteSettings(map[string]any{
		"custom_bg_presets": []string{current, "linear-gradient(red, blue)"},
	}); err != nil {
		t.Fatal(err)
	}

	deleteRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/admin/delete-bg",
		strings.NewReader(`{"filename":"`+currentFilename+`"}`),
	)
	deleteRecorder := httptest.NewRecorder()
	adm.adminDeleteBg(deleteRecorder, deleteRequest)
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(assetsDir, currentFilename)); !os.IsNotExist(err) {
		t.Fatalf("active background still exists after delete: %v", err)
	}
	updated := config.Load()
	if updated.BackgroundImage != defaultAdminBackground {
		t.Fatalf("deleted active background setting=%q", updated.BackgroundImage)
	}
	if len(updated.CustomBgPresets) != 1 || updated.CustomBgPresets[0] != "linear-gradient(red, blue)" {
		t.Fatalf("deleted background remained in custom presets: %#v", updated.CustomBgPresets)
	}
}

func newBackgroundUploadRequest(t *testing.T, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "background.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/admin/upload-bg", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestAdminListBgsOnlyReturnsSupportedImages(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "config.json")
	t.Setenv("VPROXY_CONFIG", configPath)
	assetsDir := filepath.Join(root, "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"background1.png", "background2.gif", "background.txt", "other.jpg"} {
		if err := os.WriteFile(filepath.Join(assetsDir, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	adm := &AdminHandler{handler: handler{cfg: config.StaticProvider(config.DefaultConfig())}} //nolint:exhaustruct
	rec := httptest.NewRecorder()
	adm.adminListBgs(rec, httptest.NewRequest(http.MethodGet, "/api/admin/list-bgs", nil))
	body := rec.Body.String()
	for _, want := range []string{"background1.png", "background2.gif"} {
		if !strings.Contains(body, want) {
			t.Fatalf("supported background %q missing from %s", want, body)
		}
	}
	for _, rejected := range []string{"background.txt", "other.jpg"} {
		if strings.Contains(body, rejected) {
			t.Fatalf("unsupported background %q leaked into %s", rejected, body)
		}
	}
}

func TestBodyLimitHotReloadAndChunkedResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	config.InvalidateCache()
	t.Cleanup(config.InvalidateCache)
	if err := config.WriteSettings(map[string]any{"max_request_mb": 1}); err != nil {
		t.Fatal(err)
	}

	mw := &middleware{cfg: config.GetProvider()} //nolint:exhaustruct
	readAll := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			var target struct {
				Value string `json:"value"`
			}
			(&handler{}).decodeAdminBody(w, r, &target) //nolint:exhaustruct
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	limited := mw.withBodyLimit(readAll)

	known := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(make([]byte, 2<<20)))
	knownRec := httptest.NewRecorder()
	limited.ServeHTTP(knownRec, known)
	if knownRec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("known oversized body status=%d", knownRec.Code)
	}

	if err := config.WriteSettings(map[string]any{"max_request_mb": 3}); err != nil {
		t.Fatal(err)
	}
	allowed := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(make([]byte, 2<<20)))
	allowedRec := httptest.NewRecorder()
	limited.ServeHTTP(allowedRec, allowed)
	if allowedRec.Code != http.StatusNoContent {
		t.Fatalf("hot-reloaded body limit did not apply, status=%d", allowedRec.Code)
	}
}

func TestDecodeAdminBodyReturns413ForChunkedLimit(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MaxRequestMB = 1
	mw := &middleware{cfg: config.StaticProvider(cfg)} //nolint:exhaustruct
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Value string `json:"value"`
		}
		(&handler{}).decodeAdminBody(w, r, &body) //nolint:exhaustruct
	})
	payload := `{"value":"` + strings.Repeat("x", 2<<20) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	mw.withBodyLimit(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversized body status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestChatReturns413ForChunkedLimit(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MaxRequestMB = 1
	provider := config.StaticProvider(cfg)
	chat := &ChatHandler{handler: handler{cfg: provider}} //nolint:exhaustruct
	mw := &middleware{cfg: provider}                      //nolint:exhaustruct
	payload := `{"model":"demo","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 2<<20) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	mw.withBodyLimit(http.HandlerFunc(chat.handleChatCompletions)).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked chat oversized body status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRequestIsHTTPSHandlesForwardedProtoList(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://app.example/", nil)
	req.Header.Set("X-Forwarded-Proto", " HTTPS , http")
	if !requestIsHTTPS(req) {
		t.Fatal("first forwarded proto value should be recognized case-insensitively")
	}
}

// TestAdminClientIPUsesRightmostForwardedEntry 锁定登录限流的 IP 归属规则。
// X-Forwarded-For 最左侧由客户端自称，可逐请求伪造以绕过按 IP 的失败锁定；
// 只有最右侧那跳是可信前置代理写入的，必须以它为准。
func TestAdminClientIPUsesRightmostForwardedEntry(t *testing.T) {
	t.Setenv("RENDER", "")
	t.Setenv("VPROXY_TRUST_PROXY_HEADERS", "true")

	tests := []struct {
		name       string
		forwarded  string
		remoteAddr string
		want       string
	}{
		{
			name:       "客户端伪造的最左值必须被忽略",
			forwarded:  "1.2.3.4, 203.0.113.9",
			remoteAddr: "10.0.0.1:5555",
			want:       "203.0.113.9",
		},
		{
			name:       "单跳时取该值",
			forwarded:  "203.0.113.9",
			remoteAddr: "10.0.0.1:5555",
			want:       "203.0.113.9",
		},
		{
			name:       "尾部垃圾值跳过，回退到最近的合法 IP",
			forwarded:  "1.2.3.4, 203.0.113.9, not-an-ip",
			remoteAddr: "10.0.0.1:5555",
			want:       "203.0.113.9",
		},
		{
			name:       "全部非法时回退 RemoteAddr",
			forwarded:  "garbage, junk",
			remoteAddr: "10.0.0.1:5555",
			want:       "10.0.0.1",
		},
		{
			name:       "缺少该头时回退 RemoteAddr",
			forwarded:  "",
			remoteAddr: "10.0.0.1:5555",
			want:       "10.0.0.1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://app.example/api/admin/login", nil)
			req.RemoteAddr = test.remoteAddr
			if test.forwarded != "" {
				req.Header.Set("X-Forwarded-For", test.forwarded)
			}
			if got := adminClientIP(req); got != test.want {
				t.Fatalf("adminClientIP = %q, want %q", got, test.want)
			}
		})
	}
}

// 未显式信任前置代理时，绝不能采信 X-Forwarded-For。
func TestAdminClientIPIgnoresForwardedWhenUntrusted(t *testing.T) {
	t.Setenv("RENDER", "")
	t.Setenv("VPROXY_TRUST_PROXY_HEADERS", "")

	req := httptest.NewRequest(http.MethodPost, "http://app.example/api/admin/login", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := adminClientIP(req); got != "10.0.0.1" {
		t.Fatalf("未信任代理时应使用 RemoteAddr，got %q", got)
	}
}

func TestAdminCORSAndCookieOriginProtection(t *testing.T) {
	mw := &middleware{} //nolint:exhaustruct
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	adminPreflight := httptest.NewRequest(http.MethodOptions, "/api/admin/settings", nil)
	adminRec := httptest.NewRecorder()
	mw.withCORS(next).ServeHTTP(adminRec, adminPreflight)
	if adminRec.Code != http.StatusForbidden ||
		adminRec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("admin preflight should be blocked: status=%d headers=%v", adminRec.Code, adminRec.Header())
	}

	publicPreflight := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	publicRec := httptest.NewRecorder()
	mw.withCORS(next).ServeHTTP(publicRec, publicPreflight)
	if publicRec.Code != http.StatusNoContent ||
		publicRec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("public API preflight changed unexpectedly: status=%d headers=%v", publicRec.Code, publicRec.Header())
	}

	token := issueAdminToken()
	t.Cleanup(func() { dropAdminToken(token) })
	adm := &AdminHandler{} //nolint:exhaustruct
	crossSite := httptest.NewRequest(http.MethodPost, "https://app.example/api/admin/logout", nil)
	crossSite.Host = "app.example"
	crossSite.Header.Set("Origin", "https://evil.example")
	crossSite.AddCookie(&http.Cookie{Name: adminCookieName, Value: token})
	crossRec := httptest.NewRecorder()
	adm.handleAdminAPI(crossRec, crossSite)
	if crossRec.Code != http.StatusForbidden || !checkAdminToken(token) {
		t.Fatalf("cross-origin cookie mutation was not blocked: status=%d", crossRec.Code)
	}

	sameSite := httptest.NewRequest(http.MethodPost, "https://app.example/api/admin/logout", nil)
	sameSite.Host = "app.example"
	sameSite.Header.Set("Origin", "https://app.example")
	sameSite.AddCookie(&http.Cookie{Name: adminCookieName, Value: token})
	sameRec := httptest.NewRecorder()
	adm.handleAdminAPI(sameRec, sameSite)
	if sameRec.Code != http.StatusOK || checkAdminToken(token) {
		t.Fatalf("same-origin logout failed: status=%d body=%s", sameRec.Code, sameRec.Body.String())
	}
}

func TestAdminRoutesRejectWrongMethods(t *testing.T) {
	token := issueAdminToken()
	t.Cleanup(func() { dropAdminToken(token) })
	adm := &AdminHandler{} //nolint:exhaustruct
	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/test", nil)
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: token})
	rec := httptest.NewRecorder()
	adm.handleAdminAPI(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSecurityHeaders(t *testing.T) {
	mw := &middleware{} //nolint:exhaustruct
	req := httptest.NewRequest(http.MethodGet, "https://app.example/admin", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	mw.withSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, req)

	for _, header := range []string{
		"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options",
		"Referrer-Policy", "Permissions-Policy", "Strict-Transport-Security",
	} {
		if rec.Header().Get(header) == "" {
			t.Errorf("missing security header %s", header)
		}
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("admin cache control = %q, want no-store", got)
	}

	// script-src 必须保持严格：面板已全面改用 data-*-action 事件委派，
	// 不存在内联事件处理器或内联 <script>，放开 'unsafe-inline' 会白白
	// 丢掉针对注入脚本的防护。
	csp := rec.Header().Get("Content-Security-Policy")
	scriptSrc := ""
	for _, directive := range strings.Split(csp, ";") {
		directive = strings.TrimSpace(directive)
		if strings.HasPrefix(directive, "script-src") {
			scriptSrc = directive
		}
	}
	if scriptSrc == "" {
		t.Fatalf("CSP 缺少 script-src 指令: %q", csp)
	}
	if strings.Contains(scriptSrc, "unsafe-inline") || strings.Contains(scriptSrc, "unsafe-eval") {
		t.Errorf("script-src 不应放开 unsafe-inline/unsafe-eval，got %q", scriptSrc)
	}
}

func TestConcurrencyLimitRejectsExcessUpstreamWork(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MaxConcurrentRequests = 1
	mw := &middleware{cfg: config.StaticProvider(cfg)} //nolint:exhaustruct
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	limited := mw.withConcurrencyLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))

	go func() {
		defer close(done)
		limited.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		)
	}()
	<-started

	rec := httptest.NewRecorder()
	limited.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("overload response status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	close(release)
	<-done
}
