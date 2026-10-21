package install

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/SuSonicTH/cpt/internal/state"
)

func TestGCPrunesOnlyReconstructibleFiles(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	binTmp := filepath.Join(binDir, TmpDirName)
	for _, dir := range []string{
		binTmp,
		filepath.Join(root, "tmp"),
		filepath.Join(root, "cache", "probe"),
		filepath.Join(root, "cache", "self"),
		filepath.Join(root, "bundles"),
		filepath.Join(root, "bundles", "old.d"),
		filepath.Join(root, "bundles", "keep.d"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	writeOld := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	writeOld(filepath.Join(binTmp, "stage-old"))
	writeOld(filepath.Join(binTmp, "bk-keep"))
	writeOld(filepath.Join(root, "tmp", "stage-old"))
	if err := os.WriteFile(filepath.Join(root, "tmp", "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeOld(filepath.Join(root, "cache", "probe", "deadbeef.json"))
	if err := os.WriteFile(filepath.Join(root, "cache", "probe", "fresh.json"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeOld(filepath.Join(root, "cache", "self", "cpt_0.9.0_linux_amd64"))
	writeOld(filepath.Join(root, "cache", "self", "cpt_1.2.3_darwin_arm64"))
	writeOld(filepath.Join(root, "bundles", "devbox-linux-amd64.old"))
	if err := os.Chtimes(filepath.Join(root, "bundles", "old.d"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "bundles", "keep.d"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bundles", "keep.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "bundles", "keep.json"), old, old); err != nil {
		t.Fatal(err)
	}

	res := GC(GCOptions{
		BinDir:         binDir,
		TmpDir:         filepath.Join(root, "tmp"),
		ProbeDir:       filepath.Join(root, "cache", "probe"),
		SelfDir:        filepath.Join(root, "cache", "self"),
		BundlesDir:     filepath.Join(root, "bundles"),
		CurrentVersion: "1.2.3",
		Now:            time.Now(),
	})
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	for _, gone := range []string{
		filepath.Join(binTmp, "stage-old"),
		filepath.Join(root, "tmp", "stage-old"),
		filepath.Join(root, "cache", "probe", "deadbeef.json"),
		filepath.Join(root, "cache", "self", "cpt_0.9.0_linux_amd64"),
		filepath.Join(root, "bundles", "devbox-linux-amd64.old"),
		filepath.Join(root, "bundles", "old.d"),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s was not removed: %v", gone, err)
		}
	}
	for _, kept := range []string{
		filepath.Join(binTmp, "bk-keep"),
		filepath.Join(root, "tmp", "keep"),
		filepath.Join(root, "cache", "probe", "fresh.json"),
		filepath.Join(root, "cache", "self", "cpt_1.2.3_darwin_arm64"),
		filepath.Join(root, "bundles", "keep.d"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s should have been kept: %v", kept, err)
		}
	}
	if _, err := os.Stat(binTmp); err != nil {
		t.Errorf("bin staging directory with a recovery backup was removed: %v", err)
	}
}

func TestGCRemovesOrphanedStagedBinaries(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("staged .new files are only produced on Windows")
	}
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	orphan := filepath.Join(binDir, "dropped.new.exe")
	legacyOrphan := filepath.Join(binDir, "gone.exe.new")
	keep := PendingPath(filepath.Join(binDir, "kept.exe"))
	self := PendingPath(filepath.Join(binDir, "cpt.exe"))
	for _, path := range []string{orphan, legacyOrphan, keep, self} {
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	st := state.New()
	st.Packages["kept"] = &state.Package{Bins: map[string]string{"kept.exe": "hash"}}

	res := GC(GCOptions{BinDir: binDir, State: st, Now: time.Now(), StagingMaxAge: time.Hour})
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	for _, path := range []string{orphan, legacyOrphan} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("orphan %s was not removed: %v", path, err)
		}
	}
	for _, path := range []string{keep, self} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("managed staged file %s was removed: %v", path, err)
		}
	}
}
