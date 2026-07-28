package api

import (
	"testing"
	"time"
)

func TestAdminSessionCleanupCanStop(t *testing.T) {
	const token = "cleanup-lifecycle-test"
	adminSessionsMu.Lock()
	adminSessions[token] = time.Now().Add(-time.Minute)
	adminSessionsMu.Unlock()
	t.Cleanup(func() {
		adminSessionsMu.Lock()
		delete(adminSessions, token)
		adminSessionsMu.Unlock()
	})

	stop := StartAdminSessionCleanup(5 * time.Millisecond)
	deadline := time.Now().Add(time.Second)
	for {
		adminSessionsMu.Lock()
		_, exists := adminSessions[token]
		adminSessionsMu.Unlock()
		if !exists {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatal("cleanup worker did not remove expired session")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	stop() // 幂等

	adminSessionsMu.Lock()
	adminSessions[token] = time.Now().Add(-time.Minute)
	adminSessionsMu.Unlock()
	time.Sleep(20 * time.Millisecond)
	adminSessionsMu.Lock()
	_, exists := adminSessions[token]
	adminSessionsMu.Unlock()
	if !exists {
		t.Fatal("stopped cleanup worker continued running")
	}
}
