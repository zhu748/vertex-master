//go:build !windows
// +build !windows

package cli

import (
	"os"
	"os/signal"
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

// onResizeOS listens for SIGWINCH and calls callback on terminal resize.
func onResizeOS(callback func()) {
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGWINCH)
		for range ch {
			callback()
		}
	}()
}
