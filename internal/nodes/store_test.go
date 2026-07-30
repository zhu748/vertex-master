package nodes

import (
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/db"
)

var (
	benchmarkSelectedNodes    []Node                 //nolint:gochecknoglobals
	benchmarkHealthPointers   map[string]*NodeHealth //nolint:gochecknoglobals
	benchmarkNodePoolStats    NodePoolStats          //nolint:gochecknoglobals
	benchmarkNodePoolSnapshot NodePoolSnapshot       //nolint:gochecknoglobals
	benchmarkNodePageSnapshot NodePoolPageSnapshot   //nolint:gochecknoglobals
	benchmarkNodeURIs         []string               //nolint:gochecknoglobals
	benchmarkIdentityScheme   string                 //nolint:gochecknoglobals
	benchmarkIdentityUser     string                 //nolint:gochecknoglobals
	benchmarkIdentityHost     string                 //nolint:gochecknoglobals
	benchmarkIdentityPort     int                    //nolint:gochecknoglobals
	benchmarkIdentityOK       bool                   //nolint:gochecknoglobals
)

func BenchmarkParseNodeIdentityBase64Variants(b *testing.B) {
	payload := []byte(`{"add":"vmess.example.com","port":443,"id":"12345678-1234-1234-1234-123456789012","ps":"demo"}`)
	credentials := []byte("aes-256-gcm:benchmark-password")
	for name, uri := range map[string]string{
		"vmess_standard": "vmess://" + base64.StdEncoding.EncodeToString(payload),
		"vmess_url_raw":  "vmess://" + base64.RawURLEncoding.EncodeToString(payload),
		"ss_standard": "ss://" + base64.StdEncoding.EncodeToString(credentials) +
			"@proxy.example.com:8388",
		"ss_url_raw": "ss://" + base64.RawURLEncoding.EncodeToString(credentials) +
			"@proxy.example.com:8388",
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				benchmarkIdentityScheme, benchmarkIdentityUser, benchmarkIdentityHost,
					benchmarkIdentityPort, benchmarkIdentityOK = parseNodeIdentity(uri)
				if !benchmarkIdentityOK || benchmarkIdentityScheme == "" {
					b.Fatal("identity was not parsed")
				}
			}
		})
	}
}

func resetState() {
	mu.Lock()
	defer mu.Unlock()
	nodeList = nil
	nodeIndexByURI = make(map[string]int)
	healthByNodeIndex = nil
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
		rebuildNodeHealthIndexUnsafe()
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
		rebuildNodeHealthIndexUnsafe()
		mu.Unlock()
	})

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		RecordSelection(target)
	}
}

func BenchmarkEnableNodeLargePool(b *testing.B) {
	const nodeCount = 5000
	nodes := make([]Node, nodeCount)
	health := make(map[string]*NodeHealth, 1)
	for index := range nodes {
		rawURI := fmt.Sprintf("http://enable-node-%d.invalid:8080", index)
		nodes[index] = Node{Name: fmt.Sprintf("node-%d", index), RawURI: rawURI}
	}
	target := nodes[len(nodes)-1].RawURI
	nodes[len(nodes)-1].Disabled = true
	health[target] = &NodeHealth{CooldownUntil: time.Now().Add(time.Minute).Unix()}

	mu.Lock()
	previousNodes, previousIndex, previousHealth, previousLoaded :=
		nodeList, nodeIndexByURI, healthMap, loaded
	nodeList, healthMap, loaded = nodes, health, true
	rebuildNodeIndexUnsafe()
	mu.Unlock()
	b.Cleanup(func() {
		mu.Lock()
		nodeList, nodeIndexByURI, healthMap, loaded =
			previousNodes, previousIndex, previousHealth, previousLoaded
		rebuildNodeHealthIndexUnsafe()
		mu.Unlock()
	})

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if !EnableNode(target) {
			b.Fatal("target node disappeared")
		}
	}
}

func BenchmarkPruneHealthLargePool(b *testing.B) {
	const nodeCount = 5000
	const staleCount = 500
	nodes := make([]Node, nodeCount)
	health := make(map[string]*NodeHealth, nodeCount+staleCount)
	for index := range nodes {
		uri := fmt.Sprintf("http://prune-node-%d.invalid:8080", index)
		nodes[index] = Node{Name: fmt.Sprintf("node-%d", index), RawURI: uri}
		health[uri] = &NodeHealth{SuccessCount: 1} //nolint:exhaustruct
	}
	for index := range staleCount {
		uri := fmt.Sprintf("http://stale-node-%d.invalid:8080", index)
		health[uri] = &NodeHealth{FailCount: 1} //nolint:exhaustruct
	}

	mu.Lock()
	previousNodes, previousIndex, previousHealth, previousLoaded := nodeList, nodeIndexByURI, healthMap, loaded
	nodeList, healthMap, loaded = nodes, health, true
	rebuildNodeIndexUnsafe()
	mu.Unlock()
	b.Cleanup(func() {
		mu.Lock()
		nodeList, nodeIndexByURI, healthMap, loaded = previousNodes, previousIndex, previousHealth, previousLoaded
		rebuildNodeHealthIndexUnsafe()
		mu.Unlock()
	})

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		mu.Lock()
		for index := range staleCount {
			uri := fmt.Sprintf("http://stale-node-%d.invalid:8080", index)
			healthMap[uri] = &NodeHealth{FailCount: 1} //nolint:exhaustruct
		}
		pruneHealthUnsafe()
		mu.Unlock()
	}
}

func BenchmarkSelectNodesForHealthCheckLargePool(b *testing.B) {
	const nodeCount = 5000
	nodes := make([]Node, nodeCount)
	health := make(map[string]*NodeHealth, nodeCount)
	now := time.Now()
	for index := range nodes {
		uri := fmt.Sprintf("http://health-node-%d.invalid:8080", index)
		nodes[index] = Node{Name: fmt.Sprintf("node-%d", index), RawURI: uri}
		health[uri] = &NodeHealth{
			LastSuccessAt: now.Add(-time.Duration(index+1) * time.Second).Unix(),
		}
	}

	mu.Lock()
	previousNodes, previousIndex, previousHealth, previousLoaded := nodeList, nodeIndexByURI, healthMap, loaded
	nodeList, healthMap, loaded = nodes, health, true
	rebuildNodeIndexUnsafe()
	mu.Unlock()
	b.Cleanup(func() {
		mu.Lock()
		nodeList, nodeIndexByURI, healthMap, loaded = previousNodes, previousIndex, previousHealth, previousLoaded
		rebuildNodeHealthIndexUnsafe()
		mu.Unlock()
	})

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkSelectedNodes = SelectNodesForHealthCheck(50, 0, now)
	}
}

func BenchmarkLoadNodePoolSnapshotLargePool(b *testing.B) {
	const nodeCount = 5000
	nodes := make([]Node, nodeCount)
	health := make(map[string]*NodeHealth, nodeCount)
	now := time.Now()
	for index := range nodes {
		uri := fmt.Sprintf("http://snapshot-node-%d.invalid:8080", index)
		nodes[index] = Node{Name: fmt.Sprintf("node-%d", index), RawURI: uri}
		health[uri] = &NodeHealth{
			SuccessCount:  1,
			LastSuccessAt: now.Unix(),
		}
	}

	mu.Lock()
	previousNodes, previousIndex, previousHealth, previousLoaded := nodeList, nodeIndexByURI, healthMap, loaded
	nodeList, healthMap, loaded = nodes, health, true
	rebuildNodeIndexUnsafe()
	mu.Unlock()
	b.Cleanup(func() {
		mu.Lock()
		nodeList, nodeIndexByURI, healthMap, loaded = previousNodes, previousIndex, previousHealth, previousLoaded
		rebuildNodeHealthIndexUnsafe()
		mu.Unlock()
	})

	b.Run("separate_reads", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkSelectedNodes = LoadNodes()
			benchmarkHealthPointers = LoadHealth()
			benchmarkNodePoolStats = GetNodePoolStats(now)
		}
	})
	b.Run("consistent_snapshot", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkNodePoolSnapshot = LoadNodePoolSnapshot(now)
		}
	})
	b.Run("filtered_page", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkNodePageSnapshot = LoadNodePoolPageSnapshot(now, 1, 50, nil)
		}
	})
	b.Run("filtered_uris", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkNodeURIs = LoadFilteredNodeURIs(nil)
		}
	})
}

func BenchmarkSortNodesLargePoolAlreadySorted(b *testing.B) {
	const nodeCount = 5000
	nodes := make([]Node, nodeCount)
	health := make(map[string]*NodeHealth, nodeCount)
	for index := range nodes {
		rawURI := fmt.Sprintf("http://sort-node-%d.invalid:8080", index)
		nodes[index] = Node{
			Name:   fmt.Sprintf("node-%05d", index),
			RawURI: rawURI,
		}
		health[rawURI] = &NodeHealth{LastTestMs: float64(index + 1)}
	}

	mu.Lock()
	previousNodes, previousIndex, previousHealth, previousLoaded :=
		nodeList, nodeIndexByURI, healthMap, loaded
	nodeList, healthMap, loaded = nodes, health, true
	rebuildNodeIndexUnsafe()
	mu.Unlock()
	b.Cleanup(func() {
		mu.Lock()
		nodeList, nodeIndexByURI, healthMap, loaded =
			previousNodes, previousIndex, previousHealth, previousLoaded
		rebuildNodeHealthIndexUnsafe()
		mu.Unlock()
	})

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		SortNodesByLatency()
	}
}

func TestPruneHealthRemovesOnlyUnknownNodes(t *testing.T) {
	resetState()
	defer resetState()

	firstURI := "http://first.invalid:8080"
	secondURI := "http://second.invalid:8080"
	staleURI := "http://stale.invalid:8080"
	mu.Lock()
	nodeList = []Node{
		{Name: "first", RawURI: firstURI},
		{Name: "second", RawURI: secondURI},
	}
	healthMap = map[string]*NodeHealth{
		firstURI:  {SuccessCount: 1},
		secondURI: {FailCount: 1},
		staleURI:  {FailCount: 2},
	}
	loaded = true
	rebuildNodeIndexUnsafe()
	pruneHealthUnsafe()
	mu.Unlock()

	health := LoadHealth()
	if len(health) != 2 || health[firstURI] == nil || health[secondURI] == nil {
		t.Fatalf("pruned health = %#v, want only known nodes", health)
	}
	if health[staleURI] != nil {
		t.Fatalf("stale health survived pruning: %#v", health[staleURI])
	}
}

func TestLoadNodePoolSnapshotIsConsistentAndDetached(t *testing.T) {
	resetState()
	defer resetState()

	now := time.Now()
	healthyURI := "http://healthy.invalid:8080"
	disabledURI := "http://disabled.invalid:8080"
	mu.Lock()
	nodeList = []Node{
		{Name: "healthy", RawURI: healthyURI},
		{Name: "disabled", RawURI: disabledURI, Disabled: true},
	}
	healthMap = map[string]*NodeHealth{
		healthyURI: {SuccessCount: 2, LastSuccessAt: now.Unix()},
	}
	subscriptionSources = map[string]map[int64]bool{
		healthyURI: {1: true, 2: true},
	}
	loaded = true
	rebuildNodeIndexUnsafe()
	mu.Unlock()

	snapshot := LoadNodePoolSnapshot(now)
	if len(snapshot.Nodes) != 2 || len(snapshot.Health) != 2 || len(snapshot.HasHealth) != 2 {
		t.Fatalf("snapshot sizes: nodes=%d health=%d", len(snapshot.Nodes), len(snapshot.Health))
	}
	if snapshot.Nodes[0].SubscriptionSourceCount != 2 {
		t.Fatalf("subscription source count=%d, want 2", snapshot.Nodes[0].SubscriptionSourceCount)
	}
	if snapshot.Stats.Total != 2 || snapshot.Stats.Enabled != 1 ||
		snapshot.Stats.Disabled != 1 || snapshot.Stats.Healthy != 1 {
		t.Fatalf("snapshot stats: %#v", snapshot.Stats)
	}

	snapshot.Nodes[0].Name = "mutated"
	snapshot.Health[0].SuccessCount = 99
	next := LoadNodePoolSnapshot(now)
	if next.Nodes[0].Name != "healthy" || next.Health[0].SuccessCount != 2 {
		t.Fatalf("snapshot mutation leaked into store: %#v %#v", next.Nodes[0], next.Health[0])
	}
}

func TestLoadNodePoolPageSnapshotFiltersPagesClampsAndDetaches(t *testing.T) {
	resetState()
	defer resetState()

	now := time.Now()
	mu.Lock()
	nodeList = []Node{
		{Name: "manual", RawURI: "http://manual.invalid"},
		{Name: "subscription-one", RawURI: "http://one.invalid"},
		{Name: "subscription-two", RawURI: "http://two.invalid", Disabled: true},
		{Name: "subscription-three", RawURI: "http://three.invalid"},
	}
	healthMap = map[string]*NodeHealth{
		"http://one.invalid":   {SuccessCount: 2, LastSuccessAt: now.Unix()},
		"http://three.invalid": {FailCount: 1, ConsecutiveFailures: 1, LastFailAt: now.Unix()},
	}
	subscriptionSources = map[string]map[int64]bool{
		"http://one.invalid":   {1: true},
		"http://two.invalid":   {1: true, 2: true},
		"http://three.invalid": {2: true},
	}
	loaded = true
	rebuildNodeIndexUnsafe()
	mu.Unlock()

	matchSubscription := func(node Node, _ *NodeHealth) bool {
		return node.SubscriptionSourceCount > 0
	}
	snapshot := LoadNodePoolPageSnapshot(now, 2, 1, matchSubscription)
	if snapshot.TotalMatches != 3 || snapshot.Page != 2 || snapshot.PageSize != 1 ||
		snapshot.TotalPages != 3 || len(snapshot.Nodes) != 1 {
		t.Fatalf("page snapshot metadata=%#v", snapshot)
	}
	if snapshot.Nodes[0].Name != "subscription-two" ||
		snapshot.Nodes[0].SubscriptionSourceCount != 2 || snapshot.HasHealth[0] {
		t.Fatalf("page snapshot entry=%#v health=%#v hasHealth=%v",
			snapshot.Nodes[0], snapshot.Health[0], snapshot.HasHealth[0])
	}
	if snapshot.Stats.Total != 4 || snapshot.Stats.Enabled != 3 ||
		snapshot.Stats.Disabled != 1 || snapshot.Stats.Healthy != 1 ||
		snapshot.Stats.Unhealthy != 1 {
		t.Fatalf("page snapshot stats=%#v", snapshot.Stats)
	}

	clamped := LoadNodePoolPageSnapshot(now, 99, 1, matchSubscription)
	if clamped.Page != 3 || clamped.TotalPages != 3 || len(clamped.Nodes) != 1 ||
		clamped.Nodes[0].Name != "subscription-three" || !clamped.HasHealth[0] {
		t.Fatalf("clamped page=%#v", clamped)
	}
	clamped.Nodes[0].Name = "mutated"
	clamped.Health[0].FailCount = 99
	next := LoadNodePoolPageSnapshot(now, 3, 1, matchSubscription)
	if next.Nodes[0].Name != "subscription-three" || next.Health[0].FailCount != 1 {
		t.Fatalf("page mutation leaked into store: %#v %#v", next.Nodes[0], next.Health[0])
	}

	unpaged := LoadNodePoolPageSnapshot(now, 7, 0, matchSubscription)
	if unpaged.Page != 1 || unpaged.PageSize != 3 || unpaged.TotalPages != 1 ||
		len(unpaged.Nodes) != 3 {
		t.Fatalf("unpaged snapshot=%#v", unpaged)
	}

	maxInt := int(^uint(0) >> 1)
	extreme := LoadNodePoolPageSnapshot(now, maxInt, maxInt, matchSubscription)
	if extreme.Page != 1 || extreme.PageSize != maxInt || extreme.TotalPages != 1 ||
		len(extreme.Nodes) != 3 {
		t.Fatalf("extreme pagination snapshot=%#v", extreme)
	}

	healthyURIs := LoadFilteredNodeURIs(func(node Node, health *NodeHealth) bool {
		return node.SubscriptionSourceCount > 0 && health != nil && health.SuccessCount > 0
	})
	if len(healthyURIs) != 1 || healthyURIs[0] != "http://one.invalid" {
		t.Fatalf("filtered URIs=%#v", healthyURIs)
	}
	healthyURIs[0] = "mutated"
	nextURIs := LoadFilteredNodeURIs(func(_ Node, health *NodeHealth) bool {
		return health != nil && health.SuccessCount > 0
	})
	if len(nextURIs) != 1 || nextURIs[0] != "http://one.invalid" {
		t.Fatalf("URI result mutation leaked into store: %#v", nextURIs)
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

func TestRetainHighestKnownPrioritizesHealthyThenScore(t *testing.T) {
	candidates := []scoredNode{
		{node: Node{RawURI: "recover-high"}, score: 1000, recovering: true},
		{node: Node{RawURI: "healthy-low"}, score: 1},
		{node: Node{RawURI: "recover-low"}, score: 10, recovering: true},
		{node: Node{RawURI: "healthy-high"}, score: 2},
	}
	retained := make([]scoredNode, 0, 3)
	for _, candidate := range candidates {
		retained = retainHighestKnown(retained, candidate, 3)
	}
	seen := make(map[string]bool, len(retained))
	for _, candidate := range retained {
		seen[candidate.node.RawURI] = true
	}
	for _, want := range []string{"healthy-low", "healthy-high", "recover-high"} {
		if !seen[want] {
			t.Fatalf("known top set dropped %q: %#v", want, retained)
		}
	}
	if seen["recover-low"] {
		t.Fatalf("lower-scored recovering node displaced a better candidate: %#v", retained)
	}
}

func TestRetainHighestKnownMatchesFullSort(t *testing.T) {
	const candidateCount = 257
	allHealthy := make([]scoredNode, candidateCount)
	mixed := make([]scoredNode, candidateCount)
	for index := range candidateCount {
		score := float64((index*73)%candidateCount) + float64(index)/candidateCount
		node := Node{RawURI: fmt.Sprintf("candidate-%03d", index)}
		allHealthy[index] = scoredNode{node: node, score: score}
		mixed[index] = scoredNode{node: node, score: score, recovering: index%3 == 0}
	}

	for name, candidates := range map[string][]scoredNode{
		"all healthy": allHealthy,
		"mixed":       mixed,
	} {
		for _, limit := range []int{1, 2, 3, 10, 80, 128, candidateCount} {
			t.Run(fmt.Sprintf("%s/limit=%d", name, limit), func(t *testing.T) {
				want := append([]scoredNode(nil), candidates...)
				sort.Slice(want, func(i, j int) bool {
					return knownNodeBetter(want[i], want[j])
				})
				want = want[:limit]

				got := make([]scoredNode, 0, limit)
				for _, candidate := range candidates {
					got = retainHighestKnown(got, candidate, limit)
				}
				sort.Slice(got, func(i, j int) bool {
					return knownNodeBetter(got[i], got[j])
				})
				for index := range want {
					if got[index].node.RawURI != want[index].node.RawURI ||
						got[index].score != want[index].score ||
						got[index].recovering != want[index].recovering {
						t.Fatalf("candidate %d = %#v, want %#v", index, got[index], want[index])
					}
				}
			})
		}
	}
}

func TestRetainHighestKnownPreservesNonFiniteComparisonSemantics(t *testing.T) {
	retained := make([]scoredNode, 0, 2)
	for _, candidate := range []scoredNode{
		{node: Node{RawURI: "one"}, score: 1},
		{node: Node{RawURI: "two"}, score: 2},
		{node: Node{RawURI: "nan-candidate"}, score: math.NaN()},
	} {
		retained = retainHighestKnown(retained, candidate, 2)
	}
	for _, candidate := range retained {
		if candidate.node.RawURI == "nan-candidate" {
			t.Fatalf("NaN candidate unexpectedly displaced a finite score: %#v", retained)
		}
	}

	retained = make([]scoredNode, 0, 2)
	for _, candidate := range []scoredNode{
		{node: Node{RawURI: "nan-root"}, score: math.NaN()},
		{node: Node{RawURI: "two"}, score: 2},
		{node: Node{RawURI: "three"}, score: 3},
	} {
		retained = retainHighestKnown(retained, candidate, 2)
	}
	seen := make(map[string]bool, len(retained))
	for _, candidate := range retained {
		seen[candidate.node.RawURI] = true
	}
	if !seen["nan-root"] || !seen["two"] || seen["three"] {
		t.Fatalf("NaN heap-root comparison changed: %#v", retained)
	}
}

func TestHealthByNodeIndexTracksNodeAndHealthMutations(t *testing.T) {
	resetState()
	defer resetState()

	assertIndex := func() {
		t.Helper()
		mu.RLock()
		defer mu.RUnlock()
		if len(healthByNodeIndex) != len(nodeList) {
			t.Fatalf("health index length=%d, node count=%d", len(healthByNodeIndex), len(nodeList))
		}
		for index, node := range nodeList {
			if healthByNodeIndex[index] != healthMap[node.RawURI] {
				t.Fatalf(
					"health index %d for %q=%p, map=%p",
					index,
					node.RawURI,
					healthByNodeIndex[index],
					healthMap[node.RawURI],
				)
			}
		}
	}

	if err := MergeNodes([]Node{
		{RawURI: "http://three.invalid", Name: "three"},
		{RawURI: "http://one.invalid", Name: "one"},
		{RawURI: "http://two.invalid", Name: "two"},
	}); err != nil {
		t.Fatal(err)
	}
	assertIndex()

	RecordTest("http://two.invalid", true, 20, "")
	RecordRateLimit("http://three.invalid", 30)
	assertIndex()

	SortNodesByLatency()
	assertIndex()

	if deleted, err := DeleteNodeWithError("http://two.invalid"); err != nil || !deleted {
		t.Fatalf("DeleteNodeWithError()=(%v, %v), want (true, nil)", deleted, err)
	}
	assertIndex()

	if err := BatchDeleteNodes([]string{"http://one.invalid"}); err != nil {
		t.Fatal(err)
	}
	assertIndex()
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

	if err := MergeNodes([]Node{n1, n2, n1}); err != nil {
		t.Fatalf("MergeNodes() error = %v", err)
	}

	nodes := LoadNodes()
	if len(nodes) != 2 {
		t.Fatalf("Expected 2 nodes, got %d", len(nodes))
	}

	// Test Dedup
	if err := MergeNodes([]Node{n1}); err != nil { // Add duplicate
		t.Fatalf("MergeNodes() duplicate error = %v", err)
	}
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
	if err := BatchUpdateNodesDisabled([]string{"uri1"}, true); err != nil {
		t.Fatalf("BatchUpdateNodesDisabled() error = %v", err)
	}
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
		{"password ignored", "trojan://alice:secret@example.com:8443/path?x=1#name", true, "trojan", "alice", "example.com", 8443},
		{"default port", "http://example.com/path", true, "http", "", "example.com", 443},
		{"ipv6", "socks5://user@[2001:db8::1]:1080", true, "socks5", "user", "2001:db8::1", 1080},
		{"escaped username fallback", "vless://user%20name@example.com:443", true, "vless", "user name", "example.com", 443},
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

func TestParseNodeIdentityVMessPortVariants(t *testing.T) {
	for _, test := range []struct {
		name     string
		portJSON string
		wantPort int
	}{
		{name: "string", portJSON: `"443"`, wantPort: 443},
		{name: "escaped string", portJSON: `"4\u00343"`, wantPort: 443},
		{name: "number", portJSON: `443`, wantPort: 443},
		{name: "exponent", portJSON: `4.43e2`, wantPort: 443},
		{name: "fraction", portJSON: `443.5`, wantPort: 0},
		{name: "null", portJSON: `null`, wantPort: 0},
		{name: "object", portJSON: `{}`, wantPort: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := fmt.Sprintf(
				`{"add":"vmess.example.com","port":%s,"id":"uuid"}`,
				test.portJSON,
			)
			uri := "vmess://" + base64.RawURLEncoding.EncodeToString([]byte(payload))
			scheme, user, host, port, ok := parseNodeIdentity(uri)
			if !ok || scheme != "vmess" || user != "uuid" || host != "vmess.example.com" || port != test.wantPort {
				t.Fatalf("identity=(%q, %q, %q, %d, %v), want port %d", scheme, user, host, port, ok, test.wantPort)
			}
		})
	}
}

func TestUpdateNodeTestResult(t *testing.T) {
	resetState()
	defer resetState()

	// Setup: one enabled node
	n1 := Node{RawURI: "uri1", Name: "node1"} //nolint:exhaustruct
	if err := MergeNodes([]Node{n1}); err != nil {
		t.Fatalf("MergeNodes() error = %v", err)
	}

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

	if err := MergeNodes([]Node{n1, n2}); err != nil {
		t.Fatalf("MergeNodes() error = %v", err)
	}

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

	if err := MergeNodes([]Node{n1}); err != nil {
		t.Fatalf("MergeNodes() prune error = %v", err)
	}
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
	if err := MergeNodes([]Node{n1}); err != nil {
		t.Fatalf("MergeNodes() error = %v", err)
	}

	// Also set cooldown
	RecordTest("uri1", false, 0, "timeout")
	mu.Lock()
	delete(nodeIndexByURI, "uri1")
	mu.Unlock()

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

func TestNodeMutationsReportPersistenceFailureAndRollBack(t *testing.T) {
	db.CloseDB()
	if err := db.InitDB(filepath.Join(t.TempDir(), "nodes.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.CloseDB)
	resetState()
	t.Cleanup(resetState)

	const uri = "http://rollback.invalid:8080"
	if err := MergeNodes([]Node{{
		RawURI: uri, Name: "rollback", Disabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	database := db.CurrentDB()
	if database == nil {
		t.Fatal("test database not initialized")
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	if enabled, err := EnableNodeWithError(uri); err == nil || enabled {
		t.Fatalf("enable after DB close = (%v, %v), want persistence error", enabled, err)
	}
	loaded := LoadNodes()
	if len(loaded) != 1 || !loaded[0].Disabled {
		t.Fatalf("failed enable must preserve disabled state: %#v", loaded)
	}

	if deleted, err := DeleteNodeWithError(uri); err == nil || deleted {
		t.Fatalf("delete after DB close = (%v, %v), want persistence error", deleted, err)
	}
	loaded = LoadNodes()
	if len(loaded) != 1 || loaded[0].RawURI != uri {
		t.Fatalf("failed delete must restore node: %#v", loaded)
	}
}

func TestDedupNodesSemantic(t *testing.T) {
	resetState()
	defer resetState()

	// Two nodes with same identity but different raw URIs (different names/fragments)
	n1 := Node{RawURI: "vless://uuid@example.com:443?security=tls#name1", Name: "node1"}
	n2 := Node{RawURI: "vless://uuid@example.com:443?security=tls#name2", Name: "node2"}
	if err := MergeNodes([]Node{n1, n2}); err != nil {
		t.Fatalf("MergeNodes() error = %v", err)
	}

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
	if err := MergeNodes([]Node{n1, n2, n3}); err != nil {
		t.Fatalf("MergeNodes() error = %v", err)
	}

	// Put n1 and n2 in cooldown, leave n3 normal
	RecordTest("uri1", false, 0, "timeout")
	RecordTest("uri2", false, 0, "timeout")

	// 冷却中的失败节点不应为了凑满并发数而重新进入候选。
	selected := SelectForParallel(3, 80, false, false)
	if len(selected) != 1 || selected[0].RawURI != "uri3" {
		t.Errorf("Expected only the available node, got %#v", selected)
	}
}

func TestSelectionUpperBoundMatchesScoreSemantics(t *testing.T) {
	for _, test := range []struct {
		name  string
		value float64
		upper float64
		want  float64
	}{
		{name: "negative", value: -5, upper: 100, want: -5},
		{name: "below", value: 9, upper: 10, want: 9},
		{name: "boundary", value: 10, upper: 10, want: 10},
		{name: "above", value: 11, upper: 10, want: 10},
		{name: "positive infinity", value: math.Inf(1), upper: 30, want: 30},
		{name: "negative infinity", value: math.Inf(-1), upper: 30, want: math.Inf(-1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := selectionUpperBound(test.value, test.upper); got != test.want {
				t.Fatalf("selectionUpperBound(%v, %v)=%v, want %v", test.value, test.upper, got, test.want)
			}
		})
	}
	if got := selectionUpperBound(math.NaN(), 10); !math.IsNaN(got) {
		t.Fatalf("selectionUpperBound(NaN, 10)=%v, want NaN", got)
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
	storeNodeHealthUnsafe("late", &NodeHealth{
		LastFailAt: now, CooldownUntil: now + 300, Last429At: now + 30,
	})
	storeNodeHealthUnsafe("early", &NodeHealth{
		LastFailAt: now, CooldownUntil: now + 100, Last429At: now + 10,
	})
	storeNodeHealthUnsafe("middle", &NodeHealth{
		LastFailAt: now, CooldownUntil: now + 200, Last429At: now + 20,
	})
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
	if err := MergeNodes(list); err != nil {
		t.Fatalf("MergeNodes() error = %v", err)
	}
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

func TestSelectForParallelStickyMembershipPaths(t *testing.T) {
	tests := []struct {
		name        string
		lowCount    int
		targetIndex int
		stale       bool
	}{
		{name: "inline indexes", lowCount: 127, targetIndex: 128},
		{name: "stale index fallback", lowCount: 126, targetIndex: 127, stale: true},
		{name: "dense map fallback", lowCount: 128, targetIndex: 129, stale: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetState()
			const nodeCount = 160
			now := time.Now().Unix()
			list := make([]Node, nodeCount)
			health := make(map[string]*NodeHealth, nodeCount)
			sticky := NewStickyNodePool()
			for index := range list {
				uri := fmt.Sprintf("sticky-index-%d", index)
				list[index] = Node{RawURI: uri, Name: uri}
				health[uri] = &NodeHealth{LastSuccessAt: now}
				switch {
				case index == 0:
					health[uri].SuccessCount = 10
				case index <= test.lowCount:
					health[uri].FailCount = 10
					sticky.Add(uri)
				case index == test.targetIndex:
					sticky.Add(uri)
				}
			}
			if test.stale {
				sticky.Add("stale-sticky-index")
			}

			mu.Lock()
			previousSticky := globalStickyPool
			nodeList, healthMap, loaded = list, health, true
			rebuildNodeIndexUnsafe()
			globalStickyPool = sticky
			mu.Unlock()
			defer func() {
				mu.Lock()
				globalStickyPool = previousSticky
				mu.Unlock()
				resetState()
			}()

			plain := SelectForParallel(1, 1, false, false)
			if len(plain) != 1 || plain[0].RawURI != list[0].RawURI {
				t.Fatalf("selection without sticky bonus=%#v, want %q", plain, list[0].RawURI)
			}
			boosted := SelectForParallel(1, 1, false, true)
			if len(boosted) != 1 || boosted[0].RawURI != list[test.targetIndex].RawURI {
				t.Fatalf(
					"sticky selection=%#v, want %q",
					boosted,
					list[test.targetIndex].RawURI,
				)
			}
		})
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
	storeNodeHealthUnsafe(uri, &NodeHealth{
		SuccessCount:   1000,
		FailCount:      10,
		RecentUseCount: 20,
	})
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

	if err := MergeNodes([]Node{
		{RawURI: "untested", Name: "untested"},
		{RawURI: "failed", Name: "failed"},
		{RawURI: "healthy", Name: "healthy"},
		{RawURI: "disabled", Name: "disabled", Disabled: true},
	}); err != nil {
		t.Fatalf("MergeNodes() error = %v", err)
	}
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

func TestSelectNodesForHealthCheckPreservesStableOrderAtLimit(t *testing.T) {
	resetState()
	defer resetState()

	const nodeCount = 100
	nodes := make([]Node, nodeCount)
	for index := range nodes {
		uri := fmt.Sprintf("untested-%03d", index)
		nodes[index] = Node{RawURI: uri, Name: uri}
	}
	if err := MergeNodes(nodes); err != nil {
		t.Fatal(err)
	}

	for _, limit := range []int{5, 80} {
		selected := SelectNodesForHealthCheck(limit, time.Hour, time.Now())
		if len(selected) != limit {
			t.Fatalf("limit %d selected %d nodes", limit, len(selected))
		}
		for index, node := range selected {
			want := fmt.Sprintf("untested-%03d", index)
			if node.RawURI != want {
				t.Fatalf("limit %d selected[%d]=%q, want %q", limit, index, node.RawURI, want)
			}
		}
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
