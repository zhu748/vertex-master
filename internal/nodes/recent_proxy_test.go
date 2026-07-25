package nodes

import (
	"strings"
	"testing"
)

func TestRecordProxySuccessTracksSafeRecentWinner(t *testing.T) {
	rawURI := "socks5://user:secret@8.8.8.8:1080"
	if err := MergeNodes([]Node{{
		Type:   "socks5",
		Name:   "recent winner",
		RawURI: rawURI,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { DeleteNode(rawURI) })

	RecordProxySuccess(rawURI)
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
}

func TestRecordProxySuccessTracksDirectRequest(t *testing.T) {
	RecordProxySuccess("")
	got := GetRecentProxyStatus()
	if !got.Available || !got.Direct || got.Type != "direct" || got.Address != "未使用代理" {
		t.Fatalf("unexpected direct status: %#v", got)
	}
}
