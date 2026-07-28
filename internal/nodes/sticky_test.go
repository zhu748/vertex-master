package nodes

import (
	"fmt"
	"sync"
	"testing"
)

func TestStickySnapshotIsImmutableAndRefreshesOnChange(t *testing.T) {
	pool := NewStickyNodePool()
	if snapshot := pool.snapshot(); snapshot != nil {
		t.Fatalf("new pool snapshot=%#v, want nil", snapshot)
	}

	pool.Add("node-a")
	first := pool.snapshot()
	if _, ok := first["node-a"]; !ok {
		t.Fatalf("published snapshot missing node-a: %#v", first)
	}
	pool.Add("node-a")
	pool.Evict("missing")
	if _, ok := pool.snapshot()["node-a"]; !ok {
		t.Fatal("idempotent updates changed sticky membership")
	}

	pool.Evict("node-a")
	if current := pool.snapshot(); current != nil {
		t.Fatalf("empty pool snapshot=%#v, want nil", current)
	}
	if _, ok := first["node-a"]; !ok {
		t.Fatal("published immutable snapshot was modified after eviction")
	}
}

func TestStickySnapshotConcurrentReadersAndUpdates(t *testing.T) {
	pool := NewStickyNodePool()
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := range 1000 {
				uri := fmt.Sprintf("node-%d", (worker+iteration)%32)
				_, _ = pool.snapshot()[uri]
			}
		}()
	}
	for iteration := range 1000 {
		uri := fmt.Sprintf("node-%d", iteration%32)
		if iteration%2 == 0 {
			pool.Add(uri)
		} else {
			pool.Evict(uri)
		}
	}
	workers.Wait()
}
