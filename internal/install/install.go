// Package install places extracted binaries into the flat bin directory.
package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/SuSonicTH/cpt/internal/archive"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/fsutil"
	"github.com/SuSonicTH/cpt/internal/state"
)

// Manager installs binaries and maintains ownership records.
type Manager struct {
	BinDir string
	State  *state.State
	Force  bool
	// Warnings collects non-fatal notes such as pending Windows swaps.
	Warnings []string
}

// Place copies bins into the bin directory and updates ownership in State.
// It returns the installed binary names mapped to their content hashes.
func (m *Manager) Place(name string, bins []archive.Bin) (map[string]string, error) {
	if err := os.MkdirAll(m.BinDir, 0o755); err != nil {
		return nil, fmt.Errorf("create bin directory: %w", err)
	}
	installed := make(map[string]string, len(bins))
	for _, b := range bins {
		destName := b.Name
		if runtime.GOOS == "windows" && filepath.Ext(destName) == "" {
			destName += ".exe"
		}
		dest := filepath.Join(m.BinDir, destName)

		if owner, ok := m.State.Owner(destName); ok && owner != name && !m.Force {
			return nil, cerr.New(cerr.Conflict, "binary %q is already provided by %s (use --force to replace)", destName, owner)
		}
		if !fsutil.Exists(dest) {
			delete(m.State.Provides, destName)
		} else if _, owned := m.State.Owner(destName); !owned && !m.Force {
			return nil, cerr.New(cerr.Conflict, "%s already exists and is not managed by cpt (use --force to replace)", dest)
		}

		fallback, err := copyAtomic(b.Path, dest)
		if err != nil {
			return nil, err
		}
		if fallback != "" {
			m.Warnings = append(m.Warnings, fmt.Sprintf("%s is in use; new version written to %s", destName, fallback))
			installed[destName] = ""
			continue
		}
		hash, err := FileSHA256(dest)
		if err != nil {
			return nil, err
		}
		installed[destName] = hash
		m.State.Provides[destName] = name
	}

	// Drop binaries this package used to provide but no longer does.
	if prev, ok := m.State.Packages[name]; ok {
		for bin := range prev.Bins {
			if _, keep := installed[bin]; keep {
				continue
			}
			if owner, ok := m.State.Owner(bin); ok && owner == name {
				delete(m.State.Provides, bin)
				_ = os.Remove(filepath.Join(m.BinDir, bin))
			}
		}
	}
	return installed, nil
}

// copyAtomic copies src to dst, tolerating Windows sharing violations by
// writing the new content to dst+".new" instead. It returns the fallback path
// when that happened.
func copyAtomic(src, dst string) (string, error) {
	if err := fsutil.CopyFileAtomic(src, dst, 0o755); err == nil {
		return "", nil
	} else if runtime.GOOS != "windows" {
		return "", err
	}
	alt := dst + ".new"
	if err := fsutil.CopyFileAtomic(src, alt, 0o755); err != nil {
		return "", err
	}
	return alt, nil
}

// FileSHA256 returns the hex digest of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
