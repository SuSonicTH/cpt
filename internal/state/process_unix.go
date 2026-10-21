//go:build !windows

package state

import "syscall"

// processAlive reports whether pid is running.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
