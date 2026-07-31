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

func TestClaudePromptPolicyStripsClaudeCodePromotionsByDefault(t *testing.T) {
	cfg := config.DefaultConfig()
	chatBody := map[string]any{"messages": []any{
		map[string]any{
			"role":    "system",
			"content": "environment before\n" + claudeCodePromotionPrompt + "\ncontext after",
		},
		map[string]any{"role": "user", "content": "hello"},
	}}

	result, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if result.PromotionRemovalCount != 1 ||
		result.ReplacementCount != 0 ||
		strings.Contains(result.EffectivePrompt, "Claude Fable 5") ||
		result.EffectivePrompt != "environment before\n\ncontext after" {
		t.Fatalf("unexpected default promotion removal: %#v", result)
	}
	messages := chatBody["messages"].([]any)
	if got := messages[0].(map[string]any)["content"]; got != result.EffectivePrompt {
		t.Fatalf("upstream system=%q, want %q", got, result.EffectivePrompt)
	}

	crlfPrompt := strings.ReplaceAll(claudeCodePromotionPrompt, "\n", "\r\n")
	crlfBody := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": crlfPrompt},
		map[string]any{"role": "user", "content": "hello"},
	}}
	result, err = applyClaudePromptPolicy(crlfBody, config.StaticProvider(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if result.PromotionRemovalCount != 1 || result.EffectivePrompt != "" {
		t.Fatalf("CRLF promotion block was not removed: %#v", result)
	}

	cfg.ClaudePromptStripPromotions = false
	unchangedBody := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": claudeCodePromotionPrompt},
		map[string]any{"role": "user", "content": "hello"},
	}}
	result, err = applyClaudePromptPolicy(unchangedBody, config.StaticProvider(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if result.PromotionRemovalCount != 0 || result.EffectivePrompt != claudeCodePromotionPrompt {
		t.Fatalf("promotion-removal opt-out was ignored: %#v", result)
	}
}

func TestClaudePromptPolicyDoesNotPartiallyStripChangedPromotionBlock(t *testing.T) {
	changed := strings.Replace(
		claudeCodePromotionPrompt,
		"available on Opus 5/4.8/4.7",
		"available on another model",
		1,
	)
	chatBody := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": changed},
		map[string]any{"role": "user", "content": "hello"},
	}}
	result, err := applyClaudePromptPolicy(
		chatBody,
		config.StaticProvider(config.DefaultConfig()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.PromotionRemovalCount != 0 || result.EffectivePrompt != changed {
		t.Fatalf("changed prompt was partially stripped: %#v", result)
	}
}

func TestClaudePromptPolicyReplacesSecurityPreambleByDefault(t *testing.T) {
	cfg := config.DefaultConfig()
	chatBody := map[string]any{"messages": []any{
		map[string]any{
			"role":    "system",
			"content": "before\n" + claudeSecurityPreamblePrompt + "\nafter",
		},
		map[string]any{"role": "user", "content": claudeSecurityPreamblePrompt},
	}}

	result, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if result.SecurityPreambleReplacementCount != 1 ||
		result.ReplacementCount != 0 ||
		!strings.Contains(result.EffectivePrompt, claudeSecurityPreambleReplacement) ||
		strings.Contains(result.EffectivePrompt, claudeSecurityPreamblePrompt) {
		t.Fatalf("unexpected default security preamble replacement: %#v", result)
	}
	messages := chatBody["messages"].([]any)
	if got := messages[0].(map[string]any)["content"].(string); got != result.EffectivePrompt {
		t.Fatalf("upstream system=%q, want %q", got, result.EffectivePrompt)
	}
	if got := messages[1].(map[string]any)["content"].(string); got != claudeSecurityPreamblePrompt {
		t.Fatalf("user content was unexpectedly rewritten: %q", got)
	}

	cfg.ClaudePromptReplaceSecurity = false
	unchangedBody := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": claudeSecurityPreamblePrompt},
		map[string]any{"role": "user", "content": "hello"},
	}}
	result, err = applyClaudePromptPolicy(unchangedBody, config.StaticProvider(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if result.SecurityPreambleReplacementCount != 0 ||
		result.EffectivePrompt != claudeSecurityPreamblePrompt {
		t.Fatalf("security-preamble opt-out was ignored: %#v", result)
	}
}

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
	if len(parts) != 2 ||
		parts[0].(map[string]any)["text"] != "Keep REPLACED\nCHANGED and REPLACED again" ||
		parts[1].(map[string]any)["text"] != "INJECTED" {
		t.Fatalf("upstream system prompt did not preserve replacement/injection parts: %#v", system)
	}
}

func TestClaudePromptPolicyPreservesMidConversationSystemOrder(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{
		{From: "TOP_OLD", To: "TOP_NEW"},
		{From: "MID_OLD", To: "MID_NEW"},
	}
	cfg.ClaudePromptInjectionEnabled = true
	cfg.ClaudePromptInjectionPosition = "prepend"
	cfg.ClaudePromptInjectionText = "INJECTED"
	provider := config.StaticProvider(cfg)
	chatBody, err := anthropicToChatRequest(map[string]any{
		"system": []any{
			map[string]any{"type": "text", "text": "TOP_OLD"},
			map[string]any{"type": "text", "text": "TOP_SECOND"},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "first user"},
			map[string]any{"role": "system", "content": "MID_OLD"},
			map[string]any{"role": "assistant", "content": "prior answer"},
			map[string]any{"role": "user", "content": "next user"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	chatBody["model"] = "gemini-3.6-flash"

	result, err := applyClaudePromptPolicy(chatBody, provider)
	if err != nil {
		t.Fatal(err)
	}
	if result.OriginalPrompt != "TOP_OLD\nTOP_SECOND\n\nMID_OLD" ||
		result.EffectivePrompt != "INJECTED\n\nTOP_NEW\nTOP_SECOND\n\nMID_NEW" ||
		result.ReplacementCount != 2 || !result.InjectionApplied {
		t.Fatalf("unexpected segmented policy result: %#v", result)
	}

	messages := chatBody["messages"].([]any)
	wantRoles := []string{"system", "system", "user", "system", "assistant", "user"}
	if len(messages) != len(wantRoles) {
		t.Fatalf("rewritten messages=%#v", messages)
	}
	for index, wantRole := range wantRoles {
		if got := messages[index].(map[string]any)["role"]; got != wantRole {
			t.Fatalf("messages[%d].role=%v, want %s", index, got, wantRole)
		}
	}
	if messages[0].(map[string]any)["content"] != "INJECTED" ||
		messages[1].(map[string]any)["content"] != "TOP_NEW\nTOP_SECOND" ||
		messages[3].(map[string]any)["content"] != "MID_NEW" {
		t.Fatalf("system segments were reordered or collapsed: %#v", messages)
	}

	_, payload, err := transform.ConvertChatRequest(chatBody, provider)
	if err != nil {
		t.Fatal(err)
	}
	system := payload["systemInstruction"].(map[string]any)
	parts := system["parts"].([]any)
	wantParts := []string{"INJECTED", "TOP_NEW\nTOP_SECOND", "MID_NEW"}
	if len(parts) != len(wantParts) {
		t.Fatalf("systemInstruction parts=%#v", parts)
	}
	for index, want := range wantParts {
		if got := parts[index].(map[string]any)["text"]; got != want {
			t.Fatalf("systemInstruction.parts[%d]=%v, want %q", index, got, want)
		}
	}
}

func TestClaudePromptPolicySegmentedRemovalPreservesRemainingOrder(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{{
		From: "REMOVE",
	}}
	chatBody := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "REMOVE"},
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "system", "content": "KEEP REMOVE"},
		map[string]any{"role": "user", "content": "second"},
	}}

	result, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectivePrompt != "KEEP " || result.ReplacementCount != 2 {
		t.Fatalf("unexpected segmented removal result: %#v", result)
	}
	messages := chatBody["messages"].([]any)
	if len(messages) != 3 ||
		messages[0].(map[string]any)["content"] != "first" ||
		messages[1].(map[string]any)["role"] != "system" ||
		messages[1].(map[string]any)["content"] != "KEEP " ||
		messages[2].(map[string]any)["content"] != "second" {
		t.Fatalf("segmented removal reordered messages: %#v", messages)
	}
}

func TestClaudePromptPolicySegmentedExpansionEnforcesCombinedLimit(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MaxRequestMB = 1
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{{
		From: "a",
		To:   "aa",
	}}
	segment := strings.Repeat("a", 300<<10)
	chatBody := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": segment},
		map[string]any{"role": "user", "content": "continue"},
		map[string]any{"role": "system", "content": segment},
	}}

	_, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected combined segmented output limit error, got %v", err)
	}
}

func TestClaudePromptPolicyFallsBackForCrossSegmentReplacement(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{{
		From: "TOP\n\nMID",
		To:   "COMBINED",
	}}
	chatBody := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "TOP"},
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "system", "content": "MID"},
	}}

	result, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectivePrompt != "COMBINED" || result.ReplacementCount != 1 {
		t.Fatalf("unexpected cross-segment result: %#v", result)
	}
	messages := chatBody["messages"].([]any)
	if len(messages) != 2 ||
		messages[0].(map[string]any)["role"] != "system" ||
		messages[0].(map[string]any)["content"] != "COMBINED" ||
		messages[1].(map[string]any)["role"] != "user" {
		t.Fatalf("cross-segment replacement should collapse at the first system position: %#v", messages)
	}
}

func TestClaudePromptPolicyDoesNotRewriteUserSystemReminder(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{{
		From: "private instruction",
		To:   "rewritten",
	}}
	const reminder = "<system-reminder>private instruction</system-reminder>"
	chatBody := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "top policy"},
		map[string]any{"role": "user", "content": reminder},
	}}

	result, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if result.ReplacementCount != 0 || result.EffectivePrompt != "top policy" {
		t.Fatalf("user reminder unexpectedly affected policy result: %#v", result)
	}
	messages := chatBody["messages"].([]any)
	if messages[1].(map[string]any)["content"] != reminder {
		t.Fatalf("user system-reminder was unexpectedly rewritten: %#v", messages)
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
		RuleApplicable:  []bool{true, false},
	})
	record, ok := store.Latest()
	if !ok {
		t.Fatal("expected latest record")
	}
	record.RuleMatchCounts[0] = 99
	record.RuleApplicable[0] = false
	unchanged, _ := store.Latest()
	if unchanged.RuleMatchCounts[0] != 2 || !unchanged.RuleApplicable[0] {
		t.Fatalf("stored rule metadata was mutated: counts=%#v applicable=%#v",
			unchanged.RuleMatchCounts, unchanged.RuleApplicable)
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
		OriginalPrompt:                   "before",
		EffectivePrompt:                  "after",
		HadSystem:                        true,
		PromotionRemovalCount:            1,
		SecurityPreambleReplacementCount: 1,
		ReplacementCount:                 1,
		ReplacementRules:                 1,
		MatchedRules:                     1,
		RuleMatchCounts:                  []int{1},
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
		response["promotion_removal_count"] != float64(1) ||
		response["security_preamble_replacement_count"] != float64(1) ||
		response["replacement_count"] != float64(1) ||
		response["replacement_rules"] != float64(1) ||
		response["matched_rules"] != float64(1) {
		t.Fatalf("unexpected latest response: %#v", response)
	}
	matchCounts, _ := response["rule_match_counts"].([]any)
	if len(matchCounts) != 1 || matchCounts[0] != float64(1) {
		t.Fatalf("latest response lost per-rule match counts: %#v", response)
	}
	store.Record("count-model", "count_tokens", claudePromptPolicyResult{OriginalPrompt: "count prompt"})
	countGet := httptest.NewRecorder()
	adm.adminClaudePromptLatest(
		countGet,
		httptest.NewRequest(
			http.MethodGet,
			"/api/admin/claude-prompt/latest?endpoint=count_tokens",
			nil,
		),
	)
	if !strings.Contains(countGet.Body.String(), `"original_prompt":"count prompt"`) {
		t.Fatalf("count_tokens latest response=%s", countGet.Body.String())
	}

	cleared := httptest.NewRecorder()
	adm.adminClaudePromptLatest(
		cleared,
		httptest.NewRequest(http.MethodDelete, "/api/admin/claude-prompt/latest", nil),
	)
	if _, ok := store.Latest(); ok {
		t.Fatal("DELETE did not clear the recent Claude prompt")
	}
	if _, ok := store.Latest("count_tokens"); !ok {
		t.Fatal("clearing messages unexpectedly cleared count_tokens")
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

func TestClaudePromptPolicyHonorsDisabledAndModelScopedRules(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{
		{From: "skip", To: "wrong", Disabled: true},
		{From: "target", To: "changed", Models: []string{"fake-gemini-3.6-flash"}},
		{From: "other", To: "wrong", Models: []string{"another-model"}},
	}
	chatBody := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "skip target other"},
		map[string]any{"role": "user", "content": "hello"},
	}}

	result, err := applyClaudePromptPolicy(
		chatBody,
		config.StaticProvider(cfg),
		"fake-gemini-3.6-flash",
		"gemini-3.6-flash",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectivePrompt != "skip changed other" || result.ReplacementRules != 3 ||
		result.ApplicableRules != 1 || result.MatchedRules != 1 ||
		len(result.RuleApplicable) != 3 || result.RuleApplicable[0] ||
		!result.RuleApplicable[1] || result.RuleApplicable[2] ||
		result.RuleMatchCounts[1] != 1 {
		t.Fatalf("unexpected scoped replacement result: %#v", result)
	}

	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{{
		From: "target", To: "actual", Models: []string{"gemini-3.6-flash"},
	}}
	chatBody = map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "target"},
	}}
	result, err = applyClaudePromptPolicy(
		chatBody,
		config.StaticProvider(cfg),
		"my-alias",
		"gemini-3.6-flash",
	)
	if err != nil || result.EffectivePrompt != "actual" {
		t.Fatalf("actual model scope was not honored: result=%#v err=%v", result, err)
	}
}

func TestReplaceAllClaudeLiteralWithinLimit(t *testing.T) {
	for _, test := range []struct {
		value string
		from  string
		to    string
		want  string
		count int
	}{
		{value: "no match", from: "x", to: "y", want: "no match", count: 0},
		{value: "aaaa", from: "aa", to: "b", want: "bb", count: 2},
		{value: "a-b-a", from: "a", to: "long", want: "long-b-long", count: 2},
		{value: "remove-me", from: "remove-", to: "", want: "me", count: 1},
	} {
		got, count, err := replaceAllClaudeLiteralWithinLimit(test.value, test.from, test.to, 1024)
		if err != nil || got != test.want || count != test.count {
			t.Fatalf("replace %q: got=%q count=%d err=%v", test.value, got, count, err)
		}
	}
	if _, _, err := replaceAllClaudeLiteralWithinLimit("aaaa", "a", "long", 8); err == nil {
		t.Fatal("expected output limit error")
	}
}

func TestClaudeReplacementWorkBudget(t *testing.T) {
	work, ok := addClaudeReplacementWork(maxClaudeReplacementWorkBytes-1, 1)
	if !ok || work != maxClaudeReplacementWorkBytes {
		t.Fatalf("exact work budget should be accepted: work=%d ok=%v", work, ok)
	}
	if _, ok := addClaudeReplacementWork(work, 1); ok {
		t.Fatal("work above the replacement budget should be rejected")
	}
}

func TestClaudePromptPolicyEnforcesIndependentSystemLimit(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MaxRequestMB = 64
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{{From: "missing", To: "value"}}
	chatBody := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": strings.Repeat("x", maxClaudeProcessedPromptBytes+1)},
	}}

	_, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
	policyErr, ok := err.(*claudePromptPolicyError)
	if !ok || policyErr.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected typed 413 prompt limit error, got %T %v", err, err)
	}
}

func TestClaudePromptPolicyRejectsInvalidManualInjectionConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptInjectionEnabled = true
	cfg.ClaudePromptInjectionText = "   "
	chatBody := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "system"},
	}}

	_, err := applyClaudePromptPolicy(chatBody, config.StaticProvider(cfg))
	policyErr, ok := err.(*claudePromptPolicyError)
	if !ok || policyErr.status != http.StatusInternalServerError ||
		!strings.Contains(policyErr.Error(), "without content") {
		t.Fatalf("expected typed invalid config error, got %T %v", err, err)
	}
}

func TestClaudePromptStoreSeparatesMessagesAndCountTokens(t *testing.T) {
	store := &claudePromptStore{}
	store.Record("generate-model", "messages", claudePromptPolicyResult{OriginalPrompt: "generate"})
	store.Record("count-model", "count_tokens", claudePromptPolicyResult{OriginalPrompt: "count"})

	generate, generateOK := store.Latest("messages")
	count, countOK := store.Latest("count_tokens")
	if !generateOK || !countOK || generate.OriginalPrompt != "generate" || count.OriginalPrompt != "count" {
		t.Fatalf("endpoint records were mixed: messages=%#v count=%#v", generate, count)
	}
	store.Clear("count_tokens")
	if _, ok := store.Latest("count_tokens"); ok {
		t.Fatal("count_tokens record was not cleared")
	}
	if _, ok := store.Latest("messages"); !ok {
		t.Fatal("clearing count_tokens also cleared messages")
	}
}

func TestAnthropicPromptPolicyErrorsUseTypedHTTPStatus(t *testing.T) {
	handler := &AnthropicHandler{}
	serverError := httptest.NewRecorder()
	handler.writeAnthropicConversionError(serverError, claudePromptConfigError(fmt.Errorf("secret detail")))
	if serverError.Code != http.StatusInternalServerError ||
		strings.Contains(serverError.Body.String(), "secret detail") ||
		!strings.Contains(serverError.Body.String(), "configuration is invalid") {
		t.Fatalf("unexpected config error response: status=%d body=%s", serverError.Code, serverError.Body.String())
	}

	tooLarge := httptest.NewRecorder()
	handler.writeAnthropicConversionError(tooLarge, claudePromptLimitError("internal limit detail"))
	if tooLarge.Code != http.StatusRequestEntityTooLarge ||
		!strings.Contains(tooLarge.Body.String(), "processing limit") {
		t.Fatalf("unexpected limit response: status=%d body=%s", tooLarge.Code, tooLarge.Body.String())
	}
}

func TestAdminClaudePromptPreviewUsesUnsavedPolicy(t *testing.T) {
	adm := &AdminHandler{handler: handler{cfg: config.StaticProvider(config.DefaultConfig())}} //nolint:exhaustruct
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/admin/claude-prompt/preview",
		strings.NewReader(`{
			"original_prompt":"old policy",
			"model":"fake-gemini-3.6-flash",
			"replacement_enabled":true,
			"replacements":[{"from":"old","to":"new","models":["fake-gemini-3.6-flash"]}],
			"injection_enabled":true,
			"injection_position":"append",
			"injection_text":"injected"
		}`),
	)

	adm.adminClaudePromptPreview(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["effective_prompt"] != "new policy\n\ninjected" ||
		response["replacement_count"] != float64(1) ||
		response["applicable_rules"] != float64(1) {
		t.Fatalf("unexpected preview response: %#v", response)
	}
}

func TestAdminClaudePromptPreviewReportsDefaultPromotionRemoval(t *testing.T) {
	adm := &AdminHandler{
		handler: handler{cfg: config.StaticProvider(config.DefaultConfig())},
	} //nolint:exhaustruct
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/admin/claude-prompt/preview",
		strings.NewReader(fmt.Sprintf(`{
			"original_prompt":%q,
			"strip_claude_code_promotions":true,
			"replacement_enabled":false,
			"replacements":[],
			"injection_enabled":false,
			"injection_position":"append",
			"injection_text":""
		}`, claudeCodePromotionPrompt)),
	)

	adm.adminClaudePromptPreview(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["effective_prompt"] != "" ||
		response["promotion_removal_count"] != float64(1) ||
		response["security_preamble_replacement_count"] != float64(0) ||
		response["replacement_count"] != float64(0) {
		t.Fatalf("unexpected promotion-removal preview: %#v", response)
	}
}

func TestAdminClaudePromptPreviewReportsDefaultSecurityPreambleReplacement(t *testing.T) {
	adm := &AdminHandler{
		handler: handler{cfg: config.StaticProvider(config.DefaultConfig())},
	} //nolint:exhaustruct
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/admin/claude-prompt/preview",
		strings.NewReader(fmt.Sprintf(`{
			"original_prompt":%q,
			"replace_security_preamble":true,
			"replacement_enabled":false,
			"replacements":[],
			"injection_enabled":false,
			"injection_position":"append",
			"injection_text":""
		}`, claudeSecurityPreamblePrompt)),
	)

	adm.adminClaudePromptPreview(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["effective_prompt"] != claudeSecurityPreambleReplacement ||
		response["security_preamble_replacement_count"] != float64(1) ||
		response["replacement_count"] != float64(0) {
		t.Fatalf("unexpected security-preamble preview: %#v", response)
	}
}

func BenchmarkClaudePromptPolicySegmentedLiteralRules(b *testing.B) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{
		{From: "TOKEN_0", To: "VALUE_0"},
		{From: "TOKEN_1", To: "VALUE_1"},
		{From: "TOKEN_2", To: "VALUE_2"},
		{From: "TOKEN_3", To: "VALUE_3"},
	}
	provider := config.StaticProvider(cfg)
	messages := make([]any, 0, 9)
	for index := range 4 {
		segment := strings.Repeat("system context ", 1024) +
			fmt.Sprintf(" TOKEN_%d TOKEN_%d TOKEN_%d TOKEN_%d", index, index, index, index)
		messages = append(messages, map[string]any{
			"role": "system", "content": segment,
		})
		messages = append(messages, map[string]any{
			"role": "user", "content": "continue",
		})
	}
	chatBody := map[string]any{"messages": messages}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		chatBody["messages"] = messages
		result, err := applyClaudePromptPolicy(chatBody, provider)
		if err != nil {
			b.Fatal(err)
		}
		if result.ReplacementCount != 16 {
			b.Fatalf("replacement count=%d, want 16", result.ReplacementCount)
		}
	}
}

func BenchmarkClaudePromptPolicyDefaultLargeSystem(b *testing.B) {
	cfg := config.StaticProvider(config.DefaultConfig())
	for _, segments := range []int{1, 4} {
		b.Run(fmt.Sprintf("segments_%d", segments), func(b *testing.B) {
			messages := make([]any, 0, segments*2)
			for range segments {
				messages = append(messages, map[string]any{
					"role": "system",
					"content": strings.Repeat(
						"ordinary Claude Code system context ",
						512,
					),
				})
				messages = append(messages, map[string]any{
					"role": "user", "content": "continue",
				})
			}
			chatBody := map[string]any{"messages": messages}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				chatBody["messages"] = messages
				result, err := applyClaudePromptPolicy(chatBody, cfg)
				if err != nil {
					b.Fatal(err)
				}
				if result.EffectivePrompt == "" {
					b.Fatal("effective prompt is empty")
				}
			}
		})
	}
}

func BenchmarkClaudePromptPolicyRuleMetadata(b *testing.B) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = benchmarkClaudePromptReplacementRules()
	provider := config.StaticProvider(cfg)
	messages := []any{
		map[string]any{"role": "system", "content": "ordinary system prompt"},
		map[string]any{"role": "user", "content": "continue"},
	}
	chatBody := map[string]any{"messages": messages}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		chatBody["messages"] = messages
		result, err := applyClaudePromptPolicy(
			chatBody,
			provider,
			"unmatched-model",
			"unmatched-model",
		)
		if err != nil {
			b.Fatal(err)
		}
		if result.ApplicableRules != 0 {
			b.Fatalf("applicable rules=%d, want 0", result.ApplicableRules)
		}
	}
}

func BenchmarkValidateClaudePromptPolicyConfig(b *testing.B) {
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = benchmarkClaudePromptReplacementRules()
	policy := cfg.ClaudePromptPolicy()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := validateClaudePromptPolicyConfig(policy); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkClaudePromptReplacementRules() []config.ClaudePromptReplacementRule {
	rules := make(
		[]config.ClaudePromptReplacementRule,
		maxClaudePromptReplacementRules,
	)
	for ruleIndex := range rules {
		rules[ruleIndex].From = fmt.Sprintf("source-%02d", ruleIndex)
		rules[ruleIndex].To = fmt.Sprintf("target-%02d", ruleIndex)
		rules[ruleIndex].Models = make([]string, 8)
		for modelIndex := range rules[ruleIndex].Models {
			rules[ruleIndex].Models[modelIndex] = fmt.Sprintf(
				"model-%02d-%02d",
				ruleIndex,
				modelIndex,
			)
		}
	}
	return rules
}
