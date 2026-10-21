package list

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SuSonicTH/cpt/internal/fsutil"
)

// FileName returns the on-disk file name for a list identifier.
func FileName(name string) string { return Slug(name) + ".json" }

// ReadFile loads and validates one list file.
func ReadFile(path string) (*List, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	l, err := Parse(data, path)
	if err != nil {
		return nil, nil, err
	}
	return l, data, nil
}

// Write stores raw list bytes atomically after validation.
func Write(dir, name string, raw []byte) (*List, error) {
	l, err := Parse(raw, name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create list directory: %w", err)
	}
	path := filepath.Join(dir, FileName(name))
	if err := fsutil.WriteFileAtomic(path, raw, 0o644); err != nil {
		return nil, err
	}
	return l, nil
}

// EnsureBuiltin writes the embedded master list when no copy exists yet.
// It never overwrites a refreshed copy.
func EnsureBuiltin(dir string, builtin []byte) error {
	path := filepath.Join(dir, FileName("core"))
	if fsutil.Exists(path) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create list directory: %w", err)
	}
	return fsutil.WriteFileAtomic(path, builtin, 0o644)
}

// PackageNames returns the package names of a list in file order.
func PackageNames(l *List) []string {
	out := make([]string, 0, len(l.Packages))
	for _, p := range l.Packages {
		out = append(out, p.Name)
	}
	return out
}

// TrimSpaceAll is a helper for callers that need to normalise user input.
func TrimSpaceAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
