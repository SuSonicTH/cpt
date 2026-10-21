package state

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// lockPollInterval is how often Acquire retries a contended lock.
const lockPollInterval = 100 * time.Millisecond

// Lock is an advisory, cross-platform lock backed by an OS file lock
// (flock on Unix, LockFileEx on Windows). It is held for the duration of a
// mutating operation and is released automatically when the process exits,
// even after a crash. The lock file itself remains on disk between runs; only
// the OS lock is meaningful.
type Lock struct {
	path string
	file *os.File
}

// Acquire takes the lock at path, waiting up to timeout for another process to
// release it. A timeout of zero makes a single non-blocking attempt.
func Acquire(path string, timeout time.Duration) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	start := time.Now()
	for {
		err := tryLock(f)
		if err == nil {
			break
		}
		if !isLockBusy(err) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if time.Since(start) >= timeout {
			_ = f.Close()
			return nil, fmt.Errorf("another cpt process is running (lock %s%s); close it and retry",
				path, lockHolder(path))
		}
		remaining := timeout - time.Since(start)
		delay := lockPollInterval
		if remaining < delay {
			delay = remaining
		}
		if delay <= 0 {
			delay = time.Millisecond
		}
		time.Sleep(delay)
	}
	// Metadata is diagnostic only: the OS lock is what protects the state.
	// Never fail an otherwise acquired lock because the file could not be
	// rewritten.
	writeLockMetadata(f)
	return &Lock{path: path, file: f}, nil
}

func writeLockMetadata(f *os.File) {
	if err := f.Truncate(0); err != nil {
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	token, err := lockToken()
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%d %s %s\n", os.Getpid(), token, time.Now().UTC().Format(time.RFC3339))
}

func lockHolder(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return ""
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return ""
	}
	return fmt.Sprintf(", held by pid %d", pid)
}

func lockToken() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate lock token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// Release unlocks the file. It is safe to call more than once.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	unlockErr := unlock(f)
	closeErr := f.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock %s: %w", l.path, unlockErr)
	}
	return closeErr
}
