//go:build windows

package state

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLock attempts a non-blocking exclusive LockFileEx on the first byte. The
// lock belongs to the handle and is released by the kernel when the process
// exits, so a crashed cpt cannot leave a stale lock behind.
func tryLock(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped))
}

func unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

func isLockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_IO_PENDING)
}
