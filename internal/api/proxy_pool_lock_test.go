package api

import (
	"testing"
	"time"
)

func TestProxySubscriptionLockSerializesAndReclaimsEntry(t *testing.T) {
	const subscriptionID int64 = 424242
	subscriptionRefreshLockRegistry.Lock()
	delete(subscriptionRefreshLockRegistry.entries, subscriptionID)
	subscriptionRefreshLockRegistry.Unlock()

	releaseFirst := acquireProxySubscriptionLock(subscriptionID)
	firstReleased := false
	t.Cleanup(func() {
		if !firstReleased {
			releaseFirst()
		}
	})

	started := make(chan struct{})
	acquired := make(chan func(), 1)
	go func() {
		close(started)
		acquired <- acquireProxySubscriptionLock(subscriptionID)
	}()
	<-started

	deadline := time.Now().Add(time.Second)
	for {
		subscriptionRefreshLockRegistry.Lock()
		entry := subscriptionRefreshLockRegistry.entries[subscriptionID]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		subscriptionRefreshLockRegistry.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting lock reference count=%d, want 2", refs)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case release := <-acquired:
		release()
		t.Fatal("second caller entered while first still held lock")
	default:
	}

	releaseFirst()
	firstReleased = true
	var releaseSecond func()
	select {
	case releaseSecond = <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second caller did not acquire released lock")
	}
	releaseSecond()

	subscriptionRefreshLockRegistry.Lock()
	_, exists := subscriptionRefreshLockRegistry.entries[subscriptionID]
	subscriptionRefreshLockRegistry.Unlock()
	if exists {
		t.Fatal("unused subscription lock entry was not reclaimed")
	}
}
