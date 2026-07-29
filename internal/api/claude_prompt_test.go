package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/transform"
)

func TestClaudePromptPolicyReplacesBeforeAppendingInjection(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplaceFrom = "ORIGINAL"
	cfg.ClaudePromptReplaceTo = "REPLACED"
	cfg.ClaudePromptInjectionEnabled = true
	cfg.ClaudePromptInjectionPosition = "append"
	cfg.ClaudePromptInjectionText = "INJECTED"
	provider := config.StaticProvider(cfg)

	chatBody, err := anthropicToChatRequest(map[string]any{
		"system": []any{
			map[string]any{"type": "text", "text": "Keep ORIGINAL"},
			map[string]any{"type": "text", "text": "ORIGINAL again"},
		},
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := applyClaudePromptPolicy(chatBody, provider)

	const original = "Keep ORIGINAL\nORIGINAL again"
	const effective = "Keep REPLACED\nREPLACED again\n\nINJECTED"
	if result.OriginalPrompt != original || result.EffectivePrompt != effective ||
		result.ReplacementCount != 2 || !result.InjectionApplied || !result.HadSystem {
		t.Fatalf("unexpected policy result: %#v", result)
	}

	chatBody["model"] = "gemini-3.6-flash"
	_, payload, err := transform.ConvertChatRequest(chatBody, provider)
	if err != nil {
		t.Fatal(err)
	}
	system, _ := payload["systemInstruction"].(map[string]any)
	parts, _ := system["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != effective {
		t.Fatalf("upstream system prompt=%#v, want %q", system, effective)
	}
}

func TestClaudePromptPolicySupportsPrependAndRemoval(t *testing.T) {
	t.Run("prepend without incoming system", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.ClaudePromptInjectionEnabled = true
		cfg.ClaudePromptInjectionPosition = "prepend"
		cfg.ClaudePromptInjectionText = "POLICY"
		chatBody := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		}}

		result := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))

		if result.OriginalPrompt != "" || result.EffectivePrompt != "POLICY" ||
			result.HadSystem || !result.InjectionApplied {
			t.Fatalf("unexpected prepend result: %#v", result)
		}
		messages := chatBody["messages"].([]any)
		if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
			t.Fatalf("injected system message missing: %#v", messages)
		}
	})

	t.Run("literal replacement can remove system", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.ClaudePromptReplacementEnabled = true
		cfg.ClaudePromptReplaceFrom = "REMOVE"
		chatBody := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "REMOVE"},
			map[string]any{"role": "user", "content": "hello"},
		}}

		result := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))

		if result.EffectivePrompt != "" || result.ReplacementCount != 1 {
			t.Fatalf("unexpected removal result: %#v", result)
		}
		messages := chatBody["messages"].([]any)
		if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
			t.Fatalf("empty rewritten system should be removed: %#v", messages)
		}
	})
}

func TestAnthropicConversionRecordsOriginalAndEffectiveClaudePrompts(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplaceFrom = "old"
	cfg.ClaudePromptReplaceTo = "new"
	store := &claudePromptStore{}
	handler := &AnthropicHandler{
		handler:       handler{cfg: config.StaticProvider(cfg)},
		reqConv:       transform.DefaultRequestConverter(),
		respConv:      transform.DefaultResponseConverter(),
		claudePrompts: store,
	}
	body := map[string]any{
		"model":  "gemini-3.6-flash",
		"system": "old policy",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}

	_, payload, err := handler.convertAnthropicRequest(
		body,
		"fake-gemini-3.6-flash",
		"gemini-3.6-flash",
		"messages",
	)
	if err != nil {
		t.Fatal(err)
	}
	system := payload["systemInstruction"].(map[string]any)
	parts := system["parts"].([]any)
	if parts[0].(map[string]any)["text"] != "new policy" {
		t.Fatalf("effective upstream prompt was not replaced: %#v", system)
	}
	record, ok := store.Latest()
	if !ok || record.OriginalPrompt != "old policy" ||
		record.EffectivePrompt != "new policy" ||
		record.Model != "fake-gemini-3.6-flash" ||
		record.Endpoint != "messages" ||
		record.ReplacementCount != 1 {
		t.Fatalf("unexpected recent prompt record: %#v, available=%v", record, ok)
	}
}

func TestClaudePromptStoreBoundsUTF8Records(t *testing.T) {
	store := &claudePromptStore{}
	large := strings.Repeat("界", maxClaudePromptRecordBytes/3+10)
	store.Record("model", "messages", claudePromptPolicyResult{
		OriginalPrompt:  large,
		EffectivePrompt: large,
		HadSystem:       true,
	})

	record, ok := store.Latest()
	if !ok || !record.OriginalTruncated || !record.EffectiveTruncated {
		t.Fatalf("large prompt was not marked truncated: %#v", record)
	}
	if len(record.OriginalPrompt) > maxClaudePromptRecordBytes ||
		!utf8.ValidString(record.OriginalPrompt) ||
		record.OriginalBytes != len(large) {
		t.Fatalf("bounded prompt is invalid: bytes=%d original=%d valid=%v",
			len(record.OriginalPrompt), record.OriginalBytes, utf8.ValidString(record.OriginalPrompt))
	}
}

func TestAdminClaudePromptLatestLifecycle(t *testing.T) {
	store := &claudePromptStore{}
	adm := &AdminHandler{claudePrompts: store} //nolint:exhaustruct

	empty := httptest.NewRecorder()
	adm.adminClaudePromptLatest(
		empty,
		httptest.NewRequest(http.MethodGet, "/api/admin/claude-prompt/latest", nil),
	)
	if !strings.Contains(empty.Body.String(), `"available":false`) {
		t.Fatalf("empty latest response=%s", empty.Body.String())
	}
	if empty.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("latest prompt response must not be cached: %#v", empty.Header())
	}

	store.Record("claude-alias", "messages", claudePromptPolicyResult{
		OriginalPrompt:   "before",
		EffectivePrompt:  "after",
		HadSystem:        true,
		ReplacementCount: 1,
	})
	get := httptest.NewRecorder()
	adm.adminClaudePromptLatest(
		get,
		httptest.NewRequest(http.MethodGet, "/api/admin/claude-prompt/latest", nil),
	)
	var response map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["original_prompt"] != "before" || response["effective_prompt"] != "after" ||
		response["replacement_count"] != float64(1) {
		t.Fatalf("unexpected latest response: %#v", response)
	}

	cleared := httptest.NewRecorder()
	adm.adminClaudePromptLatest(
		cleared,
		httptest.NewRequest(http.MethodDelete, "/api/admin/claude-prompt/latest", nil),
	)
	if _, ok := store.Latest(); ok {
		t.Fatal("DELETE did not clear the recent Claude prompt")
	}
}

func TestClaudePromptLatestAdminRouteIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	fx := newTestServer(t)

	claudeResponse := postWithHeader(
		t,
		fx.server.URL+"/v1/messages",
		"x-api-key",
		"sk-test-key",
		map[string]any{
			"model":      "fake-gemini-3.6-flash",
			"max_tokens": 64,
			"system":     "record this exact Claude system prompt",
			"messages": []any{
				map[string]any{"role": "user", "content": "hello"},
			},
		},
	)
	if claudeResponse.StatusCode != http.StatusOK {
		_ = claudeResponse.Body.Close()
		t.Fatalf("Claude request status=%d, want 200", claudeResponse.StatusCode)
	}
	_ = claudeResponse.Body.Close()

	loginResponse := doPost(
		t,
		fx.server.URL+"/api/admin/login",
		"",
		map[string]any{"password": "test-admin-pw"},
	)
	if loginResponse.StatusCode != http.StatusOK {
		_ = loginResponse.Body.Close()
		t.Fatalf("admin login status=%d, want 200", loginResponse.StatusCode)
	}
	var adminCookie *http.Cookie
	for _, cookie := range loginResponse.Cookies() {
		if cookie.Name == adminCookieName {
			adminCookie = cookie
			break
		}
	}
	_ = loginResponse.Body.Close()
	if adminCookie == nil {
		t.Fatal("admin login did not return the session cookie")
	}

	request, err := http.NewRequest(
		http.MethodGet,
		fx.server.URL+"/api/admin/claude-prompt/latest",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(adminCookie)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("latest prompt status=%d, want 200", response.StatusCode)
	}

	var latest map[string]any
	if err := json.NewDecoder(response.Body).Decode(&latest); err != nil {
		t.Fatal(err)
	}
	if latest["available"] != true ||
		latest["original_prompt"] != "record this exact Claude system prompt" ||
		latest["effective_prompt"] != "record this exact Claude system prompt" ||
		latest["model"] != "fake-gemini-3.6-flash" ||
		latest["endpoint"] != "messages" {
		t.Fatalf("unexpected integrated latest prompt response: %#v", latest)
	}
}
