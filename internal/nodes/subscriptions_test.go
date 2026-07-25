package nodes

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/db"
)

func TestProxySubscriptionLifecycleAndNodeReplacement(t *testing.T) {
	db.CloseDB()
	if err := db.InitDB(filepath.Join(t.TempDir(), "subscriptions.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.CloseDB)
	resetState()

	manual := Node{Type: "http", Name: "manual", RawURI: "http://127.0.0.1:8000"} //nolint:exhaustruct
	MergeNodes([]Node{manual})

	item, err := SaveProxySubscription(ProxySubscription{
		Name:                   "pool",
		URL:                    "https://example.com/proxies.txt",
		ProxyType:              "socks5",
		RefreshIntervalMinutes: 30,
		Enabled:                true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if item.ID == 0 {
		t.Fatal("subscription ID was not assigned")
	}

	managed, err := UpsertManagedProxySubscription("environment:test", ProxySubscription{
		Name:                   "managed",
		URL:                    "https://example.com/first.txt",
		ProxyType:              "http",
		RefreshIntervalMinutes: 60,
		Enabled:                true,
	})
	if err != nil {
		t.Fatal(err)
	}
	managedUpdated, err := UpsertManagedProxySubscription("environment:test", ProxySubscription{
		Name:                   "managed updated",
		URL:                    "https://example.com/second.txt",
		ProxyType:              "socks5",
		RefreshIntervalMinutes: 90,
		Enabled:                true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if managedUpdated.ID != managed.ID || managedUpdated.ManagedKey != "environment:test" ||
		managedUpdated.URL != "https://example.com/second.txt" ||
		managedUpdated.RefreshIntervalMinutes != 90 {
		t.Fatalf("managed subscription was not updated in place: %#v", managedUpdated)
	}
	if err := DeleteProxySubscription(managed.ID); err != nil {
		t.Fatal(err)
	}

	first, err := SyncSubscriptionNodes(item.ID, []Node{
		{Type: "socks5", Name: "one", RawURI: "socks5://127.0.0.1:1080"}, //nolint:exhaustruct
		{Type: "socks5", Name: "two", RawURI: "socks5://127.0.0.2:1080"}, //nolint:exhaustruct
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Count != 2 || first.Added != 2 || first.Removed != 0 {
		t.Fatalf("first sync result: %#v", first)
	}

	preservedURI := "socks5://127.0.0.1:1080"
	RecordTest(preservedURI, true, 12.5, "")
	BatchUpdateNodesDisabled([]string{preservedURI}, true)

	second, err := SyncSubscriptionNodes(item.ID, []Node{
		{Type: "socks5", Name: "one renamed", RawURI: preservedURI},        //nolint:exhaustruct
		{Type: "socks5", Name: "three", RawURI: "socks5://127.0.0.3:1080"}, //nolint:exhaustruct
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Count != 2 || second.Added != 1 || second.Removed != 1 {
		t.Fatalf("second sync result: %#v", second)
	}
	list := LoadNodes()
	if len(list) != 3 {
		t.Fatalf("manual node plus two subscription nodes expected, got %d", len(list))
	}
	for _, node := range list {
		if node.RawURI == manual.RawURI && node.SourceID != 0 {
			t.Fatal("manual node source was modified")
		}
		if node.RawURI == preservedURI && (!node.Disabled || node.Name != "one renamed") {
			t.Fatalf("unchanged node state/name not preserved correctly: %#v", node)
		}
	}
	if health := LoadHealth()[preservedURI]; health == nil || health.SuccessCount != 1 || health.LastTestMs != 12.5 {
		t.Fatalf("unchanged node health was lost: %#v", health)
	}

	if err := UpdateProxySubscriptionResult(item.ID, second.Count, nil); err != nil {
		t.Fatal(err)
	}
	due, err := DueProxySubscriptions(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("freshly updated subscription should not be due: %#v", due)
	}

	if err := UpdateProxySubscriptionResult(item.ID, second.Count, errors.New("temporary failure")); err != nil {
		t.Fatal(err)
	}
	failed, err := GetProxySubscription(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.LastRefreshedAt == 0 || failed.LastAttemptAt == 0 ||
		failed.ConsecutiveFailures != 1 || failed.NodeCount != second.Count {
		t.Fatalf("unexpected failed refresh state: %#v", failed)
	}
	due, err = DueProxySubscriptions(time.Unix(failed.LastAttemptAt, 0).Add(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("failed subscription retried too early: %#v", due)
	}
	due, err = DueProxySubscriptions(time.Unix(failed.LastAttemptAt, 0).Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != item.ID {
		t.Fatalf("failed subscription should retry after backoff: %#v", due)
	}

	removed, err := DeleteSubscriptionNodes(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("expected both subscription nodes to be removed, got %d", removed)
	}
	if err := DeleteProxySubscription(item.ID); err != nil {
		t.Fatal(err)
	}
	if got := LoadNodes(); len(got) != 1 || got[0].RawURI != manual.RawURI {
		t.Fatalf("deleting subscription must preserve manual node: %#v", got)
	}
}

func TestSharedNodeSurvivesSingleSubscriptionRemoval(t *testing.T) {
	db.CloseDB()
	if err := db.InitDB(filepath.Join(t.TempDir(), "shared-subscriptions.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.CloseDB)
	resetState()

	first, err := SaveProxySubscription(ProxySubscription{
		Name: "first", URL: "https://example.com/first", ProxyType: "http",
		RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := SaveProxySubscription(ProxySubscription{
		Name: "second", URL: "https://example.com/second", ProxyType: "http",
		RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	shared := Node{Type: "http", Name: "shared", RawURI: "http://127.0.0.1:8080"}     //nolint:exhaustruct
	onlyFirst := Node{Type: "http", Name: "first", RawURI: "http://127.0.0.1:8081"}   //nolint:exhaustruct
	onlySecond := Node{Type: "http", Name: "second", RawURI: "http://127.0.0.1:8082"} //nolint:exhaustruct
	if _, err := SyncSubscriptionNodes(first.ID, []Node{shared, onlyFirst}); err != nil {
		t.Fatal(err)
	}
	RecordTest(shared.RawURI, true, 10, "")
	if _, err := SyncSubscriptionNodes(second.ID, []Node{shared, onlySecond}); err != nil {
		t.Fatal(err)
	}

	result, err := SyncSubscriptionNodes(first.ID, []Node{onlyFirst})
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 0 {
		t.Fatalf("shared physical node must not be removed with one membership: %#v", result)
	}
	list := LoadNodes()
	foundShared := false
	for _, node := range list {
		if node.RawURI == shared.RawURI {
			foundShared = true
			if node.SourceID != second.ID {
				t.Fatalf("shared node should move to remaining subscription: %#v", node)
			}
		}
	}
	if !foundShared {
		t.Fatal("shared node disappeared after removing only one subscription membership")
	}
	if health := LoadHealth()[shared.RawURI]; health == nil || health.SuccessCount != 1 {
		t.Fatalf("shared node health was not preserved: %#v", health)
	}

	removed, err := DeleteSubscriptionNodes(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("expected second-only and shared nodes to be removed, got %d", removed)
	}
	list = LoadNodes()
	if len(list) != 1 || list[0].RawURI != onlyFirst.RawURI {
		t.Fatalf("unexpected remaining nodes: %#v", list)
	}
}
