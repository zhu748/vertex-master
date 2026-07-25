//go:build windows
// +build windows

package cli

import (
	"syscall"
	"time"
	"unsafe"
)

type _coord struct{ X, Y int16 }
type _smallRect struct{ Left, Top, Right, Bottom int16 }
type _consoleScreenBufferInfo struct {
	Size              _coord
	CursorPosition    _coord
	Attributes        uint16
	Window            _smallRect
	MaximumWindowSize _coord
}

// getTerminalWidthOS returns the terminal width via GetConsoleScreenBufferInfo (Windows).
// Returns 0 if unavailable.
func getTerminalWidthOS() int {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetConsoleScreenBufferInfo")
	handle, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil {
		return 0
	}
	var info _consoleScreenBufferInfo
	r, _, _ := proc.Call(uintptr(handle), uintptr(unsafe.Pointer(&info)))
	if r != 0 {
		w := int(info.Window.Right - info.Window.Left + 1)
		if w > 0 {
			return w
		}
	}
	return 0
}

// onResizeOS polls terminal size every second and calls callback on change.
func onResizeOS(callback func()) {
	go func() {
		lastW := 0
		for {
			time.Sleep(1 * time.Second)
			if w := getTerminalWidthOS(); w > 0 && w != lastW {
				lastW = w
				callback()
			}
		}
	}()
}
