package cli

import (
	"testing"
	"time"
)

func TestResizeWatcherCanStop(t *testing.T) {
	stop := onResizeOS(func() {})
	done := make(chan struct{})
	go func() {
		stop()
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resize watcher did not stop promptly")
	}
}
