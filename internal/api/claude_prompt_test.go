package api

import (
	"encoding/json"
	"fmt"
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
	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{
		{From: "ORIGINAL", To: "REPLACED"},
		{From: "SECOND", To: "CHANGED"},
	}
	cfg.ClaudePromptInjectionEnabled = true
	cfg.ClaudePromptInjectionPosition = "append"
	cfg.ClaudePromptInjectionText = "INJECTED"
	provider := config.StaticProvider(cfg)

	chatBody, err := anthropicToChatRequest(map[string]any{
		"system": []any{
			map[string]any{"type": "text", "text": "Keep ORIGINAL"},
			map[string]any{"type": "text", "text": "SECOND and ORIGINAL again"},
		},
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := applyClaudePromptPolicy(chatBody, provider)
	if err != nil {
		t.Fatal(err)
	}

	const original = "Keep ORIGINAL\nSECOND and ORIGINAL again"
	const effective = "Keep REPLACED\nCHANGED and REPLACED again\n\nINJECTED"
	if result.OriginalPrompt != original || result.EffectivePrompt != effective ||
		result.ReplacementCount != 3 || result.ReplacementRules != 2 ||
		result.MatchedRules != 2 || len(result.RuleMatchCounts) != 2 ||
		result.RuleMatchCounts[0] != 2 || result.RuleMatchCounts[1] != 1 ||
		!result.InjectionApplied || !result.HadSystem {
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

		result, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
		if err != nil {
			t.Fatal(err)
		}

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
		cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{{From: "REMOVE"}}
		chatBody := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "REMOVE"},
			map[string]any{"role": "user", "content": "hello"},
		}}

		result, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
		if err != nil {
			t.Fatal(err)
		}

		if result.EffectivePrompt != "" || result.ReplacementCount != 1 {
			t.Fatalf("unexpected removal result: %#v", result)
		}
		messages := chatBody["messages"].([]any)
		if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
			t.Fatalf("empty rewritten system should be removed: %#v", messages)
		}
	})
}

func TestClaudePromptPolicyAppliesRulesInOrderAndBoundsGrowth(t *testing.T) {
	t.Run("ordered rules can consume earlier output", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.ClaudePromptReplacementEnabled = true
		cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{
			{From: "alpha", To: "beta"},
			{From: "beta", To: "gamma"},
		}
		chatBody := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "alpha"},
			map[string]any{"role": "user", "content": "hello"},
		}}

		result, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
		if err != nil {
			t.Fatal(err)
		}
		if result.EffectivePrompt != "gamma" || result.ReplacementCount != 2 ||
			result.RuleMatchCounts[0] != 1 || result.RuleMatchCounts[1] != 1 {
			t.Fatalf("ordered replacement result: %#v", result)
		}
	})

	t.Run("replacement expansion cannot exceed request limit", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.MaxRequestMB = 1
		cfg.ClaudePromptReplacementEnabled = true
		cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{{
			From: "a",
			To:   "aa",
		}}
		chatBody := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": strings.Repeat("a", 600<<10)},
			map[string]any{"role": "user", "content": "hello"},
		}}

		_, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("expected bounded replacement error, got %v", err)
		}
	})

	t.Run("injection cannot exceed request limit", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.MaxRequestMB = 1
		cfg.ClaudePromptInjectionEnabled = true
		cfg.ClaudePromptInjectionText = strings.Repeat("i", 600<<10)
		chatBody := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": strings.Repeat("s", 600<<10)},
			map[string]any{"role": "user", "content": "hello"},
		}}

		_, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
		if err == nil || !strings.Contains(err.Error(), "after injection") {
			t.Fatalf("expected bounded injection error, got %v", err)
		}
	})

	t.Run("manually configured rule overflow is rejected at runtime", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.ClaudePromptReplacementEnabled = true
		cfg.ClaudePromptReplacements = make(
			[]config.ClaudePromptReplacementRule,
			maxClaudePromptReplacementRules+1,
		)
		for index := range cfg.ClaudePromptReplacements {
			cfg.ClaudePromptReplacements[index].From = fmt.Sprintf("rule-%d", index)
		}
		chatBody := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "system"},
			map[string]any{"role": "user", "content": "hello"},
		}}

		_, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
		if err == nil || !strings.Contains(err.Error(), "exceed the limit") {
			t.Fatalf("expected runtime rule-limit error, got %v", err)
		}
	})
}

func TestAnthropicConversionRecordsOriginalAndEffectiveClaudePrompts(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{{
		From: "old",
		To:   "new",
	}}
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

func TestClaudePromptStoreDoesNotExposeRuleMatchSlice(t *testing.T) {
	store := &claudePromptStore{}
	store.Record("model", "messages", claudePromptPolicyResult{
		RuleMatchCounts: []int{2, 1},
	})
	record, ok := store.Latest()
	if !ok {
		t.Fatal("expected latest record")
	}
	record.RuleMatchCounts[0] = 99
	unchanged, _ := store.Latest()
	if unchanged.RuleMatchCounts[0] != 2 {
		t.Fatalf("stored match counts were mutated: %#v", unchanged.RuleMatchCounts)
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
		ReplacementRules: 1,
		MatchedRules:     1,
		RuleMatchCounts:  []int{1},
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
		response["replacement_count"] != float64(1) ||
		response["replacement_rules"] != float64(1) ||
		response["matched_rules"] != float64(1) {
		t.Fatalf("unexpected latest response: %#v", response)
	}
	matchCounts, _ := response["rule_match_counts"].([]any)
	if len(matchCounts) != 1 || matchCounts[0] != float64(1) {
		t.Fatalf("latest response lost per-rule match counts: %#v", response)
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
