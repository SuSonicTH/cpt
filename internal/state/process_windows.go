//go:build windows

package state

import (
	"syscall"
	"unsafe"
)

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess = kernel32.NewProc("OpenProcess")
	procGetExitCode = kernel32.NewProc("GetExitCodeProcess")
	procCloseHandle = kernel32.NewProc("CloseHandle")
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// processAlive reports whether pid is running, using the Win32 process API.
func processAlive(pid int) bool {
	h, _, callErr := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(uint32(pid)))
	if h == 0 {
		// Access denied means the process exists under another account; never
		// treat that as dead, or a live lock could be stolen.
		if errno, ok := callErr.(syscall.Errno); ok && errno == 5 { // ERROR_ACCESS_DENIED
			return true
		}
		return false
	}
	defer procCloseHandle.Call(h)
	var code uint32
	ok, _, _ := procGetExitCode.Call(h, uintptr(unsafe.Pointer(&code)))
	if ok == 0 {
		// The handle is valid, so assume the process still exists.
		return true
	}
	return code == stillActive
}

// livenessSupported reports whether processAlive is reliable.
func livenessSupported() bool { return true }
