package vertex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/recaptcha"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

func TestStreamRejectsMalformedCompleteJSON(t *testing.T) {
	for _, raw := range []string{`{"text":broken}`, `{"text":"x",}`, `{"a":1 "b":2}`} {
		seen := false
		err := scanStream(strings.NewReader(raw), func(map[string]any) (bool, error) { seen = true; return false, nil })
		if seen || !errors.Is(err, errInvalidStreamJSON) {
			t.Fatalf("raw=%s seen=%v err=%v", raw, seen, err)
		}
		_, err = processStreamingJSON([]byte(raw), func(StreamChunk) bool { t.Fatal("invalid frame delivered"); return true })
		if !errors.Is(err, errInvalidStreamJSON) {
			t.Fatalf("raw processor error=%v", err)
		}
	}
}

func TestStreamingAttemptIntegrityAndTrailingUsage(t *testing.T) {
	text := wrap(`{"candidates":[{"content":{"parts":[{"text":"partial"}],"role":"model"}}]}`)
	finish := wrap(`{"candidates":[{"finishReason":"STOP"}]}`)
	usage := wrap(`{"usageMetadata":{"totalTokenCount":23}}`)
	secondText := wrap(`{"candidates":[{"index":1,"content":{"parts":[{"text":"second"}],"role":"model"}}]}`)
	secondFinish := wrap(`{"candidates":[{"index":1,"finishReason":"STOP"}]}`)
	for _, tc := range []struct {
		name, body string
		incomplete bool
		frames     int
	}{
		{"missing_finish", text, true, 1},
		{"unspecified_finish", text + wrap(`{"candidates":[{"finishReason":"FINISH_REASON_UNSPECIFIED"}]}`), true, 2},
		{"finish_then_usage", text + finish + usage, false, 3},
		{"blocked_prompt", wrap(`{"promptFeedback":{"blockReason":"SAFETY"}}`), false, 1},
		{"auth_empty", "", false, 0},
		{"metadata_only", usage, false, 1},
		{"one_of_two_candidates_unfinished", text + secondText + secondFinish, true, 3},
		{"both_candidates_finished", text + secondText + secondFinish + finish, false, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			oldURL := batchGraphqlURL
			batchGraphqlURL = upstream.URL
			defer func() { batchGraphqlURL = oldURL }()
			cfg := config.StaticProvider(config.DefaultConfig())
			client := NewVertexAIClient(cfg)
			session, err := transport.NewNetworkClient(false).CreateSession(5, "", "test")
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			frames := 0
			err = client.executeStreamingAttempt(context.Background(), session, "gemini-2.5-flash", "direct", map[string]any{}, "token", false, func(chunk StreamChunk) bool { frames++; return true })
			if errors.Is(err, ErrStreamIncomplete) != tc.incomplete || frames != tc.frames {
				t.Fatalf("err=%v frames=%d", err, frames)
			}
		})
	}
}

func TestIdleStreamReaderClosesSilentUpstream(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	var closed atomic.Int32
	r := &idleStreamReader{body: reader, timeout: 20 * time.Millisecond, closeBody: func() { closed.Add(1); _ = reader.Close() }}
	done := make(chan error, 1)
	go func() { _, err := r.Read(make([]byte, 1)); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, errStreamIdleTimeout) || closed.Load() != 1 {
			t.Fatalf("err=%v closed=%d", err, closed.Load())
		}
	case <-time.After(2 * time.Second):
		_ = reader.Close()
		t.Fatal("idle read did not unblock")
	}
}

func TestIdleStreamReaderDoesNotTimeDownstreamDelivery(t *testing.T) {
	var closed atomic.Bool
	r := &idleStreamReader{body: strings.NewReader("ab"), timeout: 10 * time.Millisecond, closeBody: func() { closed.Store(true) }}
	if _, err := r.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if closed.Load() {
		t.Fatal("downstream processing was charged to upstream idle timeout")
	}
	if _, err := r.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"17", 17}, {" 2 ", 2}, {"-1", 0}, {"0.5", 0}, {"bad", 0},
		{"18446744073709551615", maxRetryAfterSeconds},
		{now.Add(1500 * time.Millisecond).Format(http.TimeFormat), 1},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{now.Add(5 * time.Hour).Format(http.TimeFormat), maxRetryAfterSeconds},
	} {
		if got := parseRetryAfterHeader(map[string][]string{"retry-after": {tc.value}}, now); got != tc.want {
			t.Errorf("%q: got %d want %d", tc.value, got, tc.want)
		}
	}
}

func TestVideoGuardDoesNotAcquireToken(t *testing.T) {
	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	resp, err := client.CompleteChat(context.Background(), "veo-3-generate-001", nil)
	ve := asVertexError(err)
	if resp != nil || ve == nil || ve.Code != 501 || ve.IsRetryable() {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	var streamErr *VertexError
	client.StreamChat(context.Background(), "veo-3-generate-001", nil, func(chunk StreamChunk) bool { streamErr = chunk.Err; return true })
	if streamErr == nil || streamErr.Code != 501 {
		t.Fatalf("stream error=%v", streamErr)
	}
}

func TestPartialStreamDoesNotRetryDespiteRetryBudget(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, wrap(`{"candidates":[{"content":{"parts":[{"text":"partial"}],"role":"model"}}]}`))
	}))
	defer upstream.Close()
	oldURL := batchGraphqlURL
	batchGraphqlURL = upstream.URL
	defer func() { batchGraphqlURL = oldURL }()
	cfg := config.DefaultConfig()
	cfg.MaxRetries = 3
	cfg.ParallelPoolEnabled = false
	client := NewVertexAIClient(config.StaticProvider(cfg))
	client.SetTokenPool(recaptcha.NewTokenPoolCustom(func(string) (string, error) { return "token", nil }))
	var chunks int
	var lastErr *VertexError
	client.executeStreamingWithRetries(context.Background(), "gemini-2.5-flash", nil, "", func(chunk StreamChunk) bool {
		if chunk.Err != nil {
			lastErr = chunk.Err
		} else {
			chunks++
		}
		return true
	})
	if calls.Load() != 1 || chunks != 1 || lastErr == nil || !errors.Is(lastErr, ErrStreamIncomplete) {
		t.Fatalf("calls=%d chunks=%d err=%v", calls.Load(), chunks, lastErr)
	}
}

func TestUpstreamErrorLogsNeverDumpPromptsOrCredentials(t *testing.T) {
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprint(debug), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":{"message":"bad request","api_key":"PRIVATE_ERROR_KEY"}}`)
			}))
			defer upstream.Close()
			oldURL := batchGraphqlURL
			batchGraphqlURL = upstream.URL
			defer func() { batchGraphqlURL = oldURL }()
			cfg := config.DefaultConfig()
			cfg.DebugMode = debug
			client := NewVertexAIClient(config.StaticProvider(cfg))
			session, err := client.net.CreateSession(5, "", "log-test")
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			logs := captureLogs(t)
			err = client.executeStreamingAttempt(context.Background(), session, "gemini-2.5-flash", "direct", map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]any{"text": "PRIVATE_PROMPT"}}}}}, "PRIVATE_RECAPTCHA", false, func(StreamChunk) bool { return true })
			if err == nil {
				t.Fatal("expected upstream error")
			}
			if strings.Contains(logs.String(), "PRIVATE_") {
				t.Fatalf("log leaked private data: %s", logs.String())
			}
		})
	}
}

func TestLongRetryAfterFailsOverWithoutSleeping(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Retry-After", "86400")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota"}}`)
	}))
	defer upstream.Close()
	oldURL := batchGraphqlURL
	batchGraphqlURL = upstream.URL
	defer func() { batchGraphqlURL = oldURL }()
	cfg := config.DefaultConfig()
	cfg.MaxRetries = 3
	cfg.ParallelPoolEnabled = false
	client := NewVertexAIClient(config.StaticProvider(cfg))
	client.SetTokenPool(recaptcha.NewTokenPoolCustom(func(string) (string, error) { return "token", nil }))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var lastErr *VertexError
	client.executeStreamingWithRetries(ctx, "gemini-2.5-flash", nil, "", func(chunk StreamChunk) bool { lastErr = chunk.Err; return false })
	if lastErr == nil || lastErr.Code != 429 || lastErr.RetryAfter != maxRetryAfterSeconds {
		t.Fatalf("err=%+v ctx=%v", lastErr, ctx.Err())
	}
}

func TestFirstByteAllowanceAndBetweenByteTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	r := &idleStreamReader{body: reader, timeout: 10 * time.Millisecond, firstTimeout: 200 * time.Millisecond, closeBody: func() { _ = reader.Close() }}
	go func() { time.Sleep(30 * time.Millisecond); _, _ = writer.Write([]byte("a")) }()
	if n, err := r.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("first read n=%d err=%v", n, err)
	}
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, errStreamIdleTimeout) {
		t.Fatalf("between-byte error=%v", err)
	}
}
