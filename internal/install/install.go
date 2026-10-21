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
	"sort"
	"strings"

	"github.com/SuSonicTH/cpt/internal/archive"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/state"
)

// tmpDirName holds staging and backup files inside the bin directory. It is
// cleaned at the start of every install and is on the same filesystem as the
// destinations, so renames stay atomic.
const TmpDirName = ".cpt-tmp"

// backupPrefix marks copies of replaced binaries kept for recovery. Unlike
// staging files, they survive the next install.
const backupPrefix = "bk-"

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

// PendingPath returns the temporary name used when a Windows binary cannot be
// replaced because it is running: fzf.exe becomes fzf.new.exe. On other
// platforms the name is unused but harmless.
func PendingPath(dest string) string {
	ext := filepath.Ext(dest)
	return strings.TrimSuffix(dest, ext) + ".new" + ext
}

// PendingCandidates lists every staged-update name understood by this build.
// Older cpt releases used <dest>.new; accepting both lets an upgrade activate
// or clean up a staged binary left behind by the previous build.
func PendingCandidates(dest string) []string {
	legacy := dest + ".new"
	current := PendingPath(dest)
	if legacy == current {
		return []string{current}
	}
	return []string{current, legacy}
}

// IsPendingName reports whether a bin-directory entry uses either staged
// update naming convention.
func IsPendingName(name string) bool {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	return strings.HasSuffix(base, ".new") || strings.HasSuffix(name, ".new")
}

// FindPending returns the first staged replacement for dest and its hash.
// os.ErrNotExist means there is no staged file; other errors mean a staged
// path exists but cannot be used (for example a directory or unreadable file).
func FindPending(dest string) (path, hash string, err error) {
	for _, candidate := range PendingCandidates(dest) {
		info, statErr := os.Lstat(candidate)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return "", "", statErr
		}
		if !info.Mode().IsRegular() {
			return "", "", fmt.Errorf("%s is not a regular file", candidate)
		}
		hash, err := FileSHA256(candidate)
		if err != nil {
			return "", "", err
		}
		return candidate, hash, nil
	}
	return "", "", os.ErrNotExist
}

// OrphanPending lists staged-update files in binDir that no longer belong to a
// recorded package binary. They can appear when a new version drops a bin while
// it is still running, or when a package was removed on another platform.
func OrphanPending(binDir string, st *state.State) []string {
	expected := map[string]bool{}
	// cpt installs itself (and bundle mode stages a clean copy) outside the
	// package ownership map. Those staged files are managed by
	// bundle.SwapPendingSelf, never by package GC.
	for _, name := range []string{"cpt", "cpt.exe"} {
		for _, candidate := range PendingCandidates(filepath.Join(binDir, name)) {
			expected[candidate] = true
		}
	}
	if st != nil {
		for _, p := range st.Packages {
			if p == nil {
				continue
			}
			for bin := range p.Bins {
				for _, candidate := range PendingCandidates(filepath.Join(binDir, bin)) {
					expected[candidate] = true
				}
			}
		}
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !IsPendingName(e.Name()) {
			continue
		}
		path := filepath.Join(binDir, e.Name())
		if !expected[path] {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// HasPending reports whether any recorded binary has a staged replacement.
func HasPending(binDir string, st *state.State) bool {
	if st == nil {
		return false
	}
	for _, p := range st.Packages {
		if p == nil {
			continue
		}
		for bin := range p.Bins {
			if _, _, err := FindPending(filepath.Join(binDir, bin)); err == nil {
				return true
			}
		}
	}
	return false
}

// SwapPending activates binaries that were staged as *.new.exe because they
// were running during installation. It is idempotent and leaves a staged file
// in place when the destination is still locked, so a later start can retry.
func SwapPending(binDir string, st *state.State) (swapped, warnings []string) {
	if st == nil {
		return nil, nil
	}
	names := make([]string, 0, len(st.Packages))
	for name := range st.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, pkg := range names {
		p := st.Packages[pkg]
		if p == nil {
			continue
		}
		bins := make([]string, 0, len(p.Bins))
		for bin := range p.Bins {
			bins = append(bins, bin)
		}
		sort.Strings(bins)
		for _, bin := range bins {
			dest := filepath.Join(binDir, bin)
			pending, hash, err := FindPending(dest)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("not activating staged %s: %v", bin, err))
				continue
			}
			want := p.Bins[bin]
			if want != "" && !strings.EqualFold(hash, want) {
				warnings = append(warnings, fmt.Sprintf(
					"not activating %s: its hash does not match state; remove the package or run cpt doctor",
					filepath.Base(pending)))
				continue
			}
			if err := os.Rename(pending, dest); err != nil {
				warnings = append(warnings, fmt.Sprintf(
					"could not activate %s; the destination may be locked or read-only (close any running copy and restart cpt)", bin))
				continue
			}
			// Do not leave an older candidate behind that the next start
			// would activate over the version just installed.
			for _, other := range PendingCandidates(dest) {
				if other != pending {
					_ = os.Remove(other)
				}
			}
			// Legacy staged entries had no recorded hash. Adopt the bytes we
			// actually activated so verify and future installs can trust them.
			p.Bins[bin] = hash
			swapped = append(swapped, bin)
		}
	}
	return swapped, warnings
}

// Place installs bins into the bin directory and updates ownership in State.
// All conflicts are detected before any file is written. Existing binaries are
// backed up so a late failure can restore them.
func (m *Manager) Place(name string, bins []archive.Bin) (map[string]string, error) {
	if err := os.MkdirAll(m.BinDir, 0o755); err != nil {
		return nil, fmt.Errorf("create bin directory: %w", err)
	}
	tmpDir := filepath.Join(m.BinDir, TmpDirName)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	if entries, err := os.ReadDir(tmpDir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), backupPrefix) {
				continue // recovery copy from a failed restore
			}
			_ = os.RemoveAll(filepath.Join(tmpDir, e.Name()))
		}
	}

	targets, err := m.preflight(name, bins)
	if err != nil {
		return nil, err
	}
	// A stale staged update must never outlive a successful install: renames
	// into place below make the destination current, and SwapPending would
	// otherwise activate this older file on the next start. Staged names only
	// exist on Windows; elsewhere leaving user files named *.new alone.
	if runtime.GOOS == "windows" {
		for _, t := range targets {
			for _, pending := range PendingCandidates(t.dest) {
				if err := os.Remove(pending); err != nil && !os.IsNotExist(err) {
					return nil, fmt.Errorf("remove stale staged binary %s: %w", pending, err)
				}
			}
		}
	}

	temps := make([]string, 0, len(targets))
	cleanup := func() {
		for _, tmp := range temps {
			if tmp != "" {
				_ = os.Remove(tmp)
			}
		}
	}
	for _, t := range targets {
		tmp, err := stageCopy(t.src, tmpDir, "stage-")
		if err != nil {
			cleanup()
			return nil, err
		}
		temps = append(temps, tmp)
	}

	backups := make([]string, len(targets))
	for i, t := range targets {
		if info, err := os.Lstat(t.dest); err == nil && info.Mode().IsRegular() {
			b, err := stageCopy(t.dest, tmpDir, backupPrefix)
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
	rollback := func() error {
		cleanup()
		for _, path := range committed {
			_ = os.Remove(path)
		}
		var restoreFailed []string
		for i, b := range backups {
			if b == "" {
				continue
			}
			if err := os.Rename(b, targets[i].dest); err != nil {
				restoreFailed = append(restoreFailed, fmt.Sprintf("%s (backup kept at %s)", targets[i].dest, b))
				continue
			}
			backups[i] = ""
		}
		if len(restoreFailed) > 0 {
			return fmt.Errorf("could not restore %s", joinComma(restoreFailed))
		}
		return nil
	}

	installed := make(map[string]string, len(targets))
	for i, t := range targets {
		if err := os.Rename(temps[i], t.dest); err != nil {
			if runtime.GOOS != "windows" || !renameBlockedByInUse(err) {
				if rbErr := rollback(); rbErr != nil {
					return nil, fmt.Errorf("install %s: %w; %v", t.name, err, rbErr)
				}
				return nil, fmt.Errorf("install %s: %w", t.name, err)
			}
			alt := PendingPath(t.dest)
			_ = os.Remove(alt)
			if err2 := os.Rename(temps[i], alt); err2 != nil {
				if rbErr := rollback(); rbErr != nil {
					return nil, fmt.Errorf("install %s: %w; %v", t.name, err, rbErr)
				}
				return nil, fmt.Errorf("install %s: %w", t.name, err)
			}
			hash, hashErr := FileSHA256(alt)
			if hashErr != nil {
				if rbErr := rollback(); rbErr != nil {
					return nil, fmt.Errorf("install %s: %w; %v", t.name, hashErr, rbErr)
				}
				return nil, fmt.Errorf("hash %s: %w", t.name, hashErr)
			}
			m.Warnings = append(m.Warnings, fmt.Sprintf(
				"%s could not be replaced; the new version is staged as %s and will be applied on the next cpt start (close the running program or fix permissions)",
				t.name, filepath.Base(alt)))
			installed[t.name] = hash
			temps[i] = ""
			committed = append(committed, alt)
			continue
		}
		temps[i] = ""
		committed = append(committed, t.dest)

		hash, err := FileSHA256(t.dest)
		if err != nil {
			if rbErr := rollback(); rbErr != nil {
				return nil, fmt.Errorf("hash %s: %w; %v", t.name, err, rbErr)
			}
			return nil, err
		}
		installed[t.name] = hash
	}
	removeBackups()
	if entries, err := os.ReadDir(tmpDir); err == nil && len(entries) == 0 {
		_ = os.Remove(tmpDir)
	}

	for bin := range installed {
		if prev, ok := m.State.Owner(bin); ok && prev != name {
			state.DeleteBin(m.State.Packages[prev], bin)
		}
		m.State.SetOwner(bin, name)
	}

	// Drop binaries this package used to provide but no longer does.
	if prev, ok := m.State.Packages[name]; ok {
		for bin := range prev.Bins {
			if hasBin(installed, bin) {
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
		key := destName
		if runtime.GOOS == "windows" {
			key = strings.ToLower(destName)
		}
		if seen[key] {
			return nil, fmt.Errorf("package %s provides %s twice", name, destName)
		}
		seen[key] = true

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
func stageCopy(src, dir, prefix string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()

	f, err := os.CreateTemp(dir, prefix+"*")
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

func hasBin(installed map[string]string, bin string) bool {
	for name := range installed {
		if name == bin || (runtime.GOOS == "windows" && strings.EqualFold(name, bin)) {
			return true
		}
	}
	return false
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
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

// RecoveryBackups lists retained backups from failed rollbacks.
func RecoveryBackups(binDir string) []string {
	entries, err := os.ReadDir(filepath.Join(binDir, TmpDirName))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), backupPrefix) {
			out = append(out, filepath.Join(binDir, TmpDirName, e.Name()))
		}
	}
	return out
}

// StaleStaging lists leftover staging files that are safe for GC to remove.
func StaleStaging(binDir string) []string {
	entries, err := os.ReadDir(filepath.Join(binDir, TmpDirName))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), backupPrefix) {
			continue
		}
		out = append(out, filepath.Join(binDir, TmpDirName, e.Name()))
	}
	return out
}
