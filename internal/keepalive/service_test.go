package keepalive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type mutableConfig struct {
	mu       sync.RWMutex
	url      string
	interval int
}

func (c *mutableConfig) KeepAliveURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.url
}

func (c *mutableConfig) KeepAliveIntervalSeconds() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.interval
}

func (c *mutableConfig) setURL(value string) {
	c.mu.Lock()
	c.url = value
	c.mu.Unlock()
}

func TestServiceStartsImmediatelyAndUsesGET(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := &mutableConfig{url: server.URL + "/ping?secret=hidden", interval: 5}
	service := New(cfg)
	service.Start()
	defer service.Stop()

	select {
	case req := <-requests:
		if req.Method != http.MethodGet || req.URL.Path != "/ping" {
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
		if got := req.Header.Get("User-Agent"); got != "vertex-keepalive/1.0" {
			t.Fatalf("User-Agent = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("keep-alive did not send its initial request immediately")
	}
}

func TestServiceRestartUsesLatestURL(t *testing.T) {
	first := make(chan struct{}, 1)
	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		first <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer firstServer.Close()
	second := make(chan struct{}, 1)
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		second <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer secondServer.Close()

	cfg := &mutableConfig{url: firstServer.URL, interval: 5}
	service := New(cfg)
	service.Start()
	defer service.Stop()
	waitForRequest(t, first, "first URL")

	cfg.setURL(secondServer.URL)
	service.Restart()
	waitForRequest(t, second, "reloaded URL")
}

func TestServiceStopCancelsInflightRequest(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()

	service := New(&mutableConfig{url: server.URL, interval: 5})
	service.Start()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	service.Stop()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel the in-flight request")
	}
}

func TestSafeURLRemovesCredentialsPathQueryAndFragment(t *testing.T) {
	got := safeURL("https://user:pass@example.com/private/token?secret=value#fragment")
	if got != "https://example.com" {
		t.Fatalf("safe URL = %q", got)
	}
}

func waitForRequest(t *testing.T, requests <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}
