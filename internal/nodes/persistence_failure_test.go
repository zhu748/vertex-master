package nodes

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/db"
)

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

func TestSubscriptionDeleteFailureRollsBackMemoryAndDatabase(t *testing.T) {
	setupFailureTestDB(t, "delete-rollback.db")
	subscription := createFailureTestSubscription(t)
	proxyURI := "http://9.9.9.9:8200"
	if _, err := SyncSubscriptionNodesAndMarkRefreshed(subscription.ID, []Node{{
		Type: "http", Name: "kept", RawURI: proxyURI,
	}}); err != nil {
		t.Fatal(err)
	}
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
}

func TestMergeFailureRollsBackMemory(t *testing.T) {
	setupFailureTestDB(t, "merge-rollback.db")
	keptURI := "http://8.8.4.4:8300"
	if err := MergeNodes([]Node{{Type: "http", Name: "kept", RawURI: keptURI}}); err != nil {
		t.Fatal(err)
	}
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

func TestBatchMutationFailuresReturnErrorsAndRollbackMemory(t *testing.T) {
	setupFailureTestDB(t, "batch-rollback.db")
	proxyURI := "http://8.8.4.4:8600"
	if err := MergeNodes([]Node{{Type: "http", Name: "batch", RawURI: proxyURI}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CurrentDB().Exec(`CREATE TRIGGER fail_batch_update
		BEFORE UPDATE OF disabled ON nodes
		BEGIN SELECT RAISE(ABORT, 'injected update failure'); END`); err != nil {
		t.Fatal(err)
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
	if err := BatchDeleteNodes([]string{proxyURI}); err == nil {
		t.Fatal("expected injected batch delete failure")
	}
	assertOnlyNodeURI(t, proxyURI)
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
