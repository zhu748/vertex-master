package api

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
