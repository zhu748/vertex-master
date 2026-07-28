package nodes

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

func resetRecentProxyState() {
	recentProxyMu.Lock()
	recentProxyStatus = RecentProxyStatus{} //nolint:exhaustruct
	recentProxyHistory = nil
	recentProxyMu.Unlock()
}

func TestRecordProxySuccessTracksSafeRecentWinner(t *testing.T) {
	resetRecentProxyState()
	t.Cleanup(resetRecentProxyState)
	rawURI := "socks5://user:secret@8.8.8.8:1080"
	if err := MergeNodes([]Node{{
		Type:   "socks5",
		Name:   "recent winner",
		RawURI: rawURI,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { DeleteNode(rawURI) })

	RecordProxySuccessForRequest(rawURI, "request-123", 87.5)
	got := GetRecentProxyStatus()
	if !got.Available || got.Direct || got.RawURI != rawURI {
		t.Fatalf("unexpected recent proxy status: %#v", got)
	}
	if got.Name != "recent winner" || got.Type != "socks5" || got.Address != "8.8.8.8:1080" {
		t.Fatalf("unexpected recent proxy display fields: %#v", got)
	}
	if strings.Contains(got.Address, "user") || strings.Contains(got.Address, "secret") {
		t.Fatalf("proxy credentials leaked in display address: %q", got.Address)
	}
	if got.LastUsedAt <= 0 || got.Revision == 0 {
		t.Fatalf("recent proxy timestamp/revision missing: %#v", got)
	}
	if got.RequestID != "request-123" || got.LatencyMs != 87.5 {
		t.Fatalf("recent request correlation missing: %#v", got)
	}
	history := GetRecentProxyHistory(10)
	if len(history) != 1 || history[0].RequestID != "request-123" || history[0].LatencyMs != 87.5 {
		t.Fatalf("unexpected recent proxy history: %#v", history)
	}
	if strings.Contains(history[0].Address, "user") || strings.Contains(history[0].Address, "secret") {
		t.Fatalf("proxy credentials leaked in route history: %#v", history[0])
	}
}

func TestRecordProxySuccessTracksDirectRequest(t *testing.T) {
	resetRecentProxyState()
	t.Cleanup(resetRecentProxyState)
	RecordProxySuccess("")
	got := GetRecentProxyStatus()
	if !got.Available || !got.Direct || got.Type != "direct" || got.Address != "未使用代理" {
		t.Fatalf("unexpected direct status: %#v", got)
	}
}

func TestRecordProxySuccessForNodeUsesProvidedMetadata(t *testing.T) {
	resetRecentProxyState()
	t.Cleanup(resetRecentProxyState)
	node := Node{
		Type:   "SOCKS5",
		Name:   "known\nnode",
		RawURI: "socks5://user:secret@9.9.9.9:1080",
	}
	RecordProxySuccessForNode(node, "request-known", 12.5)
	got := GetRecentProxyStatus()
	if got.Name != "knownnode" || got.Type != "socks5" || got.Address != "9.9.9.9:1080" {
		t.Fatalf("known node metadata not used safely: %#v", got)
	}
}

func TestRecordProxySuccessHistoryIsBoundedAndNewestFirst(t *testing.T) {
	resetRecentProxyState()
	t.Cleanup(resetRecentProxyState)
	for index := 0; index < recentProxyHistoryLimit+5; index++ {
		RecordProxySuccessForRequest("", "request-"+strconv.Itoa(index), float64(index))
	}

	history := GetRecentProxyHistory(0)
	if len(history) != recentProxyHistoryLimit {
		t.Fatalf("history length = %d, want %d", len(history), recentProxyHistoryLimit)
	}
	for index, event := range history {
		want := recentProxyHistoryLimit + 4 - index
		if event.RequestID != "request-"+strconv.Itoa(want) || event.LatencyMs != float64(want) {
			t.Fatalf("history[%d] = %#v, want request-%d", index, event, want)
		}
	}
}

func TestRecordProxySuccessConcurrentHistoryUpdates(t *testing.T) {
	resetRecentProxyState()
	t.Cleanup(resetRecentProxyState)
	const updates = 64
	var wg sync.WaitGroup
	for index := 0; index < updates; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			RecordProxySuccessForRequest("", "request-"+strconv.Itoa(index), float64(index))
		}(index)
	}
	wg.Wait()

	if status := GetRecentProxyStatus(); status.Revision != updates {
		t.Fatalf("revision = %d, want %d", status.Revision, updates)
	}
	if history := GetRecentProxyHistory(0); len(history) != recentProxyHistoryLimit {
		t.Fatalf("history length = %d, want %d", len(history), recentProxyHistoryLimit)
	}
}

func TestSanitizeRecentRequestIDFastAndFilteredPathsMatchContract(t *testing.T) {
	if got := sanitizeRecentRequestID(" request-123 "); got != "request-123" {
		t.Fatalf("valid request ID = %q", got)
	}
	if got := sanitizeRecentRequestID("request\n中文\t-123"); got != "request-123" {
		t.Fatalf("filtered request ID = %q", got)
	}
	if got := sanitizeRecentRequestID(strings.Repeat("x", 80)); len(got) != 64 {
		t.Fatalf("bounded request ID length = %d, want 64", len(got))
	}
}

func TestStandardProxyAuthority(t *testing.T) {
	for _, test := range []struct {
		uri         string
		wantType    string
		wantAddress string
		wantOK      bool
	}{
		{
			uri: "socks5://user:secret@8.8.8.8:1080/path?x=1", wantType: "socks5",
			wantAddress: "8.8.8.8:1080", wantOK: true,
		},
		{
			uri: "VLESS://uuid@[2001:db8::1]:443#node", wantType: "vless",
			wantAddress: "[2001:db8::1]:443", wantOK: true,
		},
		{
			uri: "hysteria2://token@example.com:8443?insecure=1", wantType: "hysteria2",
			wantAddress: "example.com:8443", wantOK: true,
		},
		{uri: "ss:opaque-value", wantOK: false},
		{uri: "1invalid://host", wantOK: false},
		{uri: "http://user:pass@/path", wantOK: false},
	} {
		gotType, gotAddress, gotOK := standardProxyAuthority(test.uri)
		if gotType != test.wantType || gotAddress != test.wantAddress || gotOK != test.wantOK {
			t.Errorf("standardProxyAuthority(%q)=(%q,%q,%v), want (%q,%q,%v)",
				test.uri, gotType, gotAddress, gotOK,
				test.wantType, test.wantAddress, test.wantOK)
		}
	}
}

func BenchmarkRecordProxySuccessHotHistory(b *testing.B) {
	resetRecentProxyState()
	b.Cleanup(resetRecentProxyState)
	for index := range 20 {
		RecordProxySuccessForRequest("", "warmup-"+string(rune('a'+index)), 1)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		RecordProxySuccessForRequest("", "request-123", 1)
	}
}

func BenchmarkRecordProxySuccessLargePool(b *testing.B) {
	const nodeCount = 2048
	testNodes := make([]Node, nodeCount)
	for index := range testNodes {
		testNodes[index] = Node{
			Type:   "socks5",
			Name:   "benchmark-node-" + strconv.Itoa(index),
			RawURI: "socks5://127.0.0.1:" + strconv.Itoa(20_000+index),
		}
	}
	target := testNodes[len(testNodes)-1]

	mu.Lock()
	previousNodes, previousLoaded := nodeList, loaded
	nodeList, loaded = testNodes, true
	mu.Unlock()
	resetRecentProxyState()
	for range recentProxyHistoryLimit {
		RecordProxySuccessForRequest("", "warmup", 1)
	}
	b.Cleanup(func() {
		mu.Lock()
		nodeList, loaded = previousNodes, previousLoaded
		mu.Unlock()
		resetRecentProxyState()
	})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		RecordProxySuccessForNode(target, "request-123", 1)
	}
}
