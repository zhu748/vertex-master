package vertex

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

var benchmarkCandidatePayload map[string]any        //nolint:gochecknoglobals
var benchmarkCandidateBodies [5]batchGraphqlRequest //nolint:gochecknoglobals
var benchmarkRandomString string                    //nolint:gochecknoglobals
var benchmarkRandomString2 string                   //nolint:gochecknoglobals
var benchmarkRandomInt64 int64                      //nolint:gochecknoglobals
var benchmarkEncodedRequest []byte                  //nolint:gochecknoglobals

func BenchmarkRandomRequestIdentifiers(b *testing.B) {
	b.Run("tracking", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkRandomString = randomTrackingID()
		}
	})
	b.Run("page-view", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkRandomInt64 = randomPageViewID()
		}
	})
	b.Run("uuid", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkRandomString = randomUUID()
		}
	})
	b.Run("combined", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkRandomString, benchmarkRandomString2 = randomRequestIdentifiers()
		}
	})
}

func TestRandomRequestIdentifierFormats(t *testing.T) {
	for range 1000 {
		tracking := randomTrackingID()
		if len(tracking) != 17 || tracking[0] != 'd' {
			t.Fatalf("tracking ID format=%q", tracking)
		}
		for index := 1; index < len(tracking); index++ {
			if tracking[index] < '0' || tracking[index] > '9' {
				t.Fatalf("tracking ID contains non-digit: %q", tracking)
			}
		}

		pageView := randomPageViewID()
		if pageView < 1000000000000000 || pageView >= 10000000000000000 {
			t.Fatalf("page view ID out of range: %d", pageView)
		}

		uuid := randomUUID()
		if len(uuid) != 36 || uuid[8] != '-' || uuid[13] != '-' || uuid[18] != '-' || uuid[23] != '-' {
			t.Fatalf("UUID format=%q", uuid)
		}
		if uuid[14] != '4' || !strings.ContainsRune("89AB", rune(uuid[19])) {
			t.Fatalf("UUID version/variant=%q", uuid)
		}

		combinedUUID, combinedTracking := randomRequestIdentifiers()
		if len(combinedUUID) != 36 || combinedUUID[14] != '4' ||
			len(combinedTracking) != 17 || combinedTracking[0] != 'd' {
			t.Fatalf("combined identifier format: uuid=%q tracking=%q", combinedUUID, combinedTracking)
		}
	}
}

func BenchmarkPayloadForCandidateLarge(b *testing.B) {
	parts := make([]any, 4096)
	for index := range parts {
		parts[index] = map[string]any{
			"text":     "0123456789abcdef0123456789abcdef",
			"metadata": map[string]any{"index": index, "enabled": true},
		}
	}
	payload := map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": parts}},
		"generationConfig": map[string]any{"temperature": 0.5, "maxOutputTokens": 2048},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkCandidatePayload = payloadForCandidate(payload)
	}
}

func BenchmarkPrepareFiveCandidateRequests(b *testing.B) {
	parts := make([]any, 1024)
	for index := range parts {
		parts[index] = map[string]any{
			"text":     "0123456789abcdef0123456789abcdef",
			"metadata": map[string]any{"index": index, "enabled": true},
		}
	}
	payload := map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": parts}},
		"generationConfig": map[string]any{"temperature": 0.5, "maxOutputTokens": 2048},
	}
	cfg := config.StaticProvider(config.DefaultConfig())
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		preparedVariables := buildRequestVariables("gemini-3.1-flash", payload, cfg)
		for candidate := range benchmarkCandidateBodies {
			benchmarkCandidateBodies[candidate] = buildRequestPayloadFromVariables(
				preparedVariables, "TOKEN123",
			)
		}
	}
}

func BenchmarkPrepareCanonicalTextRequest(b *testing.B) {
	parts := make([]any, 1024)
	for index := range parts {
		parts[index] = map[string]any{"text": "0123456789abcdef0123456789abcdef"}
	}
	payload := map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": parts}},
		"generationConfig": map[string]any{"temperature": 0.5, "maxOutputTokens": 2048},
	}
	cfg := config.StaticProvider(config.DefaultConfig())
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		preparedVariables := buildRequestVariables("gemini-3.1-flash", payload, cfg)
		for candidate := range benchmarkCandidateBodies {
			benchmarkCandidateBodies[candidate] = buildRequestPayloadFromVariables(
				preparedVariables, "TOKEN123",
			)
		}
	}
}

func BenchmarkPrepareAndMarshalCanonicalRequest(b *testing.B) {
	parts := make([]any, 8)
	for index := range parts {
		parts[index] = map[string]any{"text": "0123456789abcdef0123456789abcdef"}
	}
	payload := map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": parts}},
		"generationConfig": map[string]any{"temperature": 0.5, "maxOutputTokens": 2048},
	}
	cfg := config.StaticProvider(config.DefaultConfig())
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		body := buildRequestPayload("gemini-3.1-flash", payload, "TOKEN123", cfg)
		encoded, err := json.Marshal(body)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkEncodedRequest = encoded
	}
}

func TestParseErrorResponse(t *testing.T) {
	e := parseErrorResponse(map[string]any{"error": map[string]any{
		"code": float64(404), "message": "not found", "status": "NOT_FOUND",
	}})
	if e == nil || e.Kind != "notfound" {
		t.Errorf("got %v", e)
	}
	// GraphQL errors 数组
	e2 := parseErrorResponse(map[string]any{"errors": []any{
		map[string]any{"message": "boom", "code": float64(500)},
	}})
	if e2 == nil {
		t.Error("errors 数组未解析")
	}

	invalidCode := parseErrorResponse(map[string]any{
		"error": map[string]any{"code": float64(429.5), "message": "malformed code"},
	})
	if invalidCode == nil || invalidCode.Code != 500 || invalidCode.Kind != "server" {
		t.Errorf("fractional error code should use safe default: %#v", invalidCode)
	}
}

func TestToIntRejectsInvalidNumbers(t *testing.T) {
	for _, value := range []any{
		float64(429.5),
		math.NaN(),
		math.Inf(1),
		float64(math.MaxInt) + 1,
	} {
		if got := toInt(value, 500); got != 500 {
			t.Errorf("toInt(%v, 500)=%d, want 500", value, got)
		}
	}
	if got := toInt(float64(429), 500); got != 429 {
		t.Errorf("toInt(429, 500)=%d, want 429", got)
	}
}

func TestAuthError502(t *testing.T) {
	e := NewAuthenticationError("x")
	if e.Code != 502 {
		t.Errorf("auth code=%d, want 502（红线：避免网关误判禁用渠道）", e.Code)
	}
	if !e.IsRetryable() {
		t.Error("auth 应可重试")
	}
}

func TestRaiseForStatus(t *testing.T) {
	if raiseForStatus(429, "", "x", nil, "").Kind != "ratelimit" {
		t.Error("429 → ratelimit")
	}
	if raiseForStatus(401, "", "x", nil, "").Code != 502 {
		t.Error("401 → auth(502)")
	}
	if raiseForStatus(400, "", "x", nil, "").Kind != "invalid" {
		t.Error("400 → invalid")
	}
}

func TestBuildRequestPayload(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	payload := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}},
	}}
	body := buildRequestPayload("gemini-3.1-flash", payload, "TOKEN123", cfg)
	if body.QuerySignature != querySignature {
		t.Error("querySignature 不匹配")
	}
	if body.OperationName != "StreamGenerateContentAnonymous" {
		t.Error("operationName 不匹配")
	}
	encodedVariables, err := json.Marshal(body.Variables)
	if err != nil {
		t.Fatal(err)
	}
	var vars map[string]any
	if err := json.Unmarshal(encodedVariables, &vars); err != nil {
		t.Fatal(err)
	}
	if vars["region"] != "global" {
		t.Errorf("region=%v, want global", vars["region"])
	}
	if vars["recaptchaToken"] != "TOKEN123" {
		t.Errorf("recaptchaToken=%v", vars["recaptchaToken"])
	}
	if vars["model"] != "gemini-3.1-flash" {
		t.Errorf("model=%v", vars["model"])
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	requestContext, ok := decoded["requestContext"].(map[string]any)
	if !ok || requestContext["clientVersion"] == "" || requestContext["pagePath"] == "" ||
		requestContext["jurisdiction"] != "global" {
		t.Fatalf("requestContext shape changed: %#v", decoded["requestContext"])
	}
	localization, ok := requestContext["localizationData"].(map[string]any)
	if !ok || localization["locale"] != "zh_CN" || localization["timezone"] != "Asia/Hong_Kong" {
		t.Fatalf("localizationData shape changed: %#v", requestContext["localizationData"])
	}
	for _, key := range []string{"backendOverrides", "selectedPurview"} {
		if value, ok := requestContext[key].(map[string]any); !ok || len(value) != 0 {
			t.Fatalf("%s must remain an empty JSON object: %#v", key, requestContext[key])
		}
	}
}

func TestBuildRequestPayloadFromVariablesDoesNotMutatePrepared(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	payload := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}},
	}}
	prepared := buildRequestVariables("gemini-3.1-flash", payload, cfg)
	first := buildRequestPayloadFromVariables(prepared, "TOKEN-A")
	second := buildRequestPayloadFromVariables(prepared, "TOKEN-B")
	if _, exists := prepared["recaptchaToken"]; exists {
		t.Fatalf("prepared variables were mutated: %#v", prepared)
	}
	if first.Variables.RecaptchaToken != "TOKEN-A" || second.Variables.RecaptchaToken != "TOKEN-B" {
		t.Fatalf(
			"attempt tokens leaked: first=%v second=%v",
			first.Variables.RecaptchaToken,
			second.Variables.RecaptchaToken,
		)
	}
}

func TestBuildRequestVariablesWireShapeMatchesMap(t *testing.T) {
	prepared := map[string]any{
		"contents":          []any{map[string]any{"role": "user"}},
		"generationConfig":  map[string]any{"temperature": 0.5},
		"model":             "gemini-3.1-flash",
		"region":            "global",
		"safetySettings":    []any{},
		"systemInstruction": map[string]any{"parts": []any{}},
		"toolConfig":        map[string]any{},
		"tools":             []any{},
	}
	body := buildRequestPayloadFromVariables(prepared, "TOKEN")
	got, err := json.Marshal(body.Variables)
	if err != nil {
		t.Fatal(err)
	}
	wantVariables := shallowCopy(prepared)
	wantVariables["recaptchaToken"] = "TOKEN"
	want, err := json.Marshal(wantVariables)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("variables wire shape changed:\n got=%s\nwant=%s", got, want)
	}
}

func TestBuildRequestPayloadFromVariablesConcurrent(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	prepared := buildRequestVariables("gemini-3.1-flash", map[string]any{
		"contents": []any{map[string]any{
			"role": "user", "parts": []any{map[string]any{"text": "shared"}},
		}},
	}, cfg)

	const workers = 32
	var group sync.WaitGroup
	errors := make(chan error, workers)
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			body := buildRequestPayloadFromVariables(prepared, "TOKEN")
			if _, err := json.Marshal(body); err != nil {
				errors <- err
			}
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if _, exists := prepared["recaptchaToken"]; exists {
		t.Fatal("concurrent attempts mutated prepared variables")
	}
}

func TestBuildCompleteResponse_Empty(t *testing.T) {
	c := &VertexAIClient{}
	// 无 parts、无 error、无 promptFeedback → EmptyResponseError
	_, err := c.buildCompleteResponse(&ParseResult{PromptFeedback: map[string]any{}})
	if err == nil {
		t.Error("空响应应返回 EmptyResponseError")
	}
	if ve := asVertexError(err); ve == nil || ve.Kind != "empty" {
		t.Errorf("err=%v, want empty", err)
	}
}
