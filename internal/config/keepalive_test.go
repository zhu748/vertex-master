package config

import "testing"

func TestKeepAliveEnvironmentCompatibilityAndPriority(t *testing.T) {
	t.Setenv("KEEPALIVE_URL", "https://legacy.example/keepalive")
	t.Setenv("KEEPALIVE_INTERVAL", "90")
	t.Setenv("VPROXY_KEEPALIVE_URL", "https://preferred.example/keepalive")
	t.Setenv("VPROXY_KEEPALIVE_INTERVAL_SECONDS", "45")

	cfg := DefaultConfig()
	applyEnvironmentOverrides(&cfg)
	if cfg.KeepAliveURL != "https://preferred.example/keepalive" {
		t.Fatalf("keepalive URL = %q", cfg.KeepAliveURL)
	}
	if cfg.KeepAliveIntervalSeconds != 45 {
		t.Fatalf("keepalive interval = %d", cfg.KeepAliveIntervalSeconds)
	}
}

func TestKeepAliveEmptyPrefixedURLDisablesLegacyValue(t *testing.T) {
	t.Setenv("KEEPALIVE_URL", "https://legacy.example/keepalive")
	t.Setenv("VPROXY_KEEPALIVE_URL", "")

	cfg := DefaultConfig()
	cfg.KeepAliveURL = "https://config.example/keepalive"
	applyEnvironmentOverrides(&cfg)
	if cfg.KeepAliveURL != "" {
		t.Fatalf("explicit empty URL should disable keep-alive, got %q", cfg.KeepAliveURL)
	}
}

func TestValidateKeepAliveURL(t *testing.T) {
	for _, value := range []string{"", "http://127.0.0.1:2156/keepalive", "https://example.com/path"} {
		if err := ValidateKeepAliveURL(value); err != nil {
			t.Errorf("ValidateKeepAliveURL(%q): %v", value, err)
		}
	}
	for _, value := range []string{"example.com", "ftp://example.com", "https://user:pass@example.com"} {
		if err := ValidateKeepAliveURL(value); err == nil {
			t.Errorf("ValidateKeepAliveURL(%q) unexpectedly succeeded", value)
		}
	}
}
