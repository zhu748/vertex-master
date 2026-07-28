package nodes

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

var benchmarkSelectedNodes []Node //nolint:gochecknoglobals

func resetState() {
	mu.Lock()
	defer mu.Unlock()
	nodeList = nil
	nodeIndexByURI = make(map[string]int)
	healthMap = make(map[string]*NodeHealth)
	subscriptionSources = make(map[string]map[int64]bool)
	loaded = false
	// 彻底清除物理磁盘缓存，防止测试间的数据污染
	_ = os.Remove(filepath.Join(config.ConfigDir(), "nodes.json"))
	_ = os.Remove(filepath.Join(config.ConfigDir(), "node_health.json"))
}

func BenchmarkWeightedNodeSample80Choose10(b *testing.B) {
	candidates := make([]scoredNode, 80)
	for index := range candidates {
		candidates[index] = scoredNode{
			node: Node{
				Name:   fmt.Sprintf("node-%d", index),
				RawURI: fmt.Sprintf("http://node-%d.invalid:8080", index),
			},
			score: float64(50 + index%25),
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkSelectedNodes = weightedNodeSample(candidates, 10)
	}
}

func BenchmarkSelectForParallelLargePool(b *testing.B) {
	const nodeCount = 5000
	nodes := make([]Node, nodeCount)
	health := make(map[string]*NodeHealth, nodeCount)
	sticky := NewStickyNodePool()
	for index := range nodes {
		uri := fmt.Sprintf("http://node-%d.invalid:8080", index)
		nodes[index] = Node{Name: fmt.Sprintf("node-%d", index), RawURI: uri}
		health[uri] = &NodeHealth{
			SuccessCount:  20 + index%50,
			LastTestMs:    float64(20 + index%200),
			LastSuccessAt: time.Now().Unix(),
		}
		if index%50 == 0 {
			sticky.Add(uri)
		}
	}

	mu.Lock()
	previousNodes, previousIndex, previousHealth, previousLoaded := nodeList, nodeIndexByURI, healthMap, loaded
	previousSticky := globalStickyPool
	nodeList, healthMap, loaded = nodes, health, true
	rebuildNodeIndexUnsafe()
	globalStickyPool = sticky
	mu.Unlock()
	b.Cleanup(func() {
		mu.Lock()
		nodeList, nodeIndexByURI, healthMap, loaded = previousNodes, previousIndex, previousHealth, previousLoaded
		globalStickyPool = previousSticky
		mu.Unlock()
	})

	b.Run("serial", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = SelectForParallel(10, 80, false, true)
		}
	})
	b.Run("parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = SelectForParallel(10, 80, false, true)
			}
		})
	})
}

func BenchmarkRecordSelectionLargePool(b *testing.B) {
	const nodeCount = 5000
	nodes := make([]Node, nodeCount)
	for index := range nodes {
		uri := fmt.Sprintf("http://record-node-%d.invalid:8080", index)
		nodes[index] = Node{Name: fmt.Sprintf("node-%d", index), RawURI: uri}
	}
	target := nodes[len(nodes)-1].RawURI

	mu.Lock()
	previousNodes, previousIndex, previousHealth, previousLoaded := nodeList, nodeIndexByURI, healthMap, loaded
	nodeList, healthMap, loaded = nodes, make(map[string]*NodeHealth), true
	rebuildNodeIndexUnsafe()
	mu.Unlock()
	b.Cleanup(func() {
		mu.Lock()
		nodeList, nodeIndexByURI, healthMap, loaded = previousNodes, previousIndex, previousHealth, previousLoaded
		mu.Unlock()
	})

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		RecordSelection(target)
	}
}

func TestWeightedNodeSampleIsUniqueBoundedAndDoesNotMutateInput(t *testing.T) {
	candidates := []scoredNode{
		{node: Node{Name: "one", RawURI: "http://one.invalid"}, score: 1},
		{node: Node{Name: "two", RawURI: "http://two.invalid"}, score: 20},
		{node: Node{Name: "three", RawURI: "http://three.invalid"}, score: 100},
	}
	original := append([]scoredNode(nil), candidates...)
	selected := weightedNodeSample(candidates, 10)
	if len(selected) != len(candidates) {
		t.Fatalf("selected length = %d, want %d", len(selected), len(candidates))
	}
	seen := make(map[string]bool, len(selected))
	for _, node := range selected {
		if seen[node.RawURI] {
			t.Fatalf("node selected more than once: %q", node.RawURI)
		}
		seen[node.RawURI] = true
	}
	for index := range candidates {
		if candidates[index] != original[index] {
			t.Fatalf("input candidate %d mutated: got %#v, want %#v", index, candidates[index], original[index])
		}
	}
	if got := weightedNodeSample(candidates, 0); got != nil {
		t.Fatalf("zero count returned %#v, want nil", got)
	}
}

func TestWeightedNodeSampleAboveInlineCapacity(t *testing.T) {
	const candidateCount = 128
	candidates := make([]scoredNode, candidateCount)
	for index := range candidates {
		uri := fmt.Sprintf("http://overflow-%d.invalid", index)
		candidates[index] = scoredNode{node: Node{Name: uri, RawURI: uri}, score: float64(index + 1)}
	}
	original := append([]scoredNode(nil), candidates...)
	selected := weightedNodeSample(candidates, 32)
	if len(selected) != 32 {
		t.Fatalf("selected length=%d, want 32", len(selected))
	}
	seen := make(map[string]bool, len(selected))
	for _, node := range selected {
		if seen[node.RawURI] {
			t.Fatalf("node selected more than once above inline capacity: %q", node.RawURI)
		}
		seen[node.RawURI] = true
	}
	for index := range candidates {
		if candidates[index] != original[index] {
			t.Fatalf("overflow input candidate %d mutated", index)
		}
	}
}

func TestRetainHighestScoredKeepsOnlyTopK(t *testing.T) {
	var retained []scoredNode
	for _, score := range []float64{4, 1, 9, 3, 8, 2, 10, 7, 5, 6} {
		retained = retainHighestScored(retained, scoredNode{
			node: Node{RawURI: fmt.Sprintf("score-%.0f", score)}, score: score,
		}, 4)
	}
	if len(retained) != 4 {
		t.Fatalf("retained=%d, want 4", len(retained))
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i].score > retained[j].score })
	for index, want := range []float64{10, 9, 8, 7} {
		if retained[index].score != want {
			t.Fatalf("retained[%d]=%v, want %v: %#v", index, retained[index].score, want, retained)
		}
	}
	if got := retainHighestScored(nil, scoredNode{score: 1}, 0); got != nil {
		t.Fatalf("zero limit retained %#v", got)
	}
}

func TestNodesLifecycle(t *testing.T) {
	// Setup a temporary directory for config
	_ = t.TempDir()

	// Temporarily override the behavior of fileDir if possible,
	// but since it's hardcoded to os.Executable() or "config",
	// we will create "config" in the current directory, or just mock what we can.
	// Since fileDir is fixed and we don't want to pollute actual config,
	// let's create a symlink or temporarily mock os.Executable if needed.
	// For simplicity, we just test the in-memory aspects mostly, and let it write to ./config
	// Note: In a real test environment, we should make fileDir overridable.
	// Update: fileDir() 已经被移除并重构为了 config.ConfigDir()，现在测试环境可以通过 VPROXY_CONFIG 环境变量轻松覆盖配置路径，从而避免污染真实配置。

	// We'll just test the logic that doesn't strictly depend on file system or clean up

	resetState()

	n1 := Node{RawURI: "uri1", Name: "node1"} //nolint:exhaustruct
	n2 := Node{RawURI: "uri2", Name: "node2"} //nolint:exhaustruct

	MergeNodes([]Node{n1, n2})

	nodes := LoadNodes()
	if len(nodes) != 2 {
		t.Fatalf("Expected 2 nodes, got %d", len(nodes))
	}

	// Test Dedup
	MergeNodes([]Node{n1}) // Add duplicate
	if len(LoadNodes()) != 2 {
		t.Fatalf("Expected 2 nodes after merging duplicate, got %d", len(LoadNodes()))
	}

	removed := DedupNodes()
	if removed != 0 {
		t.Errorf("Expected 0 removed during dedup, got %d", removed)
	}

	// Test RecordTest
	RecordTest("uri1", true, 10.5, "")
	health := LoadHealth()
	hUri1 := health["uri1"]
	if hUri1 == nil || hUri1.SuccessCount != 1 {
		t.Errorf("Expected success count 1, got %v", hUri1)
	}

	RecordTest("uri1", false, 0, "timeout")
	health = LoadHealth()
	hUri1 = health["uri1"]
	if hUri1 == nil || hUri1.FailCount != 1 {
		t.Errorf("Expected fail count 1, got %v", hUri1)
	}

	// Test BatchUpdateNodesDisabled
	BatchUpdateNodesDisabled([]string{"uri1"}, true)
	for _, n := range LoadNodes() {
		if n.RawURI == "uri1" && !n.Disabled {
			t.Errorf("Expected uri1 to be disabled")
		}
	}

	// Test SelectForParallel (uri1 is disabled, should only return uri2 if available)
	selected := SelectForParallel(2, 80, false, false)
	if len(selected) != 1 || selected[0].RawURI != "uri2" {
		t.Errorf("Expected only uri2 to be selected, got %v", selected)
	}

	// Test DeleteDisabled
	removed = DeleteDisabled()
	if removed != 1 {
		t.Errorf("Expected 1 node removed, got %d", removed)
	}
	if len(LoadNodes()) != 1 {
		t.Errorf("Expected 1 node remaining, got %d", len(LoadNodes()))
	}

	// Test DeleteNode
	DeleteNode("uri2")
	if len(LoadNodes()) != 0 {
		t.Errorf("Expected 0 nodes, got %d", len(LoadNodes()))
	}

	// Cleanup state
	resetState()
	_ = os.RemoveAll(filepath.Join(config.ConfigDir(), "nodes.json"))
	_ = os.RemoveAll(filepath.Join(config.ConfigDir(), "node_health.json"))
}

func TestParseNodeIdentity(t *testing.T) {
	tests := []struct { //nolint:govet
		name     string
		uri      string
		wantOK   bool
		wantS    string
		wantUI   string
		wantHost string
		wantPort int
	}{
		{"vmess", "vmess://eyJhZGQiOiIxMjcuMC4wLjEiLCJwb3J0Ijo4ODg4LCJpZCI6InV1aWQtdmFsdWUiLCJwcyI6InRlc3QifQ==", true, "vmess", "uuid-value", "127.0.0.1", 8888},
		{"ss", "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@127.0.0.1:8888", true, "ss", "aes-256-gcm:password", "127.0.0.1", 8888},
		{"vless", "vless://uuid@example.com:443", true, "vless", "uuid", "example.com", 443},
		{"trojan", "trojan://password@example.com:8443", true, "trojan", "password", "example.com", 8443},
		{"invalid", "not-a-uri://", false, "", "", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ui, host, port, ok := parseNodeIdentity(tt.uri)
			if ok != tt.wantOK {
				t.Errorf("parseNodeIdentity() ok = %v, want %v", ok, tt.wantOK)
			}
			if s != tt.wantS {
				t.Errorf("parseNodeIdentity() scheme = %q, want %q", s, tt.wantS)
			}
			if ui != tt.wantUI {
				t.Errorf("parseNodeIdentity() userinfo = %q, want %q", ui, tt.wantUI)
			}
			if host != tt.wantHost {
				t.Errorf("parseNodeIdentity() host = %q, want %q", host, tt.wantHost)
			}
			if port != tt.wantPort {
				t.Errorf("parseNodeIdentity() port = %d, want %d", port, tt.wantPort)
			}
		})
	}
}

func TestUpdateNodeTestResult(t *testing.T) {
	resetState()
	defer resetState()

	// Setup: one enabled node
	n1 := Node{RawURI: "uri1", Name: "node1"} //nolint:exhaustruct
	MergeNodes([]Node{n1})

	// Test: fail the node
	UpdateNodeTestResult("uri1", false, 100, "timeout")
	health := LoadHealth()
	h1 := health["uri1"]
	if h1 == nil || h1.ConsecutiveFailures != 1 {
		t.Errorf("Expected 1 consecutive failure")
	}
	nodes := LoadNodes()
	if len(nodes) != 1 || nodes[0].Disabled {
		t.Errorf("Expected node1 to NOT be disabled after failed test (cooldown replaces disable)")
	}
	if h1 == nil || h1.CooldownUntil == 0 {
		t.Errorf("Expected cooldown to be set after failed test")
	}

	// Test: succeed the node
	UpdateNodeTestResult("uri1", true, 50, "")
	health = LoadHealth()
	h2 := health["uri1"]
	if h2 == nil || h2.SuccessCount != 1 {
		t.Errorf("Expected 1 success")
	}
	if h2 == nil || h2.CooldownUntil != 0 {
		t.Errorf("Expected cooldown to be cleared after success")
	}
	nodes = LoadNodes()
	if len(nodes) == 0 || nodes[0].Disabled {
		t.Errorf("Expected node1 to be enabled after success")
	}
}

func TestMergeNodesPrunesHealthMap(t *testing.T) {
	resetState()
	defer resetState()

	n1 := Node{RawURI: "uri1", Name: "node1"} //nolint:exhaustruct
	n2 := Node{RawURI: "uri2", Name: "node2"} //nolint:exhaustruct

	MergeNodes([]Node{n1, n2})

	RecordTest("uri1", true, 10, "")
	RecordTest("uri2", false, 0, "timeout")
	health := LoadHealth()
	if len(health) != 2 {
		t.Fatalf("Expected 2 health entries, got %d", len(health))
	}

	DeleteNode("uri2")

	mu.Lock()
	healthMap["orphan-uri"] = &NodeHealth{SuccessCount: 99} //nolint:exhaustruct
	mu.Unlock()

	MergeNodes([]Node{n1})
	health = LoadHealth()
	if len(health) != 1 {
		t.Fatalf("Expected 1 health entry after MergeNodes prunes orphan, got %d", len(health))
	}
	if health["orphan-uri"] != nil {
		t.Errorf("Expected orphan-uri health entry to be pruned")
	}
	if health["uri1"] == nil {
		t.Errorf("Expected uri1 health entry to survive")
	}

	RecordTest("uri1", false, 0, "timeout")
	health = LoadHealth()
	if health["uri1"] == nil || health["uri1"].FailCount != 1 {
		t.Errorf("Expected RecordTest to still work after pruning, got %v", health["uri1"])
	}
}

func TestEnableNode(t *testing.T) {
	resetState()
	defer resetState()

	n1 := Node{RawURI: "uri1", Name: "node1", Disabled: true} //nolint:exhaustruct
	MergeNodes([]Node{n1})

	// Also set cooldown
	RecordTest("uri1", false, 0, "timeout")

	ok := EnableNode("uri1")
	if !ok {
		t.Errorf("Expected EnableNode to return true")
	}
	nodes := LoadNodes()
	if len(nodes) != 1 || nodes[0].Disabled {
		t.Errorf("Expected node1 to be enabled")
	}
	health := LoadHealth()
	if health["uri1"] != nil && health["uri1"].CooldownUntil != 0 {
		t.Errorf("Expected cooldown to be cleared")
	}

	// Test enabling non-existent node
	ok = EnableNode("nonexistent")
	if ok {
		t.Errorf("Expected EnableNode to return false for nonexistent node")
	}
}

func TestDedupNodesSemantic(t *testing.T) {
	resetState()
	defer resetState()

	// Two nodes with same identity but different raw URIs (different names/fragments)
	n1 := Node{RawURI: "vless://uuid@example.com:443?security=tls#name1", Name: "node1"}
	n2 := Node{RawURI: "vless://uuid@example.com:443?security=tls#name2", Name: "node2"}
	MergeNodes([]Node{n1, n2})

	removed := DedupNodes()
	if removed != 1 {
		t.Errorf("Expected 1 removed during semantic dedup, got %d", removed)
	}
	result := LoadNodes()
	if len(result) != 1 {
		t.Errorf("Expected 1 node after dedup, got %d", len(result))
	}
}

func TestSelectForParallelCooldownFallback(t *testing.T) {
	resetState()
	defer resetState()

	n1 := Node{RawURI: "uri1", Name: "node1"}
	n2 := Node{RawURI: "uri2", Name: "node2"}
	n3 := Node{RawURI: "uri3", Name: "node3"}
	MergeNodes([]Node{n1, n2, n3})

	// Put n1 and n2 in cooldown, leave n3 normal
	RecordTest("uri1", false, 0, "timeout")
	RecordTest("uri2", false, 0, "timeout")

	// 冷却中的失败节点不应为了凑满并发数而重新进入候选。
	selected := SelectForParallel(3, 80, false, false)
	if len(selected) != 1 || selected[0].RawURI != "uri3" {
		t.Errorf("Expected only the available node, got %#v", selected)
	}
}

func TestSelectForParallelAllCooldownKeepsEarliestK(t *testing.T) {
	resetState()
	defer resetState()

	now := time.Now().Unix()
	list := []Node{
		{RawURI: "late", Name: "late"},
		{RawURI: "early", Name: "early"},
		{RawURI: "middle", Name: "middle"},
	}
	if err := MergeNodes(list); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	healthMap["late"] = &NodeHealth{LastFailAt: now, CooldownUntil: now + 300, Last429At: now + 30}
	healthMap["early"] = &NodeHealth{LastFailAt: now, CooldownUntil: now + 100, Last429At: now + 10}
	healthMap["middle"] = &NodeHealth{LastFailAt: now, CooldownUntil: now + 200, Last429At: now + 20}
	mu.Unlock()

	selected := SelectForParallel(2, 80, false, false)
	if len(selected) != 2 || selected[0].RawURI != "early" || selected[1].RawURI != "middle" {
		t.Fatalf("cooldown fallback=%#v, want early then middle", selected)
	}
}

func TestSelectForParallelLimitsUntestedExploration(t *testing.T) {
	resetState()
	defer resetState()

	var list []Node
	for i := 0; i < 10; i++ {
		uri := fmt.Sprintf("healthy-%d", i)
		list = append(list, Node{RawURI: uri, Name: uri})
	}
	for i := 0; i < 10; i++ {
		uri := fmt.Sprintf("untested-%d", i)
		list = append(list, Node{RawURI: uri, Name: uri})
	}
	MergeNodes(list)
	for i := 0; i < 10; i++ {
		RecordTest(fmt.Sprintf("healthy-%d", i), true, 20, "")
	}

	selected := SelectForParallel(10, 80, false, false)
	untestedCount := 0
	for _, node := range selected {
		if strings.HasPrefix(node.RawURI, "untested-") {
			untestedCount++
		}
	}
	if untestedCount != 2 {
		t.Fatalf("expected 20%% exploration, got %d untested nodes in %#v", untestedCount, selected)
	}
}

func TestSelectForParallelAboveInlineCandidateCapacity(t *testing.T) {
	resetState()
	defer resetState()

	const nodeCount = 160
	now := time.Now().Unix()
	list := make([]Node, nodeCount)
	health := make(map[string]*NodeHealth, nodeCount)
	for index := range list {
		uri := fmt.Sprintf("healthy-%d", index)
		list[index] = Node{RawURI: uri, Name: uri}
		health[uri] = &NodeHealth{
			SuccessCount:  index + 1,
			LastSuccessAt: now,
		}
	}
	mu.Lock()
	nodeList, healthMap, loaded = list, health, true
	rebuildNodeIndexUnsafe()
	mu.Unlock()

	selected := SelectForParallel(20, 128, false, false)
	if len(selected) != 20 {
		t.Fatalf("selected %d nodes above inline capacity, want 20", len(selected))
	}
	seen := make(map[string]bool, len(selected))
	for _, node := range selected {
		if seen[node.RawURI] {
			t.Fatalf("selected duplicate node %q: %#v", node.RawURI, selected)
		}
		seen[node.RawURI] = true
	}
}

func TestHealthCountersDecayAtMutation(t *testing.T) {
	resetState()
	defer resetState()
	const uri = "decay-node"
	if err := MergeNodes([]Node{{RawURI: uri, Name: uri}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	healthMap[uri] = &NodeHealth{
		SuccessCount:   1000,
		FailCount:      10,
		RecentUseCount: 20,
	}
	mu.Unlock()

	RecordTest(uri, true, 10, "")
	health := LoadHealth()[uri]
	if health == nil || health.SuccessCount != 500 || health.FailCount != 5 || health.RecentUseCount != 10 {
		t.Fatalf("health counters were not decayed atomically: %#v", health)
	}
}

func TestSelectForParallelSharesReadLock(t *testing.T) {
	resetState()
	defer resetState()
	if err := MergeNodes([]Node{{RawURI: "read-node", Name: "read-node"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	loaded = true
	mu.Unlock()

	mu.RLock()
	done := make(chan []Node, 1)
	go func() {
		done <- SelectForParallel(1, 80, false, false)
	}()
	select {
	case selected := <-done:
		mu.RUnlock()
		if len(selected) != 1 || selected[0].RawURI != "read-node" {
			t.Fatalf("unexpected selection: %#v", selected)
		}
	case <-time.After(time.Second):
		mu.RUnlock()
		t.Fatal("read-only selection blocked behind another reader")
	}
}

func TestSelectForParallelConcurrentWithHealthUpdates(t *testing.T) {
	resetState()
	defer resetState()
	list := make([]Node, 100)
	for index := range list {
		uri := fmt.Sprintf("concurrent-%d", index)
		list[index] = Node{RawURI: uri, Name: uri}
	}
	if err := MergeNodes(list); err != nil {
		t.Fatal(err)
	}
	for _, node := range list {
		RecordTest(node.RawURI, true, 20, "")
	}
	mu.Lock()
	previousSticky := globalStickyPool
	globalStickyPool = NewStickyNodePool()
	mu.Unlock()
	defer func() {
		mu.Lock()
		globalStickyPool = previousSticky
		mu.Unlock()
	}()

	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := range 100 {
				if worker%2 == 0 {
					selected := SelectForParallel(5, 80, false, true)
					if len(selected) == 0 {
						t.Errorf("iteration %d returned no nodes", iteration)
						return
					}
				} else if worker == 7 {
					node := list[iteration%len(list)]
					if iteration%2 == 0 {
						globalStickyPool.Add(node.RawURI)
					} else {
						globalStickyPool.Evict(node.RawURI)
					}
				} else {
					node := list[(worker+iteration)%len(list)]
					RecordSelection(node.RawURI)
				}
			}
		}()
	}
	workers.Wait()
}

func TestNodeURIIndexSelfHealsAfterListMutation(t *testing.T) {
	resetState()
	defer resetState()

	mu.Lock()
	nodeList = []Node{
		{RawURI: "removed", Name: "removed"},
		{RawURI: "kept", Name: "kept"},
	}
	healthMap = make(map[string]*NodeHealth)
	loaded = true
	rebuildNodeIndexUnsafe()
	// 模拟删除/重排后尚未触发索引重建：kept 从下标 1 移到 0。
	nodeList = []Node{{RawURI: "kept", Name: "kept-renamed"}}
	mu.Unlock()

	RecordSelection("kept")
	RecordSelection("removed")
	health := LoadHealth()
	if health["kept"] == nil || health["kept"].RecentUseCount != 1 {
		t.Fatalf("kept node was not recorded after index rebuild: %#v", health["kept"])
	}
	if health["removed"] != nil {
		t.Fatalf("removed node created orphan health: %#v", health["removed"])
	}
	if got := GetNodeName("kept"); got != "kept-renamed" {
		t.Fatalf("indexed node name=%q", got)
	}
}

func TestSelectNodesForHealthCheckPriority(t *testing.T) {
	resetState()
	defer resetState()

	MergeNodes([]Node{
		{RawURI: "untested", Name: "untested"},
		{RawURI: "failed", Name: "failed"},
		{RawURI: "healthy", Name: "healthy"},
		{RawURI: "disabled", Name: "disabled", Disabled: true},
	})
	RecordTest("failed", false, 0, "timeout")
	RecordTest("healthy", true, 10, "")

	mu.Lock()
	healthMap["failed"].CooldownUntil = time.Now().Add(-time.Second).Unix()
	healthMap["healthy"].LastSuccessAt = time.Now().Add(-time.Hour).Unix()
	mu.Unlock()

	selected := SelectNodesForHealthCheck(3, 30*time.Minute, time.Now())
	if len(selected) != 3 ||
		selected[0].RawURI != "untested" ||
		selected[1].RawURI != "failed" ||
		selected[2].RawURI != "healthy" {
		t.Fatalf("unexpected health check priority: %#v", selected)
	}
}

func TestSafeNodeLabelIsSingleLineAndBounded(t *testing.T) {
	raw := "  safe\r\n\u001b[31mname\u202e" + strings.Repeat("长", 200)
	got := SafeNodeLabel(raw)
	if strings.ContainsAny(got, "\r\n\u001b") || strings.ContainsRune(got, '\u202e') {
		t.Fatalf("unsafe control characters remained in %q", got)
	}
	if len([]rune(got)) > 128 {
		t.Fatalf("safe node label exceeds 128 runes: %d", len([]rune(got)))
	}
	if got == "" {
		t.Fatal("safe node label must not be empty")
	}
}

func TestSafeNodeLabelPreservesCleanNameWithoutAllocation(t *testing.T) {
	const name = "benchmark-node-2047"
	if got := SafeNodeLabel(name); got != name {
		t.Fatalf("SafeNodeLabel(%q) = %q", name, got)
	}
	if allocations := testing.AllocsPerRun(100, func() {
		if got := SafeNodeLabel(name); got != name {
			t.Fatalf("SafeNodeLabel(%q) = %q", name, got)
		}
	}); allocations != 0 {
		t.Fatalf("clean node label allocated %.1f times", allocations)
	}
}
