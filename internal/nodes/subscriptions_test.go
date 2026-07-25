package nodes

import (
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

	added, removed := ReplaceSubscriptionNodes(item.ID, []Node{
		{Type: "socks5", Name: "one", RawURI: "socks5://127.0.0.1:1080"}, //nolint:exhaustruct
		{Type: "socks5", Name: "two", RawURI: "socks5://127.0.0.2:1080"}, //nolint:exhaustruct
	})
	if added != 2 || removed != 0 {
		t.Fatalf("first replacement added=%d removed=%d", added, removed)
	}

	added, removed = ReplaceSubscriptionNodes(item.ID, []Node{
		{Type: "socks5", Name: "three", RawURI: "socks5://127.0.0.3:1080"}, //nolint:exhaustruct
	})
	if added != 1 || removed != 2 {
		t.Fatalf("second replacement added=%d removed=%d", added, removed)
	}
	list := LoadNodes()
	if len(list) != 2 {
		t.Fatalf("manual node plus one subscription node expected, got %d", len(list))
	}
	for _, node := range list {
		if node.RawURI == manual.RawURI && node.SourceID != 0 {
			t.Fatal("manual node source was modified")
		}
	}

	if err := UpdateProxySubscriptionResult(item.ID, added, nil); err != nil {
		t.Fatal(err)
	}
	due, err := DueProxySubscriptions(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("freshly updated subscription should not be due: %#v", due)
	}

	if err := DeleteProxySubscription(item.ID); err != nil {
		t.Fatal(err)
	}
	DeleteSubscriptionNodes(item.ID)
	if got := LoadNodes(); len(got) != 1 || got[0].RawURI != manual.RawURI {
		t.Fatalf("deleting subscription must preserve manual node: %#v", got)
	}
}
