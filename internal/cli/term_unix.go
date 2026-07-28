//go:build !windows
// +build !windows

package cli

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"unsafe"
)

// getTerminalWidthOS returns the terminal width via ioctl TIOCGWINSZ (Unix).
// Returns 0 if unavailable.
func getTerminalWidthOS() int {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if err == 0 && ws.Col > 0 {
		return int(ws.Col)
	}
	return 0
}

// onResizeOS listens for SIGWINCH and returns an idempotent stop function.
func onResizeOS(callback func()) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer close(done)
		defer signal.Stop(ch)
		for {
			select {
			case <-ch:
				callback()
			case <-stop:
				return
			}
		}
	}()
	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			close(stop)
			<-done
		})
	}
}
