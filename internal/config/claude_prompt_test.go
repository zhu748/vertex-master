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
	cfg.ClaudePromptReplaceFrom = "from"
	cfg.ClaudePromptReplaceTo = "to"
	provider := StaticProvider(cfg)
	if !provider.ClaudePromptInjectionEnabled() ||
		provider.ClaudePromptInjectionPosition() != "prepend" ||
		provider.ClaudePromptInjectionText() != "inject" ||
		!provider.ClaudePromptReplacementEnabled() ||
		provider.ClaudePromptReplaceFrom() != "from" ||
		provider.ClaudePromptReplaceTo() != "to" {
		t.Fatalf("static provider lost Claude prompt settings")
	}

	cfg.ClaudePromptInjectionPosition = "invalid"
	if got := StaticProvider(cfg).ClaudePromptInjectionPosition(); got != "append" {
		t.Fatalf("invalid injection position=%q, want append fallback", got)
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
