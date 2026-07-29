package api

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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

func TestRefreshProxySubscriptionsUsesFixedWorkerPool(t *testing.T) {
	items := make([]nodes.ProxySubscription, 1000)
	for i := range items {
		items[i].ID = int64(i + 1)
	}

	var active atomic.Int32
	var maxActive atomic.Int32
	var refreshed atomic.Int32
	refreshProxySubscriptions(context.Background(), items, func(
		_ context.Context,
		_ nodes.ProxySubscription,
	) (int, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maxActive.Load()
			if current <= observed || maxActive.CompareAndSwap(observed, current) {
				break
			}
		}
		time.Sleep(time.Microsecond)
		refreshed.Add(1)
		return 0, nil
	})

	if got := refreshed.Load(); got != int32(len(items)) {
		t.Fatalf("refreshed %d subscriptions, want %d", got, len(items))
	}
	if got := maxActive.Load(); got != proxyRefreshConcurrency {
		t.Fatalf("maximum refresh concurrency = %d, want %d", got, proxyRefreshConcurrency)
	}
}

func TestRefreshProxySubscriptionsStopsQueueingAfterCancellation(t *testing.T) {
	items := make([]nodes.ProxySubscription, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, proxyRefreshConcurrency)
	returned := make(chan struct{})
	var refreshed atomic.Int32

	go func() {
		defer close(returned)
		refreshProxySubscriptions(ctx, items, func(
			ctx context.Context,
			_ nodes.ProxySubscription,
		) (int, error) {
			refreshed.Add(1)
			started <- struct{}{}
			<-ctx.Done()
			return 0, ctx.Err()
		})
	}()

	for range proxyRefreshConcurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start in time")
		}
	}
	cancel()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("worker pool did not stop after cancellation")
	}
	if got := refreshed.Load(); got != proxyRefreshConcurrency {
		t.Fatalf("refreshes started after cancellation: got %d, want %d", got, proxyRefreshConcurrency)
	}
}

func BenchmarkRefreshProxySubscriptions(b *testing.B) {
	items := make([]nodes.ProxySubscription, 10000)
	refresh := func(context.Context, nodes.ProxySubscription) (int, error) {
		return 0, nil
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		refreshProxySubscriptions(context.Background(), items, refresh)
	}
}
