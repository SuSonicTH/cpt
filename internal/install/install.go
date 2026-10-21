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

type target struct {
	name string
	dest string
	src  string
}

// Place installs bins into the bin directory and updates ownership in State.
// All conflicts are detected before any file is written. It returns the
// installed binary names mapped to their content hashes.
func (m *Manager) Place(name string, bins []archive.Bin) (map[string]string, error) {
	if err := os.MkdirAll(m.BinDir, 0o755); err != nil {
		return nil, fmt.Errorf("create bin directory: %w", err)
	}

	targets, err := m.preflight(name, bins)
	if err != nil {
		return nil, err
	}

	// Stage every copy first so a failure cannot leave a half-installed package.
	temps := make([]string, 0, len(targets))
	cleanup := func() {
		for _, tmp := range temps {
			if tmp != "" {
				_ = os.Remove(tmp)
			}
		}
	}
	for _, t := range targets {
		tmp, err := stageCopy(t.src, m.BinDir)
		if err != nil {
			cleanup()
			return nil, err
		}
		temps = append(temps, tmp)
	}

	installed := make(map[string]string, len(targets))

	// Preserve existing binaries so a late failure can restore them.
	backups := make([]string, len(targets))
	for i, t := range targets {
		if info, err := os.Lstat(t.dest); err == nil && info.Mode().IsRegular() {
			b, err := stageCopy(t.dest, m.BinDir)
			if err != nil {
				cleanup()
				for _, prev := range backups {
					if prev != "" {
						_ = os.Remove(prev)
					}
				}
				return nil, err
			}
			backups[i] = b
		}
	}
	removeBackups := func() {
		for _, b := range backups {
			if b != "" {
				_ = os.Remove(b)
			}
		}
	}
	var committed []string
	rollback := func() {
		cleanup()
		for _, path := range committed {
			_ = os.Remove(path)
		}
		for i, b := range backups {
			if b != "" {
				_ = os.Rename(b, targets[i].dest)
			}
		}
	}
	for i, t := range targets {
		if err := os.Rename(temps[i], t.dest); err != nil {
			if runtime.GOOS != "windows" {
				rollback()
				return nil, fmt.Errorf("install %s: %w", t.name, err)
			}
			alt := t.dest + ".new"
			if err2 := os.Rename(temps[i], alt); err2 != nil {
				rollback()
				return nil, fmt.Errorf("install %s: %w", t.name, err)
			}
			m.Warnings = append(m.Warnings, fmt.Sprintf("%s is in use; new version written to %s", t.name, filepath.Base(alt)))
			installed[t.name] = ""
			temps[i] = ""
			committed = append(committed, alt)
			continue
		}
		temps[i] = ""
		committed = append(committed, t.dest)

		hash, err := FileSHA256(t.dest)
		if err != nil {
			rollback()
			return nil, err
		}
		installed[t.name] = hash
	}
	removeBackups()

	for bin := range installed {
		if prev, ok := m.State.Owner(bin); ok && prev != name {
			if p := m.State.Packages[prev]; p != nil {
				delete(p.Bins, bin)
			}
		}
		m.State.Provides[bin] = name
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

// preflight validates every destination without touching the filesystem.
func (m *Manager) preflight(name string, bins []archive.Bin) ([]target, error) {
	targets := make([]target, 0, len(bins))
	seen := make(map[string]bool, len(bins))
	for _, b := range bins {
		destName := b.Name
		if runtime.GOOS == "windows" && filepath.Ext(destName) == "" {
			destName += ".exe"
		}
		if seen[destName] {
			return nil, fmt.Errorf("package %s provides %s twice", name, destName)
		}
		seen[destName] = true

		dest := filepath.Join(m.BinDir, destName)
		if owner, ok := m.State.Owner(destName); ok && owner != name && !m.Force {
			return nil, cerr.New(cerr.Conflict,
				"binary %q is already provided by %s (installing %s; use --force to replace)", destName, owner, name)
		}
		if info, err := os.Lstat(dest); err == nil {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s exists and is not a regular file", dest)
			}
			if _, owned := m.State.Owner(destName); !owned && !m.Force {
				return nil, cerr.New(cerr.Conflict,
					"%s already exists and is not managed by cpt (use --force to replace)", dest)
			}
		}
		targets = append(targets, target{name: destName, dest: dest, src: b.Path})
	}
	return targets, nil
}

// stageCopy copies src to a temporary file in dir, ready to be renamed.
func stageCopy(src, dir string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()

	f, err := os.CreateTemp(dir, ".cpt-stage-*")
	if err != nil {
		return "", fmt.Errorf("create staging file in %s: %w", dir, err)
	}
	tmp := f.Name()
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("stage %s: %w", filepath.Base(src), err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
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
