package install

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/SuSonicTH/cpt/internal/state"
)

// GCOptions describes the directories cpt may prune. Empty directories are
// skipped so callers can use the same helper for partial layouts.
type GCOptions struct {
	BinDir   string
	TmpDir   string
	ProbeDir string
	SelfDir  string
	// State identifies the staged binaries that still belong to a package.
	State *state.State
	// BundlesDir holds bundle manifests, snapshots and temporary outputs.
	BundlesDir string
	// CurrentVersion keeps the matching cross-target cpt base binaries in the
	// self cache. "dev" and empty values disable version-based pruning.
	CurrentVersion string
	// Now is injectable for tests.
	Now time.Time
	// StagingMaxAge bounds leftover staging files. Zero uses 24 hours.
	StagingMaxAge time.Duration
	// ProbeMaxAge bounds cached version probes. Zero uses 30 days.
	ProbeMaxAge time.Duration
	// SelfMaxAge bounds old cross-target cpt binaries. Zero uses 30 days.
	SelfMaxAge time.Duration
	// BundleMaxAge bounds orphaned bundle snapshots and temporary files.
	// Zero uses 7 days.
	BundleMaxAge time.Duration
}

// GCResult reports what garbage collection removed.
type GCResult struct {
	Removed  []string `json:"removed,omitempty"`
	Bytes    int64    `json:"bytes,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// GC removes cpt-owned leftovers that can always be reconstructed: crash
// staging, expired probes, obsolete cpt base binaries and orphaned bundle
// artifacts. It never removes installed binaries, downloads, snippets or
// recovery backups.
func GC(opt GCOptions) GCResult {
	var res GCResult
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	stagingAge := opt.StagingMaxAge
	if stagingAge <= 0 {
		stagingAge = 24 * time.Hour
	}
	probeAge := opt.ProbeMaxAge
	if probeAge <= 0 {
		probeAge = 30 * 24 * time.Hour
	}
	selfAge := opt.SelfMaxAge
	if selfAge <= 0 {
		selfAge = 30 * 24 * time.Hour
	}
	bundleAge := opt.BundleMaxAge
	if bundleAge <= 0 {
		bundleAge = 7 * 24 * time.Hour
	}

	// Files in .cpt-tmp named bk-* are recovery copies from a failed rollback
	// and must survive until the user resolves them.
	clean := func(path string) {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		if err := os.Remove(path); err != nil {
			res.warn(fmt.Sprintf("could not remove %s: %v", path, err))
			return
		}
		res.removed(path, info.Size())
	}

	binTmp := filepath.Join(opt.BinDir, TmpDirName)
	if entries, err := os.ReadDir(binTmp); err == nil {
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || now.Sub(info.ModTime()) < stagingAge {
				continue
			}
			if strings.HasPrefix(e.Name(), backupPrefix) {
				continue
			}
			clean(filepath.Join(binTmp, e.Name()))
		}
		removeIfEmpty(binTmp)
	}

	if runtime.GOOS == "windows" {
		for _, orphan := range OrphanPending(opt.BinDir, opt.State) {
			info, err := os.Lstat(orphan)
			if err != nil || now.Sub(info.ModTime()) < stagingAge {
				continue
			}
			res.removeAll(orphan, info)
		}
	}

	if entries, err := os.ReadDir(opt.TmpDir); err == nil {
		for _, e := range entries {
			if !stagingName(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil || now.Sub(info.ModTime()) < stagingAge {
				continue
			}
			res.removeAll(filepath.Join(opt.TmpDir, e.Name()), info)
		}
	}

	if entries, err := os.ReadDir(opt.ProbeDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			info, err := e.Info()
			if err != nil || now.Sub(info.ModTime()) < probeAge {
				continue
			}
			clean(filepath.Join(opt.ProbeDir, e.Name()))
		}
	}

	version := strings.TrimPrefix(strings.TrimSpace(opt.CurrentVersion), "v")
	if version != "" && version != "dev" {
		if entries, err := os.ReadDir(opt.SelfDir); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasPrefix(e.Name(), "cpt_") {
					continue
				}
				info, err := e.Info()
				if err != nil || now.Sub(info.ModTime()) < selfAge {
					continue
				}
				if strings.Contains(e.Name(), "_"+version+"_") {
					continue
				}
				clean(filepath.Join(opt.SelfDir, e.Name()))
			}
		}
	}

	if entries, err := os.ReadDir(opt.BundlesDir); err == nil {
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || now.Sub(info.ModTime()) < bundleAge {
				continue
			}
			name := e.Name()
			switch {
			case strings.HasSuffix(name, ".old"), strings.HasPrefix(name, ".bundle-"):
				clean(filepath.Join(opt.BundlesDir, name))
			case e.IsDir() && strings.HasSuffix(name, ".d"):
				base := strings.TrimSuffix(name, ".d")
				if _, err := os.Stat(filepath.Join(opt.BundlesDir, base+".json")); os.IsNotExist(err) {
					res.removeAll(filepath.Join(opt.BundlesDir, name), info)
				}
			}
		}
	}

	return res
}

func (r *GCResult) removed(path string, size int64) {
	r.Removed = append(r.Removed, path)
	r.Bytes += size
}

func (r *GCResult) warn(msg string) { r.Warnings = append(r.Warnings, msg) }

func (r *GCResult) removeAll(path string, info os.FileInfo) {
	size := info.Size()
	if info.IsDir() {
		size = dirSize(path)
	}
	if err := os.RemoveAll(path); err != nil {
		r.warn(fmt.Sprintf("could not remove %s: %v", path, err))
		return
	}
	r.removed(path, size)
}

func dirSize(path string) int64 {
	var size int64
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.IsDir() {
			size += dirSize(filepath.Join(path, e.Name()))
		} else {
			size += info.Size()
		}
	}
	return size
}

func stagingName(name string) bool {
	for _, prefix := range []string{"stage-", "build-", "bundle-", "self-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func removeIfEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}
