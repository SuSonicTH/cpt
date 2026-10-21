package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// staleLockAge is how old a lock file must be before it is considered
// abandoned. A cpt process never holds the lock for anywhere near this long.
const staleLockAge = 10 * time.Minute

// Lock is an advisory, cross-platform lock implemented with an exclusively
// created file. It is held for the duration of a mutating operation.
type Lock struct {
	path string
	file *os.File
}

// Acquire takes the lock at path, waiting up to timeout for another process to
// release it. Stale locks are removed.
func Acquire(path string, timeout time.Duration) (*Lock, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			return &Lock{path: path, file: f}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create lock %s: %w", path, err)
		}
		if stale(path) {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another cpt process is running (lock %s); remove it if this is wrong", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func stale(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if time.Since(info.ModTime()) > staleLockAge {
		return true
	}
	// If the owning pid is recorded and no longer alive, treat it as stale.
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return false
	}
	return !processAlive(pid)
}

// Release removes the lock file.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
