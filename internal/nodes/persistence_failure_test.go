package nodes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/db"
)

func BenchmarkBatchUpdateLargePoolRollback(b *testing.B) {
	const nodeCount = 5000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "batch-update-rollback-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	proxies := make([]Node, nodeCount)
	for index := range proxies {
		proxies[index] = Node{
			Type:   "http",
			Name:   fmt.Sprintf("node-%d", index),
			RawURI: fmt.Sprintf("http://update-benchmark-%d.invalid:8080", index),
		}
	}
	if err := MergeNodes(proxies); err != nil {
		b.Fatal(err)
	}
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_benchmark_update
		BEFORE UPDATE OF disabled ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected benchmark update failure'); END`); err != nil {
		b.Fatal(err)
	}
	target := []string{proxies[len(proxies)-1].RawURI}
	previousLogWriter := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previousLogWriter) })

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := BatchUpdateNodesDisabled(target, true); err == nil {
			b.Fatal("expected injected update failure")
		}
	}
}

func BenchmarkDeleteNodeLargePoolRollback(b *testing.B) {
	const nodeCount = 5000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "delete-node-rollback-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	proxies := make([]Node, nodeCount)
	for index := range proxies {
		proxies[index] = Node{
			Type:   "http",
			Name:   fmt.Sprintf("node-%d", index),
			RawURI: fmt.Sprintf("http://delete-node-benchmark-%d.invalid:8080", index),
		}
	}
	if err := MergeNodes(proxies); err != nil {
		b.Fatal(err)
	}
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_benchmark_delete_node
		BEFORE DELETE ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected benchmark node delete failure'); END`); err != nil {
		b.Fatal(err)
	}
	target := proxies[len(proxies)-1].RawURI
	previousLogWriter := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previousLogWriter) })

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		DeleteNode(target)
	}
	b.StopTimer()
	mu.RLock()
	defer mu.RUnlock()
	if len(nodeList) != nodeCount || nodeList[len(nodeList)-1].RawURI != target {
		b.Fatal("failed node deletion was not rolled back")
	}
}

func BenchmarkDeleteDisabledLargePoolRollback(b *testing.B) {
	const nodeCount = 5000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "delete-disabled-rollback-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	proxies := make([]Node, nodeCount)
	for index := range proxies {
		proxies[index] = Node{
			Type:   "http",
			Name:   fmt.Sprintf("node-%d", index),
			RawURI: fmt.Sprintf("http://delete-disabled-benchmark-%d.invalid:8080", index),
		}
	}
	if err := MergeNodes(proxies); err != nil {
		b.Fatal(err)
	}
	target := proxies[len(proxies)-1].RawURI
	if err := BatchUpdateNodesDisabled([]string{target}, true); err != nil {
		b.Fatal(err)
	}
	mu.Lock()
	healthMap[target] = &NodeHealth{SuccessCount: 1}
	mu.Unlock()
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_benchmark_delete_disabled
		BEFORE DELETE ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected benchmark disabled delete failure'); END`); err != nil {
		b.Fatal(err)
	}
	previousLogWriter := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previousLogWriter) })

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if removed := DeleteDisabled(); removed != 0 {
			b.Fatalf("failed disabled deletion reported %d removals", removed)
		}
	}
	b.StopTimer()
	mu.RLock()
	defer mu.RUnlock()
	if len(nodeList) != nodeCount || !nodeList[len(nodeList)-1].Disabled ||
		healthMap[target] == nil {
		b.Fatal("failed disabled deletion was not rolled back")
	}
}

func BenchmarkBatchDeleteLargePoolRollback(b *testing.B) {
	const nodeCount = 5000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "batch-delete-rollback-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	subscription, err := SaveProxySubscription(ProxySubscription{
		Name: "delete-benchmark", URL: "https://example.com/delete-benchmark", ProxyType: "http",
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
			RawURI: fmt.Sprintf("http://delete-benchmark-%d.invalid:8080", index),
		}
	}
	if _, err = SyncSubscriptionNodes(subscription.ID, proxies); err != nil {
		b.Fatal(err)
	}
	mu.Lock()
	for _, node := range nodeList {
		healthMap[node.RawURI] = &NodeHealth{SuccessCount: 1, LastSuccessAt: time.Now().Unix()}
	}
	mu.Unlock()
	if _, err = db.CurrentDB().Exec(`CREATE TRIGGER fail_benchmark_delete
		BEFORE DELETE ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected benchmark delete failure'); END`); err != nil {
		b.Fatal(err)
	}
	target := []string{proxies[len(proxies)-1].RawURI}
	previousLogWriter := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previousLogWriter) })

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err = BatchDeleteNodes(target); err == nil {
			b.Fatal("expected injected delete failure")
		}
	}
}

func BenchmarkReplaceManualLargePoolOneReplacement(b *testing.B) {
	const subscriptionNodeCount = 4999
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "replace-manual-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	subscription, err := SaveProxySubscription(ProxySubscription{
		Name: "manual-benchmark", URL: "https://example.com/manual-benchmark", ProxyType: "http",
		RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	proxies := make([]Node, subscriptionNodeCount)
	for index := range proxies {
		proxies[index] = Node{
			Type:   "http",
			Name:   fmt.Sprintf("node-%d", index),
			RawURI: fmt.Sprintf("http://manual-benchmark-%d.invalid:8080", index),
		}
	}
	if _, err = SyncSubscriptionNodes(subscription.ID, proxies); err != nil {
		b.Fatal(err)
	}
	if err = MergeNodes([]Node{{
		Type: "http", Name: "manual-initial", RawURI: "http://manual-initial.invalid:8080",
	}}); err != nil {
		b.Fatal(err)
	}
	mu.Lock()
	for _, node := range nodeList {
		healthMap[node.RawURI] = &NodeHealth{SuccessCount: 1, LastSuccessAt: time.Now().Unix()}
	}
	mu.Unlock()
	first := []Node{{
		Type: "http", Name: "manual-a", RawURI: "http://manual-a.invalid:8080",
	}}
	second := []Node{{
		Type: "http", Name: "manual-b", RawURI: "http://manual-b.invalid:8080",
	}}

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := range b.N {
		next := first
		if iteration%2 != 0 {
			next = second
		}
		if err = ReplaceManualNodes(next); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDedupLargePoolSingleDuplicateRollback(b *testing.B) {
	const nodeCount = 5000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "dedup-rollback-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	subscription, err := SaveProxySubscription(ProxySubscription{
		Name: "dedup-benchmark", URL: "https://example.com/dedup-benchmark", ProxyType: "http",
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
			RawURI: fmt.Sprintf("http://dedup-benchmark-%d.invalid:8080", index),
		}
	}
	proxies[len(proxies)-2] = Node{
		Type: "vless", Name: "duplicate-a",
		RawURI: "vless://benchmark-uuid@example.com:443?security=tls#a",
	}
	proxies[len(proxies)-1] = Node{
		Type: "vless", Name: "duplicate-b",
		RawURI: "vless://benchmark-uuid@example.com:443?security=tls#b",
	}
	if _, err = SyncSubscriptionNodes(subscription.ID, proxies); err != nil {
		b.Fatal(err)
	}
	mu.Lock()
	for _, node := range nodeList {
		healthMap[node.RawURI] = &NodeHealth{SuccessCount: 1, LastSuccessAt: time.Now().Unix()}
	}
	mu.Unlock()
	if _, err = db.CurrentDB().Exec(`CREATE TRIGGER fail_benchmark_dedup
		BEFORE DELETE ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected benchmark dedup failure'); END`); err != nil {
		b.Fatal(err)
	}
	previousLogWriter := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previousLogWriter) })

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if removed := DedupNodes(); removed != 0 {
			b.Fatalf("failed dedup reported %d removals", removed)
		}
	}
}

func BenchmarkDedupLargePoolNoDuplicates(b *testing.B) {
	const nodeCount = 5000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "dedup-noop-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	proxies := make([]Node, nodeCount)
	for index := range proxies {
		proxies[index] = Node{
			Type:   "http",
			Name:   fmt.Sprintf("node-%d", index),
			RawURI: fmt.Sprintf("http://dedup-noop-%d.invalid:8080", index),
		}
	}
	if err := MergeNodes(proxies); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if removed := DedupNodes(); removed != 0 {
			b.Fatalf("unique node pool reported %d removals", removed)
		}
	}
}

func BenchmarkMergeLargePoolRollback(b *testing.B) {
	const nodeCount = 5000
	db.CloseDB()
	if err := db.InitDB(filepath.Join(b.TempDir(), "merge-rollback-benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.CloseDB)
	resetState()

	proxies := make([]Node, nodeCount)
	for index := range proxies {
		proxies[index] = Node{
			Type:   "http",
			Name:   fmt.Sprintf("node-%d", index),
			RawURI: fmt.Sprintf("http://merge-benchmark-%d.invalid:8080", index),
		}
	}
	if err := MergeNodes(proxies); err != nil {
		b.Fatal(err)
	}
	mu.Lock()
	for _, node := range nodeList {
		healthMap[node.RawURI] = &NodeHealth{SuccessCount: 1, LastSuccessAt: time.Now().Unix()}
	}
	healthMap["http://orphan.invalid:8080"] = &NodeHealth{FailCount: 1}
	mu.Unlock()
	failedURI := "http://merge-failed.invalid:8080"
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_benchmark_merge
		BEFORE INSERT ON nodes WHEN NEW.raw_uri = '` + failedURI + `'
		BEGIN SELECT RAISE(ABORT, 'injected benchmark merge failure'); END`); err != nil {
		b.Fatal(err)
	}
	previousLogWriter := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previousLogWriter) })
	input := []Node{{Type: "http", Name: "failed", RawURI: failedURI}}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := MergeNodes(input); err == nil {
			b.Fatal("expected injected merge failure")
		}
	}
}

func TestFlushHealthPersistsQueuedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flush.db")
	db.CloseDB()
	if err := db.InitDB(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.CloseDB)
	resetState()

	proxyURI := "http://8.8.8.8:3128"
	if err := MergeNodes([]Node{{Type: "http", Name: "flush", RawURI: proxyURI}}); err != nil {
		t.Fatal(err)
	}
	RecordTest(proxyURI, true, 123.5, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := FlushHealth(ctx); err != nil {
		t.Fatal(err)
	}

	var successCount int
	var latency float64
	if err := db.CurrentDB().QueryRow(
		"SELECT success_count, last_test_ms FROM node_health WHERE raw_uri = ?",
		proxyURI,
	).Scan(&successCount, &latency); err != nil {
		t.Fatal(err)
	}
	if successCount != 1 || latency != 123.5 {
		t.Fatalf("persisted health = success:%d latency:%.1f", successCount, latency)
	}
}

func TestNodesAndHealthReloadAfterDatabaseRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	db.CloseDB()
	if err := db.InitDB(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.CloseDB)
	resetState()

	manual := Node{Type: "http", Name: "manual", RawURI: "http://8.8.8.8:8080"} //nolint:exhaustruct
	if err := MergeNodes([]Node{manual}); err != nil {
		t.Fatal(err)
	}
	subscription, err := SaveProxySubscription(ProxySubscription{
		Name: "restart", URL: "https://example.com/list", ProxyType: "socks5",
		RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxyURI := "socks5://1.1.1.1:1080"
	if _, err = SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{{
		Type: "socks5", Name: "subscription", RawURI: proxyURI,
	}}); err != nil {
		t.Fatal(err)
	}
	RecordTest(proxyURI, true, 12.5, "")
	waitForPersistedHealth(t, proxyURI)

	db.CloseDB()
	resetState()
	if err := db.InitDB(path); err != nil {
		t.Fatal(err)
	}

	loadedNodes := LoadNodes()
	if len(loadedNodes) != 2 {
		t.Fatalf("expected two reloaded nodes, got %#v", loadedNodes)
	}
	var subscriptionNode *Node
	for i := range loadedNodes {
		if loadedNodes[i].RawURI == proxyURI {
			subscriptionNode = &loadedNodes[i]
		}
	}
	if subscriptionNode == nil || subscriptionNode.SubscriptionSourceCount != 1 {
		t.Fatalf("subscription ownership was not reloaded: %#v", subscriptionNode)
	}
	health := LoadHealth()[proxyURI]
	if health == nil || health.SuccessCount != 1 || health.LastTestMs != 12.5 {
		t.Fatalf("health was not reloaded: %#v", health)
	}
	reloadedSubscription, err := GetProxySubscription(subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedSubscription.LastRefreshedAt == 0 || reloadedSubscription.NodeCount != 1 {
		t.Fatalf("refresh metadata was not committed: %#v", reloadedSubscription)
	}
}

func TestSubscriptionRefreshFailureRollsBackMemoryAndDatabase(t *testing.T) {
	setupFailureTestDB(t, "refresh-rollback.db")
	subscription := createFailureTestSubscription(t)
	oldURI := "http://8.8.8.8:8100"
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{{
		Type: "http", Name: "old", RawURI: oldURI,
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := GetProxySubscription(subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CurrentDB().Exec(`CREATE TRIGGER fail_refresh
		BEFORE UPDATE OF last_refreshed_at ON proxy_subscriptions
		BEGIN SELECT RAISE(ABORT, 'injected refresh failure'); END`); err != nil {
		t.Fatal(err)
	}

	newURI := "http://1.1.1.1:8101"
	if _, err = SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{{
		Type: "http", Name: "new", RawURI: newURI,
	}}); err == nil {
		t.Fatal("expected injected refresh transaction failure")
	}
	assertOnlyNodeURI(t, oldURI)
	after, err := GetProxySubscription(subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastRefreshedAt != before.LastRefreshedAt || after.NodeCount != before.NodeCount {
		t.Fatalf("subscription metadata changed after rollback: before=%#v after=%#v", before, after)
	}
	var oldRelations, newRelations int
	if err = db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM proxy_subscription_nodes WHERE raw_uri = ?",
		oldURI,
	).Scan(&oldRelations); err != nil {
		t.Fatal(err)
	}
	if err = db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM proxy_subscription_nodes WHERE raw_uri = ?",
		newURI,
	).Scan(&newRelations); err != nil {
		t.Fatal(err)
	}
	if oldRelations != 1 || newRelations != 0 {
		t.Fatalf("relations changed after rollback: old=%d new=%d", oldRelations, newRelations)
	}
}

func TestSubscriptionNodeUpdateFailureRestoresPreviousValue(t *testing.T) {
	setupFailureTestDB(t, "subscription-update-rollback.db")
	subscription := createFailureTestSubscription(t)
	proxyURI := "http://8.8.4.4:8120"
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{{
		Type: "http", Name: "old name", RawURI: proxyURI,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_subscription_node_update
		BEFORE UPDATE OF name ON nodes WHEN OLD.raw_uri = '` + proxyURI + `'
		BEGIN SELECT RAISE(ABORT, 'injected subscription node update failure'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := SyncSubscriptionNodes(subscription.ID, []Node{{
		Type: "http", Name: "new name", RawURI: proxyURI,
	}}); err == nil {
		t.Fatal("expected injected subscription node update failure")
	}
	loaded := LoadNodes()
	if len(loaded) != 1 || loaded[0].RawURI != proxyURI ||
		loaded[0].Name != "old name" || loaded[0].SourceID != subscription.ID {
		t.Fatalf("failed subscription update changed memory: %#v", loaded)
	}
	var persistedName string
	if err := db.CurrentDB().QueryRow(
		"SELECT name FROM nodes WHERE raw_uri = ?",
		proxyURI,
	).Scan(&persistedName); err != nil {
		t.Fatal(err)
	}
	if persistedName != "old name" {
		t.Fatalf("failed subscription update changed database name: %q", persistedName)
	}
}

func TestUnchangedSubscriptionRefreshCommitsMetadataAndRollsBackFailure(t *testing.T) {
	setupFailureTestDB(t, "unchanged-refresh.db")
	subscription := createFailureTestSubscription(t)
	proxy := Node{Type: "http", Name: "unchanged", RawURI: "http://8.8.8.8:8150"}
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{proxy}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateProxySubscriptionResult(
		subscription.ID,
		1,
		errors.New("temporary refresh failure"),
	); err != nil {
		t.Fatal(err)
	}
	failed, err := GetProxySubscription(subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.ConsecutiveFailures != 1 || failed.LastError == "" {
		t.Fatalf("failed refresh metadata was not recorded: %#v", failed)
	}

	result, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{proxy})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || result.Added != 0 || result.Removed != 0 {
		t.Fatalf("unchanged refresh result: %#v", result)
	}
	refreshed, err := GetProxySubscription(subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ConsecutiveFailures != 0 || refreshed.LastError != "" ||
		refreshed.NodeCount != 1 {
		t.Fatalf("unchanged refresh did not commit metadata: %#v", refreshed)
	}

	if _, err = db.CurrentDB().Exec(`CREATE TRIGGER fail_unchanged_refresh
		BEFORE UPDATE OF last_refreshed_at ON proxy_subscriptions
		BEGIN SELECT RAISE(ABORT, 'injected unchanged refresh failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{proxy}); err == nil {
		t.Fatal("expected unchanged refresh transaction failure")
	}
	afterFailure, err := GetProxySubscription(subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.LastRefreshedAt != refreshed.LastRefreshedAt ||
		afterFailure.LastAttemptAt != refreshed.LastAttemptAt ||
		afterFailure.NodeCount != refreshed.NodeCount {
		t.Fatalf(
			"failed unchanged refresh changed metadata: before=%#v after=%#v",
			refreshed,
			afterFailure,
		)
	}
	assertOnlyNodeURI(t, proxy.RawURI)
}

func TestMissingSubscriptionSyncRollsBackMemoryAndDatabase(t *testing.T) {
	setupFailureTestDB(t, "missing-subscription.db")
	existingURI := "http://8.8.8.8:8300"
	if err := MergeNodes([]Node{{
		Type: "http", Name: "existing", RawURI: existingURI,
	}}); err != nil {
		t.Fatal(err)
	}

	missingSourceID := int64(999_999)
	newURI := "http://1.1.1.1:8301"
	if _, err := SyncSubscriptionNodes(missingSourceID, []Node{{
		Type: "http", Name: "should rollback", RawURI: newURI,
	}}); err == nil {
		t.Fatal("expected missing subscription sync to fail")
	}
	assertOnlyNodeURI(t, existingURI)
	var newNodeCount, relationCount int
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM nodes WHERE raw_uri = ?",
		newURI,
	).Scan(&newNodeCount); err != nil {
		t.Fatal(err)
	}
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM proxy_subscription_nodes WHERE subscription_id = ?",
		missingSourceID,
	).Scan(&relationCount); err != nil {
		t.Fatal(err)
	}
	if newNodeCount != 0 || relationCount != 0 {
		t.Fatalf(
			"missing subscription sync changed database: nodes=%d relations=%d",
			newNodeCount,
			relationCount,
		)
	}

	if _, err := SyncSubscriptionNodes(missingSourceID, nil); err == nil {
		t.Fatal("expected empty sync for missing subscription to fail")
	}
	assertOnlyNodeURI(t, existingURI)
}

func TestSubscriptionDeleteFailureRollsBackMemoryAndDatabase(t *testing.T) {
	setupFailureTestDB(t, "delete-rollback.db")
	subscription := createFailureTestSubscription(t)
	proxyURI := "http://9.9.9.9:8200"
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{{
		Type: "http", Name: "kept", RawURI: proxyURI,
	}}); err != nil {
		t.Fatal(err)
	}
	RecordTest(proxyURI, true, 18.5, "")
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_subscription_delete
		BEFORE DELETE ON proxy_subscriptions
		BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := DeleteProxySubscriptionAndNodes(subscription.ID); err == nil {
		t.Fatal("expected injected delete transaction failure")
	}
	assertOnlyNodeURI(t, proxyURI)
	if _, err := GetProxySubscription(subscription.ID); err != nil {
		t.Fatalf("subscription disappeared after rollback: %v", err)
	}
	var relationCount int
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM proxy_subscription_nodes WHERE subscription_id = ? AND raw_uri = ?",
		subscription.ID,
		proxyURI,
	).Scan(&relationCount); err != nil {
		t.Fatal(err)
	}
	if relationCount != 1 {
		t.Fatalf("subscription relation disappeared after rollback: %d", relationCount)
	}
	health := LoadHealth()[proxyURI]
	if health == nil || health.SuccessCount != 1 || health.LastTestMs != 18.5 {
		t.Fatalf("node health disappeared after rollback: %#v", health)
	}
}

func TestMergeFailureRollsBackMemory(t *testing.T) {
	setupFailureTestDB(t, "merge-rollback.db")
	keptURI := "http://8.8.4.4:8300"
	if err := MergeNodes([]Node{{Type: "http", Name: "kept", RawURI: keptURI}}); err != nil {
		t.Fatal(err)
	}
	orphanURI := "http://orphan.invalid:8302"
	mu.Lock()
	healthMap[orphanURI] = &NodeHealth{SuccessCount: 99}
	mu.Unlock()
	failedURI := "http://1.0.0.1:8301"
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_node_insert
		BEFORE INSERT ON nodes WHEN NEW.raw_uri = '` + failedURI + `'
		BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := MergeNodes([]Node{{Type: "http", Name: "failed", RawURI: failedURI}}); err == nil {
		t.Fatal("expected injected node insert failure")
	}
	assertOnlyNodeURI(t, keptURI)
	orphanHealth := LoadHealth()[orphanURI]
	if orphanHealth == nil || orphanHealth.SuccessCount != 99 {
		t.Fatalf("orphan health was not restored after rollback: %#v", orphanHealth)
	}
	var failedCount int
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM nodes WHERE raw_uri = ?",
		failedURI,
	).Scan(&failedCount); err != nil {
		t.Fatal(err)
	}
	if failedCount != 0 {
		t.Fatalf("failed node was persisted: %d", failedCount)
	}
}

func TestMergeNodesManualizesSubscriptionNodeWithoutDroppingOwnership(t *testing.T) {
	setupFailureTestDB(t, "merge-manualize.db")
	subscription := createFailureTestSubscription(t)
	subscriptionURI := "http://9.9.9.9:8350"
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{{
		Type: "http", Name: "subscription", RawURI: subscriptionURI,
	}}); err != nil {
		t.Fatal(err)
	}
	manualURI := "http://8.8.8.8:8351"
	if err := MergeNodes([]Node{
		{Type: "http", Name: "manual overlap", RawURI: subscriptionURI},
		{Type: "http", Name: "manual new", RawURI: manualURI},
	}); err != nil {
		t.Fatal(err)
	}

	loaded := LoadNodes()
	byURI := make(map[string]Node, len(loaded))
	for _, node := range loaded {
		byURI[node.RawURI] = node
	}
	if byURI[subscriptionURI].SourceID != 0 ||
		byURI[subscriptionURI].SubscriptionSourceCount != 1 {
		t.Fatalf("overlapping node ownership changed unexpectedly: %#v", byURI[subscriptionURI])
	}
	var sourceID int64
	var relationCount int
	if err := db.CurrentDB().QueryRow(
		"SELECT source_id FROM nodes WHERE raw_uri = ?",
		subscriptionURI,
	).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM proxy_subscription_nodes WHERE subscription_id = ? AND raw_uri = ?",
		subscription.ID,
		subscriptionURI,
	).Scan(&relationCount); err != nil {
		t.Fatal(err)
	}
	if sourceID != 0 || relationCount != 1 {
		t.Fatalf("persisted overlap source_id=%d relation_count=%d", sourceID, relationCount)
	}
	var manualSortOrder int
	if err := db.CurrentDB().QueryRow(
		"SELECT sort_order FROM nodes WHERE raw_uri = ?",
		manualURI,
	).Scan(&manualSortOrder); err != nil {
		t.Fatal(err)
	}
	if manualSortOrder != 1 {
		t.Fatalf("new manual node sort_order=%d, want 1", manualSortOrder)
	}
}

func TestMergeManualizationFailureRollsBackMemory(t *testing.T) {
	setupFailureTestDB(t, "merge-manualize-rollback.db")
	subscription := createFailureTestSubscription(t)
	proxyURI := "http://9.9.9.9:8360"
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{{
		Type: "http", Name: "subscription", RawURI: proxyURI,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_manualize
		BEFORE UPDATE OF source_id ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected manualize failure'); END`); err != nil {
		t.Fatal(err)
	}

	insertedURI := "http://8.8.4.4:8361"
	if err := MergeNodes([]Node{
		{Type: "http", Name: "manual", RawURI: proxyURI},
		{Type: "http", Name: "new", RawURI: insertedURI},
	}); err == nil {
		t.Fatal("expected injected manualization failure")
	}
	loaded := LoadNodes()
	if len(loaded) != 1 || loaded[0].SourceID != subscription.ID {
		t.Fatalf("failed manualization changed memory: %#v", loaded)
	}
	var sourceID int64
	if err := db.CurrentDB().QueryRow(
		"SELECT source_id FROM nodes WHERE raw_uri = ?",
		proxyURI,
	).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	if sourceID != subscription.ID {
		t.Fatalf("failed manualization changed database source_id=%d", sourceID)
	}
	var insertedCount int
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM nodes WHERE raw_uri = ?",
		insertedURI,
	).Scan(&insertedCount); err != nil {
		t.Fatal(err)
	}
	if insertedCount != 0 {
		t.Fatalf("failed manualization persisted new node: %d", insertedCount)
	}
}

func TestSortNodesPersistsChangedPositionsAndRollsBackOnFailure(t *testing.T) {
	setupFailureTestDB(t, "sort-order.db")
	firstURI := "http://8.8.8.8:8370"
	secondURI := "http://8.8.4.4:8371"
	untestedURI := "http://1.1.1.1:8372"
	if err := MergeNodes([]Node{
		{Type: "http", Name: "first", RawURI: firstURI},
		{Type: "http", Name: "second", RawURI: secondURI},
		{Type: "http", Name: "untested", RawURI: untestedURI},
	}); err != nil {
		t.Fatal(err)
	}
	RecordTest(firstURI, true, 100, "")
	RecordTest(secondURI, true, 10, "")

	SortNodesByLatency()
	wantOrder := []string{secondURI, firstURI, untestedURI}
	assertNodeOrder(t, wantOrder)

	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_sort_order
		BEFORE UPDATE OF sort_order ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected sort failure'); END`); err != nil {
		t.Fatal(err)
	}
	SortNodesByLatencyDesc()
	assertNodeOrder(t, wantOrder)
}

func TestReplaceManualNodesIsAtomicAndPreservesSubscriptionNodes(t *testing.T) {
	setupFailureTestDB(t, "replace-manual.db")
	subscription := createFailureTestSubscription(t)
	subscriptionURI := "http://9.9.9.9:8400"
	oldManualURI := "http://8.8.8.8:8401"
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{{
		Type: "http", Name: "subscription", RawURI: subscriptionURI,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := MergeNodes([]Node{{Type: "http", Name: "old manual", RawURI: oldManualURI}}); err != nil {
		t.Fatal(err)
	}

	newManualURI := "http://1.1.1.1:8402"
	if err := ReplaceManualNodes([]Node{{
		Type: "http", Name: "new manual", RawURI: newManualURI,
	}}); err != nil {
		t.Fatal(err)
	}
	loadedNodes := LoadNodes()
	if len(loadedNodes) != 2 {
		t.Fatalf("expected subscription + replacement manual nodes, got %#v", loadedNodes)
	}
	seen := map[string]Node{}
	for _, node := range loadedNodes {
		seen[node.RawURI] = node
	}
	if _, ok := seen[oldManualURI]; ok {
		t.Fatalf("old manual node survived replacement: %#v", loadedNodes)
	}
	if seen[subscriptionURI].SubscriptionSourceCount != 1 || seen[newManualURI].SourceID != 0 {
		t.Fatalf("ownership changed during manual replacement: %#v", loadedNodes)
	}
	item, err := GetProxySubscription(subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.NodeCount != 1 {
		t.Fatalf("subscription node_count changed during manual replacement: %#v", item)
	}
	var persistedManuals int
	if err = db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM nodes WHERE raw_uri IN (?, ?)",
		oldManualURI,
		newManualURI,
	).Scan(&persistedManuals); err != nil {
		t.Fatal(err)
	}
	if persistedManuals != 1 {
		t.Fatalf("persisted manual replacement count=%d, want 1", persistedManuals)
	}

	failedURI := "http://1.0.0.1:8403"
	if _, err = db.CurrentDB().Exec(`CREATE TRIGGER fail_manual_replace
		BEFORE INSERT ON nodes WHEN NEW.raw_uri = '` + failedURI + `'
		BEGIN SELECT RAISE(ABORT, 'injected replacement failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = ReplaceManualNodes([]Node{{
		Type: "http", Name: "failed", RawURI: failedURI,
	}}); err == nil {
		t.Fatal("expected injected replacement failure")
	}
	loadedNodes = LoadNodes()
	seen = map[string]Node{}
	for _, node := range loadedNodes {
		seen[node.RawURI] = node
	}
	if len(loadedNodes) != 2 || seen[newManualURI].RawURI == "" || seen[subscriptionURI].RawURI == "" {
		t.Fatalf("failed replacement did not restore memory: %#v", loadedNodes)
	}
	var failedPersisted, replacementPersisted int
	if err = db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM nodes WHERE raw_uri = ?",
		failedURI,
	).Scan(&failedPersisted); err != nil {
		t.Fatal(err)
	}
	if err = db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM nodes WHERE raw_uri = ?",
		newManualURI,
	).Scan(&replacementPersisted); err != nil {
		t.Fatal(err)
	}
	if failedPersisted != 0 || replacementPersisted != 1 {
		t.Fatalf(
			"failed replacement changed database: failed=%d replacement=%d",
			failedPersisted,
			replacementPersisted,
		)
	}
}

func TestReplaceManualNodesPersistsIndexedUpdatesAndPositions(t *testing.T) {
	setupFailureTestDB(t, "replace-manual-indexed.db")
	oldManualURI := "http://8.8.8.8:8410"
	if err := MergeNodes([]Node{{
		Type: "http", Name: "old manual", RawURI: oldManualURI,
	}}); err != nil {
		t.Fatal(err)
	}

	subscription := createFailureTestSubscription(t)
	overlapURI := "http://9.9.9.9:8411"
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{{
		Type: "http", Name: "subscription", RawURI: overlapURI,
	}}); err != nil {
		t.Fatal(err)
	}

	if err := ReplaceManualNodes([]Node{{
		Type: "http", Name: "manual overlap", RawURI: overlapURI,
	}}); err != nil {
		t.Fatal(err)
	}
	loaded := LoadNodes()
	if len(loaded) != 1 || loaded[0].RawURI != overlapURI || loaded[0].Name != "manual overlap" ||
		loaded[0].SourceID != 0 || loaded[0].SubscriptionSourceCount != 1 {
		t.Fatalf("indexed manual replacement state=%#v", loaded)
	}

	var persistedName string
	var sourceID int64
	var sortOrder int
	if err := db.CurrentDB().QueryRow(
		"SELECT name, source_id, sort_order FROM nodes WHERE raw_uri = ?",
		overlapURI,
	).Scan(&persistedName, &sourceID, &sortOrder); err != nil {
		t.Fatal(err)
	}
	if persistedName != "manual overlap" || sourceID != 0 || sortOrder != 0 {
		t.Fatalf(
			"persisted indexed replacement name=%q source=%d order=%d",
			persistedName,
			sourceID,
			sortOrder,
		)
	}
	var oldCount, relationCount int
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM nodes WHERE raw_uri = ?",
		oldManualURI,
	).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM proxy_subscription_nodes WHERE subscription_id = ? AND raw_uri = ?",
		subscription.ID,
		overlapURI,
	).Scan(&relationCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 0 || relationCount != 1 {
		t.Fatalf("indexed replacement old_count=%d relation_count=%d", oldCount, relationCount)
	}
}

func TestDedupNodesMigratesSubscriptionMembershipsAndCounts(t *testing.T) {
	setupFailureTestDB(t, "dedup-memberships.db")
	firstSubscription := createFailureTestSubscription(t)
	secondSubscription, err := SaveProxySubscription(ProxySubscription{
		Name: "dedup-second", URL: "https://example.com/dedup-second", ProxyType: "vless",
		RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstURI := "vless://uuid@example.com:443?security=tls#first"
	secondURI := "vless://uuid@example.com:443?security=tls#second"
	if _, err = SyncSubscriptionNodesAndMarkRefreshed(firstSubscription.ID, []Node{
		{Type: "vless", Name: "first", RawURI: firstURI},
		{Type: "vless", Name: "second", RawURI: secondURI},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = SyncSubscriptionNodesAndMarkRefreshed(secondSubscription.ID, []Node{{
		Type: "vless", Name: "shared", RawURI: secondURI,
	}}); err != nil {
		t.Fatal(err)
	}

	if removed := DedupNodes(); removed != 1 {
		t.Fatalf("DedupNodes() removed=%d, want 1", removed)
	}
	assertOnlyNodeURI(t, firstURI)
	for _, sourceID := range []int64{firstSubscription.ID, secondSubscription.ID} {
		var relationCount, nodeCount int
		if err = db.CurrentDB().QueryRow(
			`SELECT COUNT(*) FROM proxy_subscription_nodes
			 WHERE subscription_id = ? AND raw_uri = ?`,
			sourceID,
			firstURI,
		).Scan(&relationCount); err != nil {
			t.Fatal(err)
		}
		if err = db.CurrentDB().QueryRow(
			"SELECT node_count FROM proxy_subscriptions WHERE id = ?",
			sourceID,
		).Scan(&nodeCount); err != nil {
			t.Fatal(err)
		}
		if relationCount != 1 || nodeCount != 1 {
			t.Fatalf(
				"subscription %d after dedup: relation=%d node_count=%d",
				sourceID,
				relationCount,
				nodeCount,
			)
		}
	}
	var removedRelations int
	if err = db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM proxy_subscription_nodes WHERE raw_uri = ?",
		secondURI,
	).Scan(&removedRelations); err != nil {
		t.Fatal(err)
	}
	if removedRelations != 0 {
		t.Fatalf("removed URI still has %d persisted memberships", removedRelations)
	}
}

func TestDedupNodesFailureRollsBackMembershipsAndMemory(t *testing.T) {
	setupFailureTestDB(t, "dedup-rollback.db")
	subscription := createFailureTestSubscription(t)
	firstURI := "vless://uuid@example.com:443?security=tls#first"
	secondURI := "vless://uuid@example.com:443?security=tls#second"
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{
		{Type: "vless", Name: "first", RawURI: firstURI},
		{Type: "vless", Name: "second", RawURI: secondURI},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_dedup_delete
		BEFORE DELETE ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected dedup failure'); END`); err != nil {
		t.Fatal(err)
	}

	if removed := DedupNodes(); removed != 0 {
		t.Fatalf("failed DedupNodes() removed=%d, want 0", removed)
	}
	loaded := LoadNodes()
	if len(loaded) != 2 {
		t.Fatalf("failed dedup changed memory: %#v", loaded)
	}
	var nodeCount, relationCount, subscriptionCount int
	if err := db.CurrentDB().QueryRow("SELECT COUNT(*) FROM nodes").Scan(&nodeCount); err != nil {
		t.Fatal(err)
	}
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM proxy_subscription_nodes WHERE subscription_id = ?",
		subscription.ID,
	).Scan(&relationCount); err != nil {
		t.Fatal(err)
	}
	if err := db.CurrentDB().QueryRow(
		"SELECT node_count FROM proxy_subscriptions WHERE id = ?",
		subscription.ID,
	).Scan(&subscriptionCount); err != nil {
		t.Fatal(err)
	}
	if nodeCount != 2 || relationCount != 2 || subscriptionCount != 2 {
		t.Fatalf(
			"failed dedup changed database: nodes=%d relations=%d node_count=%d",
			nodeCount,
			relationCount,
			subscriptionCount,
		)
	}
}

func TestDeletingSubscriptionNodesUpdatesPersistedNodeCount(t *testing.T) {
	setupFailureTestDB(t, "node-count.db")
	subscription := createFailureTestSubscription(t)
	firstURI := "http://8.8.8.8:8500"
	secondURI := "http://1.1.1.1:8501"
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{
		{Type: "http", Name: "first", RawURI: firstURI},
		{Type: "http", Name: "second", RawURI: secondURI},
	}); err != nil {
		t.Fatal(err)
	}
	DeleteNode(firstURI)
	item, err := GetProxySubscription(subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.NodeCount != 1 {
		t.Fatalf("single delete left stale node_count: %#v", item)
	}
	if err = BatchDeleteNodes([]string{secondURI}); err != nil {
		t.Fatal(err)
	}
	item, err = GetProxySubscription(subscription.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.NodeCount != 0 {
		t.Fatalf("batch delete left stale node_count: %#v", item)
	}
}

func TestDeleteNodeMiddlePositionRollsBackAndRepairsIndex(t *testing.T) {
	setupFailureTestDB(t, "delete-middle-index.db")
	firstURI := "http://8.8.8.8:8550"
	middleURI := "http://8.8.4.4:8551"
	lastURI := "http://1.1.1.1:8552"
	if err := MergeNodes([]Node{
		{Type: "http", Name: "first", RawURI: firstURI},
		{Type: "http", Name: "middle", RawURI: middleURI},
		{Type: "http", Name: "last", RawURI: lastURI},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_middle_delete
		BEFORE DELETE ON nodes WHEN OLD.raw_uri = '` + middleURI + `'
		BEGIN SELECT RAISE(ABORT, 'injected middle delete failure'); END`); err != nil {
		t.Fatal(err)
	}

	DeleteNode(middleURI)
	assertNodeOrder(t, []string{firstURI, middleURI, lastURI})
	mu.RLock()
	middleIndex := nodeIndexByURI[middleURI]
	lastIndex := nodeIndexByURI[lastURI]
	mu.RUnlock()
	if middleIndex != 1 || lastIndex != 2 {
		t.Fatalf("failed deletion damaged index: middle=%d last=%d", middleIndex, lastIndex)
	}

	if _, err := db.CurrentDB().Exec("DROP TRIGGER fail_middle_delete"); err != nil {
		t.Fatal(err)
	}
	DeleteNode(middleURI)
	loaded := LoadNodes()
	if len(loaded) != 2 || loaded[0].RawURI != firstURI || loaded[1].RawURI != lastURI {
		t.Fatalf("successful middle deletion damaged order: %#v", loaded)
	}
	mu.RLock()
	_, middleExists := nodeIndexByURI[middleURI]
	lastIndex = nodeIndexByURI[lastURI]
	mu.RUnlock()
	if middleExists || lastIndex != 1 {
		t.Fatalf("successful deletion left stale index: middle=%v last=%d", middleExists, lastIndex)
	}
	var persistedMiddle int
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM nodes WHERE raw_uri = ?",
		middleURI,
	).Scan(&persistedMiddle); err != nil {
		t.Fatal(err)
	}
	if persistedMiddle != 0 {
		t.Fatalf("successful deletion left persisted middle node: %d", persistedMiddle)
	}
}

func TestDeleteDisabledSparsePositionsRollsBackAndRepairsIndex(t *testing.T) {
	setupFailureTestDB(t, "delete-disabled-sparse.db")
	uris := []string{
		"http://8.8.8.8:8560",
		"http://8.8.4.4:8561",
		"http://1.1.1.1:8562",
		"http://1.0.0.1:8563",
	}
	input := make([]Node, len(uris))
	for index, rawURI := range uris {
		input[index] = Node{Type: "http", Name: fmt.Sprintf("node-%d", index), RawURI: rawURI}
	}
	if err := MergeNodes(input); err != nil {
		t.Fatal(err)
	}
	if err := BatchUpdateNodesDisabled([]string{uris[1], uris[3]}, true); err != nil {
		t.Fatal(err)
	}
	RecordTest(uris[1], true, 17.5, "")
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_sparse_disabled_delete
		BEFORE DELETE ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected sparse disabled delete failure'); END`); err != nil {
		t.Fatal(err)
	}

	if removed := DeleteDisabled(); removed != 0 {
		t.Fatalf("failed sparse disabled deletion reported %d removals", removed)
	}
	assertNodeOrder(t, uris)
	loaded := LoadNodes()
	if !loaded[1].Disabled || !loaded[3].Disabled {
		t.Fatalf("failed sparse deletion lost disabled state: %#v", loaded)
	}
	health := LoadHealth()[uris[1]]
	if health == nil || health.SuccessCount != 1 || health.LastTestMs != 17.5 {
		t.Fatalf("failed sparse deletion lost health: %#v", health)
	}

	if _, err := db.CurrentDB().Exec("DROP TRIGGER fail_sparse_disabled_delete"); err != nil {
		t.Fatal(err)
	}
	if removed := DeleteDisabled(); removed != 2 {
		t.Fatalf("successful sparse disabled deletion removed %d nodes", removed)
	}
	loaded = LoadNodes()
	if len(loaded) != 2 || loaded[0].RawURI != uris[0] || loaded[1].RawURI != uris[2] {
		t.Fatalf("successful sparse deletion damaged order: %#v", loaded)
	}
	mu.RLock()
	_, firstRemovedExists := nodeIndexByURI[uris[1]]
	_, secondRemovedExists := nodeIndexByURI[uris[3]]
	keptIndex := nodeIndexByURI[uris[2]]
	mu.RUnlock()
	if firstRemovedExists || secondRemovedExists || keptIndex != 1 {
		t.Fatalf(
			"successful sparse deletion left stale indexes: first=%v second=%v kept=%d",
			firstRemovedExists,
			secondRemovedExists,
			keptIndex,
		)
	}
	if LoadHealth()[uris[1]] != nil {
		t.Fatal("successful sparse deletion retained removed health")
	}
}

func TestBatchDeleteSparsePositionsRollsBackAndRepairsIndex(t *testing.T) {
	setupFailureTestDB(t, "batch-delete-sparse.db")
	uris := []string{
		"http://8.8.8.8:8570",
		"http://8.8.4.4:8571",
		"http://1.1.1.1:8572",
		"http://1.0.0.1:8573",
	}
	input := make([]Node, len(uris))
	for index, rawURI := range uris {
		input[index] = Node{Type: "http", Name: fmt.Sprintf("node-%d", index), RawURI: rawURI}
	}
	if err := MergeNodes(input); err != nil {
		t.Fatal(err)
	}
	RecordTest(uris[1], true, 19.5, "")
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_sparse_batch_delete
		BEFORE DELETE ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected sparse batch delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	targets := []string{uris[1], uris[3], uris[1], "missing-uri"}

	if err := BatchDeleteNodes(targets); err == nil {
		t.Fatal("expected sparse batch deletion failure")
	}
	assertNodeOrder(t, uris)
	health := LoadHealth()[uris[1]]
	if health == nil || health.SuccessCount != 1 || health.LastTestMs != 19.5 {
		t.Fatalf("failed sparse batch deletion lost health: %#v", health)
	}
	mu.RLock()
	firstIndex := nodeIndexByURI[uris[1]]
	secondIndex := nodeIndexByURI[uris[3]]
	mu.RUnlock()
	if firstIndex != 1 || secondIndex != 3 {
		t.Fatalf("failed sparse batch deletion damaged indexes: first=%d second=%d", firstIndex, secondIndex)
	}

	if _, err := db.CurrentDB().Exec("DROP TRIGGER fail_sparse_batch_delete"); err != nil {
		t.Fatal(err)
	}
	if err := BatchDeleteNodes(targets); err != nil {
		t.Fatal(err)
	}
	loaded := LoadNodes()
	if len(loaded) != 2 || loaded[0].RawURI != uris[0] || loaded[1].RawURI != uris[2] {
		t.Fatalf("successful sparse batch deletion damaged order: %#v", loaded)
	}
	mu.RLock()
	_, firstRemovedExists := nodeIndexByURI[uris[1]]
	_, secondRemovedExists := nodeIndexByURI[uris[3]]
	keptIndex := nodeIndexByURI[uris[2]]
	mu.RUnlock()
	if firstRemovedExists || secondRemovedExists || keptIndex != 1 {
		t.Fatalf(
			"successful sparse batch deletion left stale indexes: first=%v second=%v kept=%d",
			firstRemovedExists,
			secondRemovedExists,
			keptIndex,
		)
	}
	if LoadHealth()[uris[1]] != nil {
		t.Fatal("successful sparse batch deletion retained removed health")
	}
	var persistedRemoved int
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM nodes WHERE raw_uri IN (?, ?)",
		uris[1],
		uris[3],
	).Scan(&persistedRemoved); err != nil {
		t.Fatal(err)
	}
	if persistedRemoved != 0 {
		t.Fatalf("successful sparse batch deletion retained %d database rows", persistedRemoved)
	}
}

func TestBatchMutationFailuresReturnErrorsAndRollbackMemory(t *testing.T) {
	setupFailureTestDB(t, "batch-rollback.db")
	subscription := createFailureTestSubscription(t)
	proxyURI := "http://8.8.4.4:8600"
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{{
		Type: "http", Name: "batch", RawURI: proxyURI,
	}}); err != nil {
		t.Fatal(err)
	}
	RecordTest(proxyURI, true, 22.5, "")
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_batch_update
		BEFORE UPDATE OF disabled ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected update failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := BatchUpdateNodesDisabled([]string{proxyURI, proxyURI, "missing-uri"}, false); err != nil {
		t.Fatalf("no-op batch update should not reach database trigger: %v", err)
	}
	if err := BatchUpdateNodesDisabled([]string{proxyURI}, true); err == nil {
		t.Fatal("expected injected batch update failure")
	}
	if loadedNodes := LoadNodes(); len(loadedNodes) != 1 || loadedNodes[0].Disabled {
		t.Fatalf("failed batch update changed memory: %#v", loadedNodes)
	}
	if _, err := db.CurrentDB().Exec("DROP TRIGGER fail_batch_update"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_batch_delete
		BEFORE DELETE ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	DeleteNode(proxyURI)
	assertOnlyNodeURI(t, proxyURI)

	if err := BatchUpdateNodesDisabled([]string{proxyURI, proxyURI, "missing-uri"}, true); err != nil {
		t.Fatal(err)
	}
	if removed := DeleteDisabled(); removed != 0 {
		t.Fatalf("failed disabled-node delete reported %d removals", removed)
	}
	assertOnlyNodeURI(t, proxyURI)

	if err := BatchDeleteNodes([]string{proxyURI}); err == nil {
		t.Fatal("expected injected batch delete failure")
	}
	assertOnlyNodeURI(t, proxyURI)
	var relationCount, nodeCount int
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM proxy_subscription_nodes WHERE subscription_id = ? AND raw_uri = ?",
		subscription.ID,
		proxyURI,
	).Scan(&relationCount); err != nil {
		t.Fatal(err)
	}
	if err := db.CurrentDB().QueryRow(
		"SELECT node_count FROM proxy_subscriptions WHERE id = ?",
		subscription.ID,
	).Scan(&nodeCount); err != nil {
		t.Fatal(err)
	}
	health := LoadHealth()[proxyURI]
	if relationCount != 1 || nodeCount != 1 ||
		health == nil || health.SuccessCount != 1 || health.LastTestMs != 22.5 {
		t.Fatalf(
			"failed batch deletes lost runtime state: relation=%d count=%d health=%#v",
			relationCount,
			nodeCount,
			health,
		)
	}
}

func setupFailureTestDB(t *testing.T, name string) {
	t.Helper()
	db.CloseDB()
	if err := db.InitDB(filepath.Join(t.TempDir(), name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.CloseDB)
	resetState()
}

func createFailureTestSubscription(t *testing.T) ProxySubscription {
	t.Helper()
	item, err := SaveProxySubscription(ProxySubscription{
		Name: "failure-test", URL: "https://example.com/list", ProxyType: "http",
		RefreshIntervalMinutes: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func assertOnlyNodeURI(t *testing.T, rawURI string) {
	t.Helper()
	loadedNodes := LoadNodes()
	if len(loadedNodes) != 1 || loadedNodes[0].RawURI != rawURI {
		t.Fatalf("unexpected in-memory nodes: %#v", loadedNodes)
	}
	var persistedCount int
	if err := db.CurrentDB().QueryRow(
		"SELECT COUNT(*) FROM nodes WHERE raw_uri = ?",
		rawURI,
	).Scan(&persistedCount); err != nil {
		t.Fatal(err)
	}
	if persistedCount != 1 {
		t.Fatalf("expected persisted node %q, count=%d", rawURI, persistedCount)
	}
}

func assertNodeOrder(t *testing.T, want []string) {
	t.Helper()
	loaded := LoadNodes()
	if len(loaded) != len(want) {
		t.Fatalf("in-memory node count=%d, want %d: %#v", len(loaded), len(want), loaded)
	}
	for index, rawURI := range want {
		if loaded[index].RawURI != rawURI {
			t.Fatalf("in-memory order[%d]=%q, want %q", index, loaded[index].RawURI, rawURI)
		}
		var persistedURI string
		if err := db.CurrentDB().QueryRow(
			"SELECT raw_uri FROM nodes WHERE sort_order = ?",
			index,
		).Scan(&persistedURI); err != nil {
			t.Fatal(err)
		}
		if persistedURI != rawURI {
			t.Fatalf("persisted order[%d]=%q, want %q", index, persistedURI, rawURI)
		}
	}
}

func waitForPersistedHealth(t *testing.T, rawURI string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var successCount int
		err := db.CurrentDB().QueryRow(
			"SELECT success_count FROM node_health WHERE raw_uri = ?",
			rawURI,
		).Scan(&successCount)
		if err == nil && successCount == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("health did not persist before deadline: err=%v count=%d", err, successCount)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
