//go:build windows

package state

// processAlive cannot reliably test liveness on Windows without extra system
// calls, so only the lock file's age is used there.
func processAlive(int) bool { return true }

// livenessSupported reports whether processAlive is reliable.
func livenessSupported() bool { return false }
