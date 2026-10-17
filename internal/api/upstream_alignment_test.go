package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

func TestMissingUpstreamFinishDoesNotSynthesizeSuccess(t *testing.T) {
	for _, tc := range []struct{ name, path, body, forbidden string }{
		{"chat", "/v1/chat/completions", `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`, `"finish_reason":"stop"`},
		{"responses", "/v1/responses", `{"model":"gemini-2.5-flash","input":"hi","stream":true}`, "event: response.completed"},
		{"anthropic", "/v1/messages", `{"model":"gemini-2.5-flash","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`, "event: message_stop"},
		{"gemini", "/v1beta/models/gemini-2.5-flash:streamGenerateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, `"finishReason":"STOP"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newTestServer(t)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, `{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":{"candidates":[{"content":{"parts":[{"text":"partial"}],"role":"model"}}]}}}}]}`)
			}))
			defer upstream.Close()
			vertex.SetBatchGraphqlURL(upstream.URL)
			req, err := http.NewRequest(http.MethodPost, fx.server.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sk-test-key")
			resp, err := fx.server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			if !strings.Contains(body, "partial") || !strings.Contains(body, "incomplete") || strings.Contains(body, tc.forbidden) {
				t.Fatalf("unexpected stream: %s", body)
			}
			if calls.Load() != 1 {
				t.Fatalf("partial response replayed %d times", calls.Load())
			}
		})
	}
}

func TestUpstreamRetryAfterReachesCompatibilityClients(t *testing.T) {
	fx := newTestServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Retry-After", "37")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota"}}`)
	}))
	defer upstream.Close()
	vertex.SetBatchGraphqlURL(upstream.URL)
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"gemini-2.5-flash","input":"hi"}`},
		{"/v1/messages", `{"model":"gemini-2.5-flash","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1beta/models/gemini-2.5-flash:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
	} {
		req, err := http.NewRequest(http.MethodPost, fx.server.URL+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk-test-key")
		req.Header.Set("Content-Type", "application/json")
		resp, err := fx.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "37" {
			t.Errorf("%s status=%d retry=%s", tc.path, resp.StatusCode, resp.Header.Get("Retry-After"))
		}
	}
}

func TestAdminDatabaseEndpointsAuthMethodsAndResults(t *testing.T) {
	fx := newTestServer(t)
	token := mustIssueAdminToken(t)
	for _, tc := range []struct {
		path, method string
		auth         bool
		want         int
	}{
		{"stats", "GET", false, 401}, {"check", "POST", false, 401}, {"checkpoint", "POST", false, 401},
		{"stats", "POST", true, 405}, {"check", "GET", true, 405}, {"checkpoint", "GET", true, 405},
		{"stats", "GET", true, 200}, {"check", "POST", true, 200}, {"checkpoint", "POST", true, 200},
	} {
		req, err := http.NewRequest(tc.method, fx.server.URL+"/api/admin/db/"+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.auth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := fx.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, resp.StatusCode, raw)
		}
		if tc.want == 200 {
			var result map[string]any
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if tc.path == "check" && result["ok"] != true {
				t.Fatalf("check=%s", raw)
			}
		}
	}
}

func TestRequestLatencyExcludesActiveRequests(t *testing.T) {
	metrics := &requestMetrics{}
	metrics.begin()
	metrics.begin()
	metrics.finish(200, 20*time.Millisecond)
	got := metrics.snapshot()
	if got.Total != 2 || got.Active != 1 || got.Completed != 1 || got.AverageLatencyMillis != 20 {
		t.Fatalf("snapshot=%+v", got)
	}
}

func TestImageSafetyFinishAcrossStreamingAdapters(t *testing.T) {
	for _, tc := range []struct{ name, path, body, want string }{
		{"chat", "/v1/chat/completions", `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`, `"finish_reason":"content_filter"`},
		{"responses", "/v1/responses", `{"model":"gemini-2.5-flash","input":"hi","stream":true}`, `"reason":"content_filter"`},
		{"anthropic", "/v1/messages", `{"model":"gemini-2.5-flash","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`, `"stop_reason":"refusal"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newTestServer(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, `{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":{"candidates":[{"finishReason":"IMAGE_SAFETY"}],"usageMetadata":{"promptTokenCount":1,"totalTokenCount":1}}}}}]}`)
			}))
			defer upstream.Close()
			vertex.SetBatchGraphqlURL(upstream.URL)
			req, err := http.NewRequest(http.MethodPost, fx.server.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer sk-test-key")
			req.Header.Set("Content-Type", "application/json")
			resp, err := fx.server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if !strings.Contains(string(raw), tc.want) {
				t.Fatalf("missing %s in %s", tc.want, raw)
			}
		})
	}
}
