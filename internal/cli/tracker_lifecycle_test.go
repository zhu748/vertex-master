package cli

import (
	"io"
	"os"
	"sync"
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

func TestLogInterceptorConcurrentStateChange(t *testing.T) {
	output, err := os.CreateTemp(t.TempDir(), "tracker-output-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()

	mu.Lock()
	oldEnabled := enabled
	oldOutput := osStdout
	oldWriter := additionalLogWriter
	oldLogs := logBuffer
	oldHeight := lastHeight
	enabled = true
	osStdout = output
	additionalLogWriter = io.Discard
	logBuffer = nil
	lastHeight = 0
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		enabled = oldEnabled
		osStdout = oldOutput
		additionalLogWriter = oldWriter
		logBuffer = oldLogs
		lastHeight = oldHeight
		mu.Unlock()
	})

	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for range 500 {
			_, _ = (logInterceptor{}).Write([]byte("concurrent log\n"))
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for range 500 {
			mu.Lock()
			enabled = true
			mu.Unlock()
		}
	}()
	close(start)
	workers.Wait()
}
