//go:build windows

package install

import (
	"errors"

	"golang.org/x/sys/windows"
)

// renameBlockedByInUse reports whether a rename failed because the destination
// is a running executable. Access-denied and sharing violations are also
// produced by read-only files and antivirus, so callers must still surface the
// staged-update warning until a later attempt can replace the file.
func renameBlockedByInUse(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_USER_MAPPED_FILE)
}
