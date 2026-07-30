package vertex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/recaptcha"
)

var benchmarkTokenCountCacheKey tokenCountCacheKey //nolint:gochecknoglobals

type resolvingCountConfig struct {
	config.ConfigProvider
	alias  string
	target string
}

func (c resolvingCountConfig) ResolveModelName(model string) string {
	if model == c.alias {
		return c.target
	}
	return model
}

func BenchmarkMakeTokenCountCacheKey(b *testing.B) {
	benchmarks := []struct {
		name     string
		contents []any
	}{
		{
			name: "short_text",
			contents: []any{map[string]any{
				"role": "user", "parts": []any{map[string]any{"text": "hello"}},
			}},
		},
		{
			name: "long_text_history",
			contents: func() []any {
				contents := make([]any, 128)
				for index := range contents {
					contents[index] = map[string]any{
						"role": "user", "parts": []any{map[string]any{
							"text": strings.Repeat("0123456789abcdef", 16),
						}},
					}
				}
				return contents
			}(),
		},
	}

	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				key, ok := makeTokenCountCacheKey("gemini-test", benchmark.contents)
				if !ok {
					b.Fatal("cache key unexpectedly exceeded budget")
				}
				benchmarkTokenCountCacheKey = key
			}
		})
	}
}

func TestLiveCountTokens(t *testing.T) {
	if os.Getenv("VERTEX_LIVE_COUNT_TEST") == "" {
		t.Skip("set VERTEX_LIVE_COUNT_TEST=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	input := client.CountTokens(ctx, "gemini-3.6-flash", []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "hello"}},
	}})
	output := client.CountTokens(ctx, "gemini-3.6-flash", []any{map[string]any{
		"role": "model", "parts": []any{map[string]any{"text": "hello"}},
	}})
	if input <= 0 || output <= 0 {
		t.Fatalf("live CountTokens returned zero: input=%d output=%d", input, output)
	}
	t.Logf("live CountTokens: input=%d output=%d", input, output)
}

func TestLiveCountTokenSets(t *testing.T) {
	if os.Getenv("VERTEX_LIVE_COUNT_TEST") == "" {
		t.Skip("set VERTEX_LIVE_COUNT_TEST=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	counts := client.CountTokenSets(
		ctx,
		"gemini-3.6-flash",
		[]any{map[string]any{
			"role": "user", "parts": []any{map[string]any{"text": "hello"}},
		}},
		[]any{map[string]any{
			"role": "model", "parts": []any{map[string]any{"text": "hello world"}},
		}},
	)
	if len(counts) != 2 || counts[0] != 1 || counts[1] != 2 {
		t.Fatalf("live CountTokenSets=%v, want [1 2]", counts)
	}
}

// ---- parseCountTokensResponse：三种 unwrap 形态 + errors 跳过 + 字符串/数字 totalTokens ----

func TestParseCountTokensResponse(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{
			name: "data.ui.countTokensV2 (number)",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":42}}}}]}]`,
			want: 42,
		},
		{
			name: "data.countTokensV2 (number)",
			raw:  `[{"results":[{"data":{"countTokensV2":{"totalTokens":100}}}]}]`,
			want: 100,
		},
		{
			name: "data.countTokens (number)",
			raw:  `[{"results":[{"data":{"countTokens":{"totalTokens":7}}}]}]`,
			want: 7,
		},
		{
			name: "totalTokens as string",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":"256"}}}}]}]`,
			want: 256,
		},
		{
			name: "single object (not array)",
			raw:  `{"results":[{"data":{"countTokensV2":{"totalTokens":15}}}]}`,
			want: 15,
		},
		{
			name: "entry-level errors skipped",
			raw:  `[{"errors":[{"message":"boom"}]},{"results":[{"data":{"countTokensV2":{"totalTokens":9}}}]}]`,
			want: 9,
		},
		{
			name: "result-level errors skipped",
			raw:  `[{"results":[{"errors":[{"x":1}]},{"data":{"countTokensV2":{"totalTokens":11}}}]}]`,
			want: 11,
		},
		{
			name: "ui preferred over flat",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":1}},"countTokensV2":{"totalTokens":999}}}]}]`,
			want: 1,
		},
		{
			name: "no countData returns 0",
			raw:  `[{"results":[{"data":{"somethingElse":{}}}]}]`,
			want: 0,
		},
		{
			name: "missing totalTokens returns 0",
			raw:  `[{"results":[{"data":{"countTokensV2":{}}}]}]`,
			want: 0,
		},
		{
			name: "empty results returns 0",
			raw:  `[{"results":[]}]`,
			want: 0,
		},
		{
			name: "invalid json returns 0",
			raw:  `not json{`,
			want: 0,
		},
		{
			name: "json primitive returns 0",
			raw:  `12345`,
			want: 0,
		},
		{
			name: "empty array returns 0",
			raw:  `[]`,
			want: 0,
		},
		{
			name: "totalTokens non-numeric string returns 0",
			raw:  `[{"results":[{"data":{"countTokensV2":{"totalTokens":"abc"}}}]}]`,
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseCountTokensResponse([]byte(c.raw)); got != c.want {
				t.Errorf("parseCountTokensResponse(%s)=%d，期望 %d", c.raw, got, c.want)
			}
		})
	}
}

func TestParseCountTokensResultDistinguishesZeroAndStructuredErrors(t *testing.T) {
	count, found, err := parseCountTokensResult([]byte(
		`[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":0}}}}]}]`,
	))
	if err != nil || !found || count != 0 {
		t.Fatalf("valid zero count: count=%d found=%v err=%v", count, found, err)
	}

	count, found, err = parseCountTokensResult([]byte(
		`[{"results":[{"data":{"ui":{"countTokensV2":null}},"errors":[{` +
			`"message":"Publisher model was not found","extensions":{"status":{"code":5,` +
			`"message":"Publisher model was not found"}}}]}]}]`,
	))
	var vertexErr *VertexError
	if count != 0 || found || !errors.As(err, &vertexErr) ||
		vertexErr.Code != http.StatusNotFound || vertexErr.Kind != "notfound" {
		t.Fatalf("structured error: count=%d found=%v err=%#v", count, found, err)
	}
}

func TestParseCountTokensResultRejectsInvalidNumericCounts(t *testing.T) {
	for _, raw := range []string{
		`[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":42.5}}}}]}]`,
		`[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":-1}}}}]}]`,
	} {
		count, found, err := parseCountTokensResult([]byte(raw))
		if err != nil || found || count != 0 {
			t.Fatalf("invalid totalTokens should not be accepted: count=%d found=%v err=%v", count, found, err)
		}
	}
}

// ---- coerceTokenCount ----

func TestCoerceTokenCount(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"float64", float64(42), 42},
		{"fractional float64 rejected", float64(42.9), 0},
		{"int", 7, 7},
		{"numeric string", "123", 123},
		{"trimmed not supported (atoi strict)", " 5 ", 0}, // Atoi 不 trim
		{"non-numeric string", "abc", 0},
		{"nil", nil, 0},
		{"bool", true, 0},
		{"zero float", float64(0), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := coerceTokenCount(c.in); got != c.want {
				t.Errorf("coerceTokenCount(%v)=%d，期望 %d", c.in, got, c.want)
			}
		})
	}
}

func TestTokenCountValueRejectsNonFiniteAndOutOfRangeNumbers(t *testing.T) {
	for _, value := range []any{
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		math.MaxFloat64,
		float64(-1),
	} {
		if count, ok := tokenCountValue(value); ok || count != 0 {
			t.Fatalf("tokenCountValue(%v)=(%d,%v), want (0,false)", value, count, ok)
		}
	}
}

func TestCountTokensUsesUpstreamOperation(t *testing.T) {
	oldURL := batchGraphqlURL
	t.Cleanup(func() { batchGraphqlURL = oldURL })

	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":42}}}}]}]`))
	}))
	defer upstream.Close()
	batchGraphqlURL = upstream.URL

	cfg := config.DefaultConfig()
	cfg.ParallelPoolEnabled = false
	client := NewVertexAIClient(config.StaticProvider(cfg))
	client.SetTokenPool(recaptcha.NewTokenPoolCustom(func(string) (string, error) {
		return "test-recaptcha-token", nil
	}))

	contents := []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "hello"}},
	}}
	if got := client.CountTokens(context.Background(), "gemini-test", contents); got != 42 {
		t.Fatalf("CountTokens=%d, want 42", got)
	}
	if received["operationName"] != "CountTokens" {
		t.Fatalf("未调用上游 CountTokens operation: %#v", received)
	}
	variables, _ := received["variables"].(map[string]any)
	if variables["recaptchaToken"] != "test-recaptcha-token" {
		t.Fatalf("CountTokens 请求缺少 recaptchaToken: %#v", variables)
	}
}

func TestCountTokensResolvesModelAliasBeforeCacheAndUpstream(t *testing.T) {
	oldURL := batchGraphqlURL
	t.Cleanup(func() { batchGraphqlURL = oldURL })

	var receivedModel any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		if variables, ok := request["variables"].(map[string]any); ok {
			receivedModel = variables["model"]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":9}}}}]}]`))
	}))
	defer upstream.Close()
	batchGraphqlURL = upstream.URL

	base := config.DefaultConfig()
	base.ParallelPoolEnabled = false
	cfg := resolvingCountConfig{
		ConfigProvider: config.StaticProvider(base),
		alias:          "story-fast",
		target:         "gemini-3.6-flash",
	}
	client := NewVertexAIClient(cfg)
	client.SetTokenPool(recaptcha.NewTokenPoolCustom(func(string) (string, error) {
		return "test-recaptcha-token", nil
	}))

	contents := []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "alias-token-test"}},
	}}
	if got := client.CountTokens(context.Background(), "story-fast", contents); got != 9 {
		t.Fatalf("CountTokens=%d, want 9", got)
	}
	if receivedModel != "gemini-3.6-flash" {
		t.Fatalf("CountTokens 上游模型=%v, want gemini-3.6-flash", receivedModel)
	}
}

func TestCountTokensExactPropagatesStructuredUpstreamError(t *testing.T) {
	oldURL := batchGraphqlURL
	t.Cleanup(func() { batchGraphqlURL = oldURL })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`[{"results":[{"data":{"ui":{"countTokensV2":null}},"errors":[{` +
				`"message":"Publisher model was not found","extensions":{"status":{"code":5,` +
				`"message":"Publisher model was not found"}}}]}]}]`,
		))
	}))
	defer upstream.Close()
	batchGraphqlURL = upstream.URL

	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	client.SetTokenPool(recaptcha.NewTokenPoolCustom(func(string) (string, error) {
		return "test-recaptcha-token", nil
	}))
	count, err := client.CountTokensExact(context.Background(), "missing-model", []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}},
	})
	var vertexErr *VertexError
	if count != 0 || !errors.As(err, &vertexErr) || vertexErr.Code != http.StatusNotFound {
		t.Fatalf("CountTokensExact count=%d err=%#v", count, err)
	}
}

func TestCountTokenSetsSharesRecaptchaAcrossConcurrentRequests(t *testing.T) {
	oldURL := batchGraphqlURL
	t.Cleanup(func() { batchGraphqlURL = oldURL })

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		upstreamCalls.Add(1)
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		variables, _ := request["variables"].(map[string]any)
		contents, _ := variables["contents"].([]any)
		count := 42
		if len(contents) > 0 {
			content, _ := contents[0].(map[string]any)
			if content["role"] == "model" {
				count = 7
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":%d}}}}]}]`, count)
	}))
	defer upstream.Close()
	batchGraphqlURL = upstream.URL

	var tokenFetches atomic.Int32
	cfg := config.DefaultConfig()
	cfg.ParallelPoolEnabled = false
	client := NewVertexAIClient(config.StaticProvider(cfg))
	client.SetTokenPool(recaptcha.NewTokenPoolCustom(func(string) (string, error) {
		tokenFetches.Add(1)
		return "shared-recaptcha-token", nil
	}))

	counts := client.CountTokenSets(
		context.Background(),
		"gemini-test",
		[]any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
		[]any{map[string]any{"role": "model", "parts": []any{map[string]any{"text": "world"}}}},
	)
	if len(counts) != 2 || counts[0] != 42 || counts[1] != 7 {
		t.Fatalf("CountTokenSets=%v, want [42 7]", counts)
	}
	if tokenFetches.Load() != 1 {
		t.Fatalf("recaptcha fetches=%d, want 1", tokenFetches.Load())
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want 2", upstreamCalls.Load())
	}

	cached := client.CountTokenSets(
		context.Background(),
		"gemini-test",
		[]any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
		[]any{map[string]any{"role": "model", "parts": []any{map[string]any{"text": "world"}}}},
	)
	if len(cached) != 2 || cached[0] != 42 || cached[1] != 7 {
		t.Fatalf("cached CountTokenSets=%v, want [42 7]", cached)
	}
	if tokenFetches.Load() != 1 || upstreamCalls.Load() != 2 {
		t.Fatalf(
			"缓存命中后仍访问上游: recaptcha=%d upstream=%d",
			tokenFetches.Load(), upstreamCalls.Load(),
		)
	}
	empty := client.CountTokenSets(context.Background(), "gemini-test", nil, []any{})
	if len(empty) != 2 || empty[0] != 0 || empty[1] != 0 || tokenFetches.Load() != 1 {
		t.Fatalf("empty sets should not fetch recaptcha: counts=%v fetches=%d", empty, tokenFetches.Load())
	}
}

func TestCountTokensSingleflightMergesConcurrentIdenticalRequests(t *testing.T) {
	oldURL := batchGraphqlURL
	t.Cleanup(func() { batchGraphqlURL = oldURL })

	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		startOnce.Do(func() { close(started) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":42}}}}]}]`))
	}))
	defer upstream.Close()
	batchGraphqlURL = upstream.URL

	var tokenFetches atomic.Int32
	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	client.SetTokenPool(recaptcha.NewTokenPoolCustom(func(string) (string, error) {
		tokenFetches.Add(1)
		return "singleflight-token", nil
	}))
	contents := []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "same request"}},
	}}

	const concurrency = 8
	results := make([]int, concurrency)
	var wg sync.WaitGroup
	for index := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[index] = client.CountTokens(context.Background(), "gemini-test", contents)
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("singleflight owner did not reach upstream")
	}
	waitForCountTokenSharedWaits(t, client, concurrency-1)
	close(release)
	wg.Wait()

	for index, count := range results {
		if count != 42 {
			t.Fatalf("result[%d]=%d, want 42", index, count)
		}
	}
	stats := client.CountTokenStats()
	if tokenFetches.Load() != 1 || upstreamCalls.Load() != 1 ||
		stats.CacheMisses != concurrency || stats.SharedWaits != concurrency-1 ||
		stats.UpstreamQueries != 1 || stats.HTTPRequests != 1 || stats.Failures != 0 ||
		stats.CacheEntries != 1 || stats.InFlight != 0 {
		t.Fatalf(
			"unexpected singleflight stats: token_fetches=%d upstream=%d stats=%+v",
			tokenFetches.Load(), upstreamCalls.Load(), stats,
		)
	}
	if cached := client.CountTokens(context.Background(), "gemini-test", contents); cached != 42 {
		t.Fatalf("cached CountTokens=%d, want 42", cached)
	}
	if stats = client.CountTokenStats(); stats.CacheHits != 1 || upstreamCalls.Load() != 1 {
		t.Fatalf("cache hit not recorded: upstream=%d stats=%+v", upstreamCalls.Load(), stats)
	}
}

func TestCountTokensExactSingleflightSharesStructuredError(t *testing.T) {
	oldURL := batchGraphqlURL
	t.Cleanup(func() { batchGraphqlURL = oldURL })

	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		startOnce.Do(func() { close(started) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`[{"results":[{"errors":[{"message":"model not found",` +
				`"extensions":{"status":{"code":5,"message":"model not found"}}}]}]}]`,
		))
	}))
	defer upstream.Close()
	batchGraphqlURL = upstream.URL

	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	client.SetTokenPool(recaptcha.NewTokenPoolCustom(func(string) (string, error) {
		return "singleflight-error-token", nil
	}))
	contents := []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "same failed request"}},
	}}

	const concurrency = 8
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for index := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[index] = client.CountTokensExact(context.Background(), "missing-model", contents)
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("singleflight owner did not reach upstream")
	}
	waitForCountTokenSharedWaits(t, client, concurrency-1)
	close(release)
	wg.Wait()

	for index, err := range errs {
		var vertexErr *VertexError
		if !errors.As(err, &vertexErr) || vertexErr.Code != http.StatusNotFound {
			t.Fatalf("error[%d]=%#v, want VertexError 404", index, err)
		}
	}
	stats := client.CountTokenStats()
	if upstreamCalls.Load() != 1 || stats.UpstreamQueries != 1 || stats.HTTPRequests != 1 ||
		stats.SharedWaits != concurrency-1 || stats.Failures != 1 ||
		stats.CacheEntries != 0 || stats.InFlight != 0 {
		t.Fatalf("unexpected failed singleflight stats: upstream=%d stats=%+v", upstreamCalls.Load(), stats)
	}
}

func TestCountTokensSingleflightWaiterCancellationDoesNotCancelOwner(t *testing.T) {
	oldURL := batchGraphqlURL
	t.Cleanup(func() { batchGraphqlURL = oldURL })

	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":42}}}}]}]`))
	}))
	defer upstream.Close()
	batchGraphqlURL = upstream.URL

	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	client.SetTokenPool(recaptcha.NewTokenPoolCustom(func(string) (string, error) {
		return "singleflight-token", nil
	}))
	contents := []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "cancel waiter"}},
	}}
	ownerResult := make(chan int, 1)
	go func() {
		ownerResult <- client.CountTokens(context.Background(), "gemini-test", contents)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("owner did not reach upstream")
	}

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterResult := make(chan int, 1)
	go func() {
		waiterResult <- client.CountTokens(waiterCtx, "gemini-test", contents)
	}()
	waitForCountTokenSharedWaits(t, client, 1)
	cancelWaiter()
	select {
	case count := <-waiterResult:
		if count != 0 {
			t.Fatalf("canceled waiter count=%d, want 0", count)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not return")
	}
	close(release)
	select {
	case count := <-ownerResult:
		if count != 42 {
			t.Fatalf("owner count=%d, want 42", count)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter cancellation stopped owner")
	}
}

func TestCountTokensCancellationStopsTokenFetchAndReleasesFlight(t *testing.T) {
	started := make(chan struct{})
	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	client.SetTokenPool(recaptcha.NewTokenPoolCustomContext(
		func(ctx context.Context, _ string) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
	))
	contents := []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "cancel token fetch"}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan int, 1)
	go func() {
		result <- client.CountTokens(ctx, "gemini-test", contents)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("token fetch did not start")
	}
	cancel()
	select {
	case count := <-result:
		if count != 0 {
			t.Fatalf("canceled CountTokens=%d, want 0", count)
		}
	case <-time.After(time.Second):
		t.Fatal("CountTokens did not stop after cancellation")
	}
	stats := client.CountTokenStats()
	if stats.InFlight != 0 || stats.Failures != 1 || stats.HTTPRequests != 0 {
		t.Fatalf("canceled token fetch left dirty state: %+v", stats)
	}
}

func TestCountTokensSingleflightFailureCanRetry(t *testing.T) {
	oldURL := batchGraphqlURL
	t.Cleanup(func() { batchGraphqlURL = oldURL })

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`[{"results":[{"data":{"ui":{"countTokensV2":{}}}}]}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":42}}}}]}]`))
	}))
	defer upstream.Close()
	batchGraphqlURL = upstream.URL

	var tokenFetches atomic.Int32
	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	client.SetTokenPool(recaptcha.NewTokenPoolCustom(func(string) (string, error) {
		tokenFetches.Add(1)
		return "retry-token", nil
	}))
	contents := []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "retry request"}},
	}}
	if first := client.CountTokens(context.Background(), "gemini-test", contents); first != 0 {
		t.Fatalf("first failed count=%d, want 0", first)
	}
	if second := client.CountTokens(context.Background(), "gemini-test", contents); second != 42 {
		t.Fatalf("retry count=%d, want 42", second)
	}
	stats := client.CountTokenStats()
	if tokenFetches.Load() != 2 || upstreamCalls.Load() != 2 || stats.Failures != 1 ||
		stats.UpstreamQueries != 2 || stats.CacheEntries != 1 || stats.InFlight != 0 {
		t.Fatalf(
			"failed flight was not retried cleanly: token_fetches=%d upstream=%d stats=%+v",
			tokenFetches.Load(), upstreamCalls.Load(), stats,
		)
	}
}

func waitForCountTokenSharedWaits(t *testing.T, client *VertexAIClient, want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if client.CountTokenStats().SharedWaits >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("shared waits=%d, want at least %d", client.CountTokenStats().SharedWaits, want)
}

func TestTokenCountCacheSkipsLargePayloads(t *testing.T) {
	if _, ok := makeTokenCountCacheKey("gemini-test", []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "hello"}},
	}}); !ok {
		t.Fatal("small text payload should be cacheable")
	}
	if _, ok := makeTokenCountCacheKey("gemini-test", []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{
			"inlineData": map[string]any{
				"mimeType": "image/png", "data": strings.Repeat("A", tokenCountCacheMaxBytes+1),
			},
		}},
	}}); ok {
		t.Fatal("large media payload should bypass token count cache")
	}
	if _, ok := makeTokenCountCacheKey(strings.Repeat("m", tokenCountCacheMaxBytes+1), []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}},
	}); ok {
		t.Fatal("oversized model name should bypass token count cache")
	}
}

func TestTokenCountCacheKeyIsDeterministicAndTypeSafe(t *testing.T) {
	first := []any{map[string]any{
		"role": "user",
		"parts": []any{map[string]any{
			"text": "hello", "enabled": true, "weight": float64(1),
		}},
	}}
	secondPart := make(map[string]any)
	secondPart["weight"] = float64(1)
	secondPart["enabled"] = true
	secondPart["text"] = "hello"
	secondContent := make(map[string]any)
	secondContent["parts"] = []any{secondPart}
	secondContent["role"] = "user"
	second := []any{secondContent}

	firstKey, firstOK := makeTokenCountCacheKey("gemini-test", first)
	secondKey, secondOK := makeTokenCountCacheKey("gemini-test", second)
	if !firstOK || !secondOK || firstKey != secondKey {
		t.Fatalf("equivalent maps must produce the same key: first=%x second=%x", firstKey, secondKey)
	}

	changedModel, _ := makeTokenCountCacheKey("gemini-other", first)
	changedText, _ := makeTokenCountCacheKey("gemini-test", []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{
			"text": "world", "enabled": true, "weight": float64(1),
		}},
	}})
	changedType, _ := makeTokenCountCacheKey("gemini-test", []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{
			"text": "hello", "enabled": true, "weight": int64(1),
		}},
	}})
	for name, key := range map[string]tokenCountCacheKey{
		"model": changedModel,
		"text":  changedText,
		"type":  changedType,
	} {
		if key == firstKey {
			t.Fatalf("changed %s collided with original cache key", name)
		}
	}

	if _, ok := makeTokenCountCacheKey("gemini-test", []any{map[string]any{
		"unsupported": func() {},
	}}); ok {
		t.Fatal("unsupported Go values must bypass token count cache")
	}
}

func TestTokenCountCacheExpiresAndRemainsBounded(t *testing.T) {
	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	var expiredKey tokenCountCacheKey
	expiredKey[0] = 1
	client.countCache[expiredKey] = tokenCountCacheEntry{
		count: 9, storedAt: time.Now().Add(-tokenCountCacheTTL),
	}
	if entries := client.CountTokenStats().CacheEntries; entries != 0 {
		t.Fatalf("stats included %d expired cache entries, want 0", entries)
	}
	client.countCache[expiredKey] = tokenCountCacheEntry{
		count: 9, storedAt: time.Now().Add(-tokenCountCacheTTL),
	}
	if count, ok := client.loadTokenCountCache(expiredKey); ok || count != 0 {
		t.Fatalf("expired cache entry returned count=%d hit=%v", count, ok)
	}

	for index := 0; index < tokenCountCacheMaxItems+32; index++ {
		var key tokenCountCacheKey
		key[0] = byte(index)
		key[1] = byte(index >> 8)
		client.storeTokenCountCache(key, index+1)
	}
	client.countCacheMu.Lock()
	cacheSize := len(client.countCache)
	client.countCacheMu.Unlock()
	if cacheSize != tokenCountCacheMaxItems {
		t.Fatalf("cache size=%d, want bounded at %d", cacheSize, tokenCountCacheMaxItems)
	}

	var existingKey tokenCountCacheKey
	existingKey[0] = byte(tokenCountCacheMaxItems - 1)
	client.storeTokenCountCache(existingKey, 999)
	client.countCacheMu.Lock()
	cacheSize = len(client.countCache)
	updated := client.countCache[existingKey].count
	client.countCacheMu.Unlock()
	if cacheSize != tokenCountCacheMaxItems || updated != 999 {
		t.Fatalf("refreshing existing key changed capacity: size=%d count=%d", cacheSize, updated)
	}
}
