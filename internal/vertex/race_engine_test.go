package vertex

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/db"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
)

func TestRunRaceRetryableFailureLaunchesNextImmediately(t *testing.T) {
	installRaceTestNodes(t, 3)
	cfg := raceTestConfig(1, 3, 5_000)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	started := make(chan int, 3)
	outcomes := make(chan bool)
	type raceOutcome struct {
		value string
		err   error
	}
	result := make(chan raceOutcome, 1)
	var calls int32

	go func() {
		value, err := RunRace(ctx, cfg, func(candidateCtx context.Context, _ string) (string, error) {
			call := int(atomic.AddInt32(&calls, 1))
			started <- call
			select {
			case succeed := <-outcomes:
				if succeed {
					return fmt.Sprintf("winner-%d", call), nil
				}
				return "", NewUnavailableError("retryable test failure")
			case <-candidateCtx.Done():
				return "", candidateCtx.Err()
			}
		})
		result <- raceOutcome{value: value, err: err}
	}()

	if call := waitForRaceStart(t, started, time.Second); call != 1 {
		t.Fatalf("first call = %d, want 1", call)
	}
	outcomes <- false

	// The hedge delay is five seconds. Starting call 2 inside one second proves
	// that a retryable failure hands off immediately instead of waiting for it.
	if call := waitForRaceStart(t, started, time.Second); call != 2 {
		t.Fatalf("second call = %d, want 2", call)
	}
	outcomes <- true

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("RunRace() error = %v", got.err)
		}
		if got.value != "winner-2" {
			t.Fatalf("RunRace() value = %q, want winner-2", got.value)
		}
	case <-time.After(time.Second):
		t.Fatal("RunRace did not return after the second candidate succeeded")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("candidate calls = %d, want 2", got)
	}
}

func TestRunRaceRespectsMaximumConcurrentCandidates(t *testing.T) {
	const (
		candidateCount = 5
		maxConcurrent  = 2
	)

	installRaceTestNodes(t, candidateCount)
	cfg := raceTestConfig(maxConcurrent, candidateCount, 100)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	started := make(chan int, candidateCount)
	outcomes := make(chan bool)
	finished := make(chan struct{}, candidateCount)
	type raceOutcome struct {
		value string
		err   error
	}
	result := make(chan raceOutcome, 1)
	var calls int32
	var active int32
	var peak int32

	go func() {
		value, err := RunRace(ctx, cfg, func(candidateCtx context.Context, _ string) (string, error) {
			call := int(atomic.AddInt32(&calls, 1))
			current := atomic.AddInt32(&active, 1)
			updateAtomicMaximum(&peak, current)
			started <- call
			defer func() {
				atomic.AddInt32(&active, -1)
				finished <- struct{}{}
			}()

			select {
			case succeed := <-outcomes:
				if succeed {
					return "winner", nil
				}
				return "", NewUnavailableError("retryable test failure")
			case <-candidateCtx.Done():
				return "", candidateCtx.Err()
			}
		})
		result <- raceOutcome{value: value, err: err}
	}()

	waitForRaceStart(t, started, time.Second)
	waitForRaceStart(t, started, 2*time.Second)

	// Both slots remain blocked. Several hedge intervals may pass, but a third
	// candidate must not start until one of those slots is released.
	select {
	case call := <-started:
		t.Fatalf("candidate %d started while %d slots were already occupied", call, maxConcurrent)
	case <-time.After(400 * time.Millisecond):
	}

	for wantStarted := 3; wantStarted <= candidateCount; wantStarted++ {
		outcomes <- false
		if call := waitForRaceStart(t, started, time.Second); call != wantStarted {
			t.Fatalf("started call = %d, want %d", call, wantStarted)
		}
	}
	outcomes <- true

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("RunRace() error = %v", got.err)
		}
		if got.value != "winner" {
			t.Fatalf("RunRace() value = %q, want winner", got.value)
		}
	case <-time.After(time.Second):
		t.Fatal("RunRace did not return after a candidate succeeded")
	}

	for completed := 0; completed < candidateCount; completed++ {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatalf("only %d/%d candidate goroutines stopped", completed, candidateCount)
		}
	}
	if got := atomic.LoadInt32(&peak); got != maxConcurrent {
		t.Fatalf("peak candidate concurrency = %d, want %d", got, maxConcurrent)
	}
	if got := atomic.LoadInt32(&active); got != 0 {
		t.Fatalf("active candidate goroutines after return = %d, want 0", got)
	}
}

func TestStreamParallelCancelsLoserAndWinnerWhenConsumerStops(t *testing.T) {
	installRaceTestNodes(t, 2)
	cfg := raceTestConfig(2, 2, 100)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	loserCanceled := make(chan struct{}, 1)
	winnerCanceled := make(chan struct{}, 1)
	yielded := make(chan StreamChunk, 1)
	done := make(chan struct{})
	var calls int32

	go func() {
		defer close(done)
		StreamParallel(ctx, cfg, func(candidateCtx context.Context, _ string) <-chan StreamChunk {
			call := atomic.AddInt32(&calls, 1)
			stream := make(chan StreamChunk)
			if call == 1 {
				go func() {
					defer close(stream)
					<-candidateCtx.Done()
					loserCanceled <- struct{}{}
				}()
				return stream
			}

			go func() {
				defer close(stream)
				select {
				case stream <- StreamChunk{Data: map[string]any{"winner": true}}:
				case <-candidateCtx.Done():
					winnerCanceled <- struct{}{}
					return
				}
				<-candidateCtx.Done()
				winnerCanceled <- struct{}{}
			}()
			return stream
		}, func(chunk StreamChunk) bool {
			yielded <- chunk
			return false
		})
	}()

	select {
	case chunk := <-yielded:
		if chunk.Err != nil {
			t.Fatalf("yielded stream error = %v", chunk.Err)
		}
		if winner, _ := chunk.Data["winner"].(bool); !winner {
			t.Fatalf("yielded chunk = %#v, want winner marker", chunk.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream winner was not yielded")
	}

	waitForSignal(t, loserCanceled, time.Second, "losing stream context was not canceled")
	waitForSignal(t, winnerCanceled, time.Second, "winning stream context was not canceled after consumer stopped")
	waitForSignal(t, done, time.Second, "StreamParallel did not return after consumer stopped")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("stream candidate calls = %d, want 2", got)
	}
}

func raceTestConfig(poolSize, maxAttempts, delayMs int) config.ConfigProvider {
	cfg := config.DefaultConfig()
	cfg.ParallelPoolEnabled = true
	cfg.StickyNodePriority = false
	cfg.ParallelPoolSize = poolSize
	cfg.ParallelNodeTopK = maxAttempts
	cfg.ParallelPoolDelayDynamic = false
	cfg.ParallelPoolDelayMs = delayMs
	cfg.ProxyFailoverMaxAttempts = maxAttempts
	return config.StaticProvider(cfg)
}

func installRaceTestNodes(t *testing.T, count int) []string {
	t.Helper()

	if db.CurrentDB() == nil {
		if err := db.InitDB(filepath.Join(t.TempDir(), "race-test.db")); err != nil {
			t.Fatalf("initialize race test database: %v", err)
		}
		t.Cleanup(db.CloseDB)
	}

	existing := nodes.LoadNodes()
	reenable := make([]string, 0, len(existing))
	for _, node := range existing {
		if !node.Disabled {
			reenable = append(reenable, node.RawURI)
		}
	}
	if err := nodes.BatchUpdateNodesDisabled(reenable, true); err != nil {
		t.Fatalf("disable existing race test nodes: %v", err)
	}

	prefix := strings.NewReplacer("/", "-", "_", "-").Replace(strings.ToLower(t.Name()))
	testNodes := make([]nodes.Node, 0, count)
	uris := make([]string, 0, count)
	for i := 0; i < count; i++ {
		uri := fmt.Sprintf("http://%s-%d.invalid:%d", prefix, i+1, 30_000+i)
		uris = append(uris, uri)
		testNodes = append(testNodes, nodes.Node{
			Type:   "http",
			Name:   fmt.Sprintf("race-test-%d", i+1),
			RawURI: uri,
		})
	}
	if err := nodes.MergeNodes(testNodes); err != nil {
		t.Fatalf("install race test nodes: %v", err)
	}

	t.Cleanup(func() {
		for _, uri := range uris {
			nodes.GetStickyPool().Evict(uri)
		}
		if err := nodes.BatchDeleteNodes(uris); err != nil {
			t.Errorf("delete race test nodes: %v", err)
		}
		if err := nodes.BatchUpdateNodesDisabled(reenable, false); err != nil {
			t.Errorf("restore existing race test nodes: %v", err)
		}
	})
	return uris
}

func waitForRaceStart(t *testing.T, started <-chan int, timeout time.Duration) int {
	t.Helper()
	select {
	case call := <-started:
		return call
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a candidate to start")
		return 0
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, timeout time.Duration, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(timeout):
		t.Fatal(message)
	}
}

func updateAtomicMaximum(target *int32, value int32) {
	for {
		current := atomic.LoadInt32(target)
		if value <= current || atomic.CompareAndSwapInt32(target, current, value) {
			return
		}
	}
}
