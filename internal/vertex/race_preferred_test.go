package vertex

import (
	"context"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/nodes"
)

func TestRunRacePreferredDoesNotLetFastFallbackCancelPreferredResult(t *testing.T) {
	softURI := "http://8.8.8.8:39101"
	preferredURI := "http://1.1.1.1:39102"
	if err := nodes.MergeNodes([]nodes.Node{
		{Type: "http", Name: "soft", RawURI: softURI},
		{Type: "http", Name: "preferred", RawURI: preferredURI},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		nodes.DeleteNode(softURI)
		nodes.DeleteNode(preferredURI)
	})

	result, err := RunRacePreferred(
		context.Background(),
		raceTestConfig(2, 2, 100),
		func(_ context.Context, uri string) (string, error) {
			if uri == softURI {
				time.Sleep(10 * time.Millisecond)
				return "truncated", nil
			}
			time.Sleep(30 * time.Millisecond)
			return "complete", nil
		},
		func(value string) bool { return value == "complete" },
		func(values []string) (string, error) { return values[0], nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result != "complete" {
		t.Fatalf("fast fallback won over preferred result: %q", result)
	}
	if recent := nodes.GetRecentProxyStatus(); recent.RawURI != preferredURI {
		t.Fatalf("recent proxy=%q, want preferred winner %q", recent.RawURI, preferredURI)
	}
}
