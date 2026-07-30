package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudePromptConfigDefaultsAndProvider(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ClaudePromptInjectionEnabled || cfg.ClaudePromptReplacementEnabled {
		t.Fatal("Claude prompt rewriting must be opt-in")
	}
	if cfg.ClaudePromptInjectionPosition != "append" {
		t.Fatalf("default injection position=%q, want append", cfg.ClaudePromptInjectionPosition)
	}

	cfg.ClaudePromptInjectionEnabled = true
	cfg.ClaudePromptInjectionPosition = "prepend"
	cfg.ClaudePromptInjectionText = "inject"
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []ClaudePromptReplacementRule{
		{From: "from one", To: "to one"},
		{From: "from two", To: "to two"},
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
	if provider.ClaudePromptReplacementRules()[0].From != "from one" {
		t.Fatal("provider exposed its replacement rule slice for mutation")
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
		cfg.ClaudePromptInjectionPosition != "append" {
		t.Fatalf("old config got unsafe Claude prompt defaults: %#v", cfg)
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
