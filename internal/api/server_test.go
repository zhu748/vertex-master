package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServerExposesBuildInfo(t *testing.T) {
	server := &Server{
		mw: &middleware{keys: NewAPIKeyManager()},
	}
	server.SetBuildInfo("v1.2.10", "abc1234", "2026-07-27T15:00:00Z")

	recorder := httptest.NewRecorder()
	server.handleRoot(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["version"] != "v1.2.10" || body["build_commit"] != "abc1234" ||
		body["build_time"] != "2026-07-27T15:00:00Z" {
		t.Fatalf("build info not exposed: %#v", body)
	}
}

func TestHealthDoesNotExposeOperationalDetails(t *testing.T) {
	server := &Server{
		mw: &middleware{keys: NewAPIKeyManager()},
	}
	server.SetBuildInfo("v1.2.10", "abc1234", "2026-07-27T15:00:00Z")

	recorder := httptest.NewRecorder()
	server.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "healthy" || body["version"] != "v1.2.10" {
		t.Fatalf("health response missing public status: %#v", body)
	}
	for _, field := range []string{"api_keys_loaded", "build_commit", "build_time"} {
		if _, exposed := body[field]; exposed {
			t.Fatalf("health response exposed %q: %#v", field, body)
		}
	}
}

func TestHealthMethods(t *testing.T) {
	server := &Server{mw: &middleware{keys: NewAPIKeyManager()}}
	for _, test := range []struct {
		method string
		status int
	}{
		{method: http.MethodGet, status: http.StatusOK},
		{method: http.MethodHead, status: http.StatusOK},
		{method: http.MethodPost, status: http.StatusMethodNotAllowed},
	} {
		recorder := httptest.NewRecorder()
		server.handleHealth(recorder, httptest.NewRequest(test.method, "/healthz", nil))
		if recorder.Code != test.status {
			t.Errorf("%s status=%d, want %d", test.method, recorder.Code, test.status)
		}
	}
}

func TestResolveN(t *testing.T) {
	cases := []struct { //nolint:govet
		name    string
		raw     any
		maxN    int
		wantN   int
		wantErr bool
	}{
		{"nil 缺省 1", nil, 8, 1, false},
		{"float 整数", float64(3), 8, 3, false},
		{"int", 4, 8, 4, false},
		{"非整数 float", 2.5, 8, 0, true},
		{"字符串非法", "x", 8, 0, true},
		{"小于 1", float64(0), 8, 0, true},
		{"超上限", float64(20), 8, 0, true},
		{"等于上限 OK", float64(8), 8, 8, false},
		{"maxN<=0 用默认 8", float64(8), 0, 8, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, errMsg := resolveN(c.raw, c.maxN)
			if c.wantErr {
				if errMsg == "" {
					t.Errorf("want error, got n=%d", n)
				}
				return
			}
			if errMsg != "" {
				t.Errorf("unexpected error: %s", errMsg)
			}
			if n != c.wantN {
				t.Errorf("n=%d, want %d", n, c.wantN)
			}
		})
	}
}

// TestStatusWriterFlush 验证 statusWriter 透传 Flush（保 SSE 流式不被破坏）。
func TestStatusWriterFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK} //nolint:exhaustruct
	// httptest.ResponseRecorder 实现 http.Flusher；断言能命中且不 panic。
	if _, ok := interface{}(sw).(http.Flusher); !ok {
		t.Fatal("statusWriter 应实现 http.Flusher")
	}
	sw.Flush()
	if !rec.Flushed {
		t.Fatal("Flush 应透传到底层 ResponseRecorder")
	}
}
