package state

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// staleLockAge is how old a lock file must be before it is considered
// abandoned. On platforms where liveness cannot be checked, a crashed process
// holds the lock for at most this long.
const staleLockAge = 30 * time.Minute

// Lock is an advisory, cross-platform lock implemented with an exclusively
// created file. It is held for the duration of a mutating operation.
type Lock struct {
	path  string
	file  *os.File
	token string
}

// Acquire takes the lock at path, waiting up to timeout for another process to
// release it. Stale locks are removed.
func Acquire(path string, timeout time.Duration) (*Lock, error) {
	deadline := time.Now().Add(timeout)
	for {
		token, err := lockToken()
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d %s %s\n", os.Getpid(), token, time.Now().UTC().Format(time.RFC3339))
			return &Lock{path: path, file: f, token: token}, nil
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

// stale reports whether the lock at path belongs to a dead process.
func stale(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return time.Since(info.ModTime()) > staleLockAge
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return time.Since(info.ModTime()) > staleLockAge
	}
	if livenessSupported() {
		return !processAlive(pid)
	}
	// Without a reliable liveness check the file age is the only signal.
	return time.Since(info.ModTime()) > staleLockAge
}

func lockToken() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate lock token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// Release removes the lock file only when it is still ours.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	data, err := os.ReadFile(l.path)
	if err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 2 && fields[1] != l.token {
			// Another process owns the lock now; do not remove it.
			return nil
		}
	}
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
