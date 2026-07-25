package api

import (
	"path/filepath"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/db"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
)

func TestProxySubscriptionFromEnvironmentDefaults(t *testing.T) {
	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_URL", "https://user:pass@example.com/list.txt?token=secret")
	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_TYPE", "")
	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_INTERVAL_MINUTES", "")

	item, err := proxySubscriptionFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if item == nil || item.ManagedKey != environmentProxySubscriptionKey ||
		item.ProxyType != "http" || item.RefreshIntervalMinutes != 60 ||
		item.Name != "环境变量代理池 (example.com)" || !item.Enabled {
		t.Fatalf("unexpected environment subscription: %#v", item)
	}
}

func TestProxySubscriptionFromEnvironmentOverrides(t *testing.T) {
	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_URL", "https://example.com/list.txt")
	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_TYPE", "SOCKS5")
	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_INTERVAL_MINUTES", "90")

	item, err := proxySubscriptionFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if item.ProxyType != "socks5" || item.RefreshIntervalMinutes != 90 {
		t.Fatalf("environment overrides were not applied: %#v", item)
	}
}

func TestProxySubscriptionFromEnvironmentValidation(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		proxy    string
		interval string
		wantNil  bool
	}{
		{name: "disabled", wantNil: true},
		{name: "invalid URL", url: "file:///tmp/list.txt"},
		{name: "invalid type", url: "https://example.com/list.txt", proxy: "ssh"},
		{name: "invalid interval", url: "https://example.com/list.txt", interval: "0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("VPROXY_PROXY_SUBSCRIPTION_URL", test.url)
			t.Setenv("VPROXY_PROXY_SUBSCRIPTION_TYPE", test.proxy)
			t.Setenv("VPROXY_PROXY_SUBSCRIPTION_INTERVAL_MINUTES", test.interval)
			item, err := proxySubscriptionFromEnvironment()
			if test.wantNil {
				if err != nil || item != nil {
					t.Fatalf("expected disabled subscription, got %#v, %v", item, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected validation error, got %#v", item)
			}
		})
	}
}

func TestSyncEnvironmentProxySubscriptionLifecycle(t *testing.T) {
	db.CloseDB()
	if err := db.InitDB(filepath.Join(t.TempDir(), "proxy-pool.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.CloseDB)

	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_URL", "https://example.com/first.txt")
	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_TYPE", "")
	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_INTERVAL_MINUTES", "")
	if err := SyncEnvironmentProxySubscription(); err != nil {
		t.Fatal(err)
	}
	first, err := nodes.GetManagedProxySubscription(environmentProxySubscriptionKey)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_URL", "https://example.com/second.txt")
	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_TYPE", "socks5")
	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_INTERVAL_MINUTES", "120")
	if err := SyncEnvironmentProxySubscription(); err != nil {
		t.Fatal(err)
	}
	updated, err := nodes.GetManagedProxySubscription(environmentProxySubscriptionKey)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != first.ID || updated.URL != "https://example.com/second.txt" ||
		updated.ProxyType != "socks5" || updated.RefreshIntervalMinutes != 120 {
		t.Fatalf("managed subscription was not synchronized in place: %#v", updated)
	}

	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_URL", "")
	if err := SyncEnvironmentProxySubscription(); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.GetManagedProxySubscription(environmentProxySubscriptionKey); err == nil {
		t.Fatal("managed subscription should be removed when the environment URL is empty")
	}
}
