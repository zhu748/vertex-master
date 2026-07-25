package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

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
