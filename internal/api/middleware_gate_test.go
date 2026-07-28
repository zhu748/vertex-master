package api

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestRequestConcurrencyGateHonorsLimitUnderContention(t *testing.T) {
	const (
		workers = 100
		limit   = 4
	)
	var gate requestConcurrencyGate
	start := make(chan struct{})
	release := make(chan struct{})
	attempted := make(chan struct{}, workers)
	var acquired atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok := gate.tryAcquire(limit)
			if ok {
				acquired.Add(1)
			}
			attempted <- struct{}{}
			if ok {
				<-release
				gate.release()
			}
		}()
	}
	close(start)
	for range workers {
		<-attempted
	}
	if got := acquired.Load(); got != limit {
		t.Fatalf("acquired=%d, want %d", got, limit)
	}
	if got := gate.active.Load(); got != limit {
		t.Fatalf("active=%d, want %d", got, limit)
	}
	close(release)
	wg.Wait()
	if got := gate.active.Load(); got != 0 {
		t.Fatalf("active after release=%d", got)
	}
}

func TestRequestConcurrencyGateHandlesDynamicLimitAndExtraRelease(t *testing.T) {
	var gate requestConcurrencyGate
	for range 4 {
		if !gate.tryAcquire(4) {
			t.Fatal("initial acquire unexpectedly rejected")
		}
	}
	if gate.tryAcquire(2) {
		t.Fatal("shrunk limit accepted a new request above limit")
	}
	for range 3 {
		gate.release()
	}
	if !gate.tryAcquire(2) {
		t.Fatal("gate did not reopen after active count dropped below new limit")
	}
	gate.release()
	gate.release()
	gate.release() // 额外释放不能下溢。
	if got := gate.active.Load(); got != 0 {
		t.Fatalf("active underflowed to %d", got)
	}
}

func BenchmarkRequestConcurrencyGate(b *testing.B) {
	var gate requestConcurrencyGate
	for range b.N {
		if !gate.tryAcquire(16) {
			b.Fatal("unexpected rejection")
		}
		gate.release()
	}
}
