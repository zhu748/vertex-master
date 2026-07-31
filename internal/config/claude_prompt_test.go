package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudePromptConfigDefaultsAndProvider(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ClaudePromptInjectionEnabled || cfg.ClaudePromptReplacementEnabled {
		t.Fatal("custom Claude prompt rewriting must be opt-in")
	}
	if !cfg.ClaudePromptStripPromotions {
		t.Fatal("Claude Code promotion removal must be enabled by default")
	}
	if !cfg.ClaudePromptReplaceSecurity {
		t.Fatal("Claude security preamble replacement must be enabled by default")
	}
	if cfg.ClaudePromptInjectionPosition != "append" {
		t.Fatalf("default injection position=%q, want append", cfg.ClaudePromptInjectionPosition)
	}

	cfg.ClaudePromptInjectionEnabled = true
	cfg.ClaudePromptInjectionPosition = "prepend"
	cfg.ClaudePromptInjectionText = "inject"
	cfg.ClaudePromptStripPromotions = false
	cfg.ClaudePromptReplaceSecurity = false
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []ClaudePromptReplacementRule{
		{From: "from one", To: "to one", Models: []string{"fake-model"}},
		{From: "from two", To: "to two", Disabled: true},
	}
	provider := StaticProvider(cfg)
	rules := provider.ClaudePromptReplacementRules()
	if !provider.ClaudePromptInjectionEnabled() ||
		provider.ClaudePromptInjectionPosition() != "prepend" ||
		provider.ClaudePromptInjectionText() != "inject" ||
		!provider.ClaudePromptReplacementEnabled() ||
		len(rules) != 2 || rules[0].From != "from one" || rules[1].To != "to two" {
		t.Fatalf("static provider lost Claude prompt settings")
	}
	rules[0].From = "mutated"
	rules[0].Models[0] = "mutated-model"
	if current := provider.ClaudePromptReplacementRules(); current[0].From != "from one" ||
		current[0].Models[0] != "fake-model" {
		t.Fatal("provider exposed its replacement rule slice for mutation")
	}
	policy := provider.ClaudePromptPolicy()
	if !policy.InjectionEnabled || policy.StripPromotions || policy.ReplaceSecurity ||
		!policy.ReplacementEnabled ||
		policy.InjectionPosition != "prepend" || len(policy.ReplacementRules) != 2 ||
		!policy.ReplacementRules[1].Disabled || policy.MaxRequestMB != cfg.MaxRequestMB {
		t.Fatalf("static provider lost the Claude prompt policy snapshot: %#v", policy)
	}
	policy.ReplacementRules[0].Models[0] = "mutated-policy"
	if provider.ClaudePromptPolicy().ReplacementRules[0].Models[0] != "fake-model" {
		t.Fatal("policy snapshot exposed nested model filters for mutation")
	}

	cfg.ClaudePromptInjectionPosition = "invalid"
	if got := StaticProvider(cfg).ClaudePromptInjectionPosition(); got != "append" {
		t.Fatalf("invalid injection position=%q, want append fallback", got)
	}
}

func TestLegacyClaudePromptReplacementRuleRemainsCompatible(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ClaudePromptReplaceFrom = "legacy from"
	cfg.ClaudePromptReplaceTo = "legacy to"
	rules := StaticProvider(cfg).ClaudePromptReplacementRules()
	if len(rules) != 1 || rules[0].From != "legacy from" || rules[0].To != "legacy to" {
		t.Fatalf("legacy Claude replacement was not preserved: %#v", rules)
	}

	cfg.ClaudePromptReplacements = []ClaudePromptReplacementRule{}
	if rules := StaticProvider(cfg).ClaudePromptReplacementRules(); len(rules) != 0 {
		t.Fatalf("explicit empty multi-rule config must override legacy fields: %#v", rules)
	}
}

func TestClaudePromptPolicySnapshotValidationFollowsHotReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	t.Cleanup(InvalidateCache)
	provider := GetProvider()

	writeConfig := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		InvalidateCache()
	}

	writeConfig(`{
		"claude_prompt_replacement_enabled":true,
		"claude_prompt_replacements":[
			{"from":"source","to":"target","models":["fake-model"]}
		]
	}`)
	first := provider.ClaudePromptPolicySnapshot()
	second := provider.ClaudePromptPolicySnapshot()
	if err := first.ValidationError(); err != nil {
		t.Fatalf("valid policy snapshot failed validation: %v", err)
	}
	if len(first.ReplacementRules) != 1 ||
		&first.ReplacementRules[0] != &second.ReplacementRules[0] {
		t.Fatal("unchanged policy snapshots did not reuse immutable rule storage")
	}
	detached := provider.ClaudePromptPolicy()
	detached.ReplacementRules[0].From = "mutated"
	detached.ReplacementRules[0].Models[0] = "mutated-model"
	current := provider.ClaudePromptPolicySnapshot()
	if current.ReplacementRules[0].From != "source" ||
		current.ReplacementRules[0].Models[0] != "fake-model" {
		t.Fatal("detached policy copy mutated the cached snapshot")
	}

	writeConfig(`{
		"claude_prompt_replacement_enabled":true,
		"claude_prompt_replacements":[
			{"from":"duplicate","to":"one"},
			{"from":"duplicate","to":"two"}
		]
	}`)
	invalid := provider.ClaudePromptPolicySnapshot()
	if err := invalid.ValidationError(); err == nil ||
		!strings.Contains(err.Error(), "duplicate source") {
		t.Fatalf("invalid reloaded policy validation error=%v", err)
	}

	writeConfig(`{
		"claude_prompt_replacement_enabled":true,
		"claude_prompt_replacements":[
			{"from":"fixed","to":"value"}
		]
	}`)
	recovered := provider.ClaudePromptPolicySnapshot()
	if err := recovered.ValidationError(); err != nil ||
		len(recovered.ReplacementRules) != 1 ||
		recovered.ReplacementRules[0].From != "fixed" {
		t.Fatalf("policy snapshot did not recover after reload: policy=%#v err=%v", recovered, err)
	}
}

func TestLoadOldConfigGetsSafeClaudePromptDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	if err := os.WriteFile(path, []byte(`{"port_api":2156}`), 0o600); err != nil {
		t.Fatal(err)
	}
	InvalidateCache()
	t.Cleanup(InvalidateCache)

	cfg := Load()
	if cfg.ClaudePromptInjectionEnabled || cfg.ClaudePromptReplacementEnabled ||
		!cfg.ClaudePromptStripPromotions || !cfg.ClaudePromptReplaceSecurity ||
		cfg.ClaudePromptInjectionPosition != "append" {
		t.Fatalf("old config got unsafe Claude prompt defaults: %#v", cfg)
	}
}

func TestLoadCanDisableClaudeCodePromotionRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	if err := os.WriteFile(path, []byte(
		`{"claude_prompt_strip_claude_code_promotions":false}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	InvalidateCache()
	t.Cleanup(InvalidateCache)

	if cfg := Load(); cfg.ClaudePromptStripPromotions {
		t.Fatalf("explicit promotion-removal opt-out was ignored: %#v", cfg)
	}
}

func TestLoadCanDisableClaudeSecurityPreambleReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	if err := os.WriteFile(path, []byte(
		`{"claude_prompt_replace_security_preamble":false}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	InvalidateCache()
	t.Cleanup(InvalidateCache)

	if cfg := Load(); cfg.ClaudePromptReplaceSecurity {
		t.Fatalf("explicit security-preamble replacement opt-out was ignored: %#v", cfg)
	}
}

func TestLoadLegacyClaudePromptRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	if err := os.WriteFile(path, []byte(`{
		"claude_prompt_replacement_enabled": true,
		"claude_prompt_replace_from": "legacy",
		"claude_prompt_replace_to": "replacement"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	InvalidateCache()
	t.Cleanup(InvalidateCache)

	rules := GetProvider().ClaudePromptReplacementRules()
	if len(rules) != 1 || rules[0].From != "legacy" || rules[0].To != "replacement" {
		t.Fatalf("legacy config was not loaded as a replacement rule: %#v", rules)
	}
}
