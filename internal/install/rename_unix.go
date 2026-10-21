//go:build !windows

package install

// renameBlockedByInUse reports whether a rename failed because the destination
// is a running executable. Only Windows locks running images this way.
func renameBlockedByInUse(error) bool { return false }
