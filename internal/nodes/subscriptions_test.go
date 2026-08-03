package nodes

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/db"
)

func BenchmarkDueProxySubscriptionsMostlyNotDue(b *testing.B) {
	const (
		subscriptionCount = 10_000
		dueCount          = 100
	)
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "due-subscriptions-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	database := db.CurrentDB()
	tx, err := database.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO proxy_subscriptions
		(name, url, proxy_type, refresh_interval_minutes, enabled,
		 last_refreshed_at, created_at, updated_at)
		VALUES (?, ?, 'http', 60, 1, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		b.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	for index := range subscriptionCount {
		lastRefreshedAt := now.Unix()
		if index < dueCount {
			lastRefreshedAt = 0
		}
		if _, err = stmt.Exec(
			fmt.Sprintf("subscription-%d", index),
			fmt.Sprintf("https://example.com/%d", index),
			lastRefreshedAt,
			now.Unix(),
			now.Unix(),
		); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			b.Fatal(err)
		}
	}
	if err = stmt.Close(); err != nil {
		_ = tx.Rollback()
		b.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		items, queryErr := DueProxySubscriptions(now)
		if queryErr != nil {
			b.Fatal(queryErr)
		}
		if len(items) != dueCount {
			b.Fatalf("due subscription count = %d, want %d", len(items), dueCount)
		}
	}
}

func TestDueProxySubscriptionsDatabasePrefilterPreservesSchedule(t *testing.T) {
	db.CloseDB()
	if err := db.InitDB(filepath.Join(t.TempDir(), "due-subscriptions.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.CloseDB)
	resetState()

	now := time.Unix(2_000_000_000, 0)
	type subscriptionState struct {
		name          string
		enabled       bool
		interval      int
		lastRefreshed int64
		lastAttempt   int64
		lastError     string
		failures      int
		wantDue       bool
	}
	states := []subscriptionState{
		{name: "never refreshed", enabled: true, interval: 60, wantDue: true},
		{name: "fresh", enabled: true, interval: 60, lastRefreshed: now.Unix(), wantDue: false},
		{name: "interval elapsed", enabled: true, interval: 60,
			lastRefreshed: now.Add(-time.Hour).Unix(), wantDue: true},
		{name: "disabled", enabled: false, interval: 60, wantDue: false},
		{name: "retry too early", enabled: true, interval: 60,
			lastAttempt: now.Add(-59 * time.Second).Unix(), lastError: "temporary", failures: 1, wantDue: false},
		{name: "retry elapsed", enabled: true, interval: 60,
			lastAttempt: now.Add(-time.Minute).Unix(), lastError: "temporary", failures: 1, wantDue: true},
		{name: "retry capped by interval", enabled: true, interval: 1,
			lastAttempt: now.Add(-time.Minute).Unix(), lastError: "temporary", failures: 8, wantDue: true},
		{name: "legacy error without attempt", enabled: true, interval: 60,
			lastError: "legacy", wantDue: true},
		{name: "future refresh", enabled: true, interval: 60,
			lastRefreshed: now.Add(time.Hour).Unix(), wantDue: false},
	}

	database := db.CurrentDB()
	for _, state := range states {
		_, err := database.Exec(`INSERT INTO proxy_subscriptions
			(name, url, proxy_type, refresh_interval_minutes, enabled,
			 last_refreshed_at, last_attempt_at, last_error, consecutive_failures,
			 created_at, updated_at)
			VALUES (?, ?, 'http', ?, ?, ?, ?, ?, ?, ?, ?)`,
			state.name, "https://example.com/"+state.name, state.interval, state.enabled,
			state.lastRefreshed, state.lastAttempt, state.lastError, state.failures,
			now.Unix(), now.Unix(),
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	due, err := DueProxySubscriptions(now)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(due))
	for _, item := range due {
		got[item.Name] = true
	}
	for _, state := range states {
		if got[state.name] != state.wantDue {
			t.Errorf("subscription %q due = %t, want %t", state.name, got[state.name], state.wantDue)
		}
	}
}

func BenchmarkSyncSubscriptionLargePoolNoChanges(b *testing.B) {
	const nodeCount = 5000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "subscription-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	item, err := SaveProxySubscription(ProxySubscription{
		Name: "benchmark", URL: "https://example.com/benchmark", ProxyType: "http",
		RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	proxies := make([]Node, nodeCount)
	for index := range proxies {
		proxies[index] = Node{
			Type:   "http",
			Name:   fmt.Sprintf("node-%d", index),
			RawURI: fmt.Sprintf("http://benchmark-%d.invalid:8080", index),
		}
	}
	if _, err = SyncSubscriptionNodes(item.ID, proxies); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err = SyncSubscriptionNodes(item.ID, proxies); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSyncSubscriptionLargePoolInitialLoad(b *testing.B) {
	const nodeCount = 2000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "subscription-initial-load-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	item, err := SaveProxySubscription(ProxySubscription{
		Name: "initial-load-benchmark", URL: "https://example.com/initial-load-benchmark",
		ProxyType: "http", RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	proxies := make([]Node, nodeCount)
	for index := range proxies {
		proxies[index] = Node{
			Type: "http", Name: fmt.Sprintf("node-%d", index),
			RawURI: fmt.Sprintf("http://initial-load-%d.invalid:8080", index),
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err = SyncSubscriptionNodes(item.ID, proxies); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if _, err = SyncSubscriptionNodes(item.ID, nil); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

func BenchmarkSyncSubscriptionLargePoolOneReplacement(b *testing.B) {
	const nodeCount = 5000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "subscription-change-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	item, err := SaveProxySubscription(ProxySubscription{
		Name: "change-benchmark", URL: "https://example.com/change-benchmark", ProxyType: "http",
		RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	proxies := make([]Node, nodeCount)
	for index := range proxies {
		proxies[index] = Node{
			Type:   "http",
			Name:   fmt.Sprintf("node-%d", index),
			RawURI: fmt.Sprintf("http://change-benchmark-%d.invalid:8080", index),
		}
	}
	if _, err = SyncSubscriptionNodes(item.ID, proxies); err != nil {
		b.Fatal(err)
	}
	firstSet := append([]Node(nil), proxies...)
	secondSet := append([]Node(nil), proxies...)
	firstSet[len(firstSet)-1] = Node{
		Type: "http", Name: "replacement-a", RawURI: "http://replacement-a.invalid:8080",
	}
	secondSet[len(secondSet)-1] = Node{
		Type: "http", Name: "replacement-b", RawURI: "http://replacement-b.invalid:8080",
	}
	mu.Lock()
	for _, node := range nodeList {
		healthMap[node.RawURI] = &NodeHealth{SuccessCount: 1, LastSuccessAt: time.Now().Unix()}
	}
	mu.Unlock()

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := range b.N {
		next := firstSet
		if iteration%2 != 0 {
			next = secondSet
		}
		if _, err = SyncSubscriptionNodes(item.ID, next); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSyncSubscriptionLargePoolFullReplacement(b *testing.B) {
	const nodeCount = 2000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "subscription-full-replacement-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	item, err := SaveProxySubscription(ProxySubscription{
		Name: "full-replacement-benchmark", URL: "https://example.com/full-replacement-benchmark",
		ProxyType: "http", RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	firstSet := make([]Node, nodeCount)
	secondSet := make([]Node, nodeCount)
	for index := range nodeCount {
		firstSet[index] = Node{
			Type: "http", Name: fmt.Sprintf("first-%d", index),
			RawURI: fmt.Sprintf("http://full-replacement-a-%d.invalid:8080", index),
		}
		secondSet[index] = Node{
			Type: "http", Name: fmt.Sprintf("second-%d", index),
			RawURI: fmt.Sprintf("http://full-replacement-b-%d.invalid:8080", index),
		}
	}
	if _, err = SyncSubscriptionNodes(item.ID, firstSet); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := range b.N {
		next := secondSet
		if iteration%2 != 0 {
			next = firstSet
		}
		if _, err = SyncSubscriptionNodes(item.ID, next); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSyncSubscriptionLargePoolAllMetadataUpdated(b *testing.B) {
	const nodeCount = 2000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "subscription-metadata-update-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	item, err := SaveProxySubscription(ProxySubscription{
		Name: "metadata-update-benchmark", URL: "https://example.com/metadata-update-benchmark",
		ProxyType: "http", RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	firstSet := make([]Node, nodeCount)
	secondSet := make([]Node, nodeCount)
	for index := range nodeCount {
		rawURI := fmt.Sprintf("http://metadata-update-%d.invalid:8080", index)
		firstSet[index] = Node{Type: "http", Name: fmt.Sprintf("first-%d", index), RawURI: rawURI}
		secondSet[index] = Node{Type: "https", Name: fmt.Sprintf("second-%d", index), RawURI: rawURI}
	}
	if _, err = SyncSubscriptionNodes(item.ID, firstSet); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := range b.N {
		next := secondSet
		if iteration%2 != 0 {
			next = firstSet
		}
		if _, err = SyncSubscriptionNodes(item.ID, next); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSyncSubscriptionDeduplicatesInputWithStableOrderAndLastValue(t *testing.T) {
	db.CloseDB()
	if err := db.InitDB(filepath.Join(t.TempDir(), "subscription-dedup.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.CloseDB)
	resetState()

	item, err := SaveProxySubscription(ProxySubscription{
		Name: "dedup", URL: "https://example.com/dedup", ProxyType: "http",
		RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstURI := "http://first.invalid:8080"
	secondURI := "http://second.invalid:8080"
	input := []Node{
		{Type: "http", Name: "first-old", RawURI: "  " + firstURI + "  "},
		{Type: "http", Name: "second", RawURI: secondURI},
		{Type: "http", Name: "first-new", RawURI: firstURI},
	}
	result, err := SyncSubscriptionNodes(item.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 2 || result.Added != 2 || result.Removed != 0 {
		t.Fatalf("deduplicated sync result: %#v", result)
	}
	loaded := LoadNodes()
	if len(loaded) != 2 ||
		loaded[0].RawURI != firstURI ||
		loaded[0].Name != "first-new" ||
		loaded[1].RawURI != secondURI {
		t.Fatalf("deduplicated nodes lost order or last value: %#v", loaded)
	}

	result, err = SyncSubscriptionNodes(item.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 2 || result.Added != 0 || result.Removed != 0 {
		t.Fatalf("unchanged deduplicated sync result: %#v", result)
	}

	duplicate := Node{Type: "http", Name: "first-new", RawURI: firstURI}
	result, err = SyncSubscriptionNodes(item.ID, []Node{duplicate, duplicate})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || result.Added != 0 || result.Removed != 1 {
		t.Fatalf("duplicate-only sync result: %#v", result)
	}
	loaded = LoadNodes()
	if len(loaded) != 1 || loaded[0].RawURI != firstURI || loaded[0].Name != "first-new" {
		t.Fatalf("duplicate input incorrectly hit unchanged fast path: %#v", loaded)
	}
}

func TestProxySubscriptionLifecycleAndNodeReplacement(t *testing.T) {
	db.CloseDB()
	if err := db.InitDB(filepath.Join(t.TempDir(), "subscriptions.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.CloseDB)
	resetState()

	manual := Node{Type: "http", Name: "manual", RawURI: "http://127.0.0.1:8000"} //nolint:exhaustruct
	if err := MergeNodes([]Node{manual}); err != nil {
		t.Fatal(err)
	}

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
	if err := BatchUpdateNodesDisabled([]string{preservedURI}, true); err != nil {
		t.Fatal(err)
	}

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
	persistedSubscription, err := GetProxySubscription(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedSubscription.NodeCount != second.Count {
		t.Fatalf("incremental sync node_count=%d, want %d", persistedSubscription.NodeCount, second.Count)
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
	var persistedSourceID int64
	if err := db.CurrentDB().QueryRow(
		"SELECT source_id FROM nodes WHERE raw_uri = ?",
		shared.RawURI,
	).Scan(&persistedSourceID); err != nil {
		t.Fatal(err)
	}
	var firstMembership, secondMembership int
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM proxy_subscription_nodes WHERE subscription_id = ? AND raw_uri = ?",
		first.ID,
		shared.RawURI,
	).Scan(&firstMembership); err != nil {
		t.Fatal(err)
	}
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM proxy_subscription_nodes WHERE subscription_id = ? AND raw_uri = ?",
		second.ID,
		shared.RawURI,
	).Scan(&secondMembership); err != nil {
		t.Fatal(err)
	}
	if persistedSourceID != second.ID || firstMembership != 0 || secondMembership != 1 {
		t.Fatalf(
			"persisted shared ownership source=%d first=%d second=%d",
			persistedSourceID,
			firstMembership,
			secondMembership,
		)
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
