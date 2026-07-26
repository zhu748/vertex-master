package nodes

import (
	"strings"
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
