package install

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SuSonicTH/cpt/internal/archive"
	"github.com/SuSonicTH/cpt/internal/state"
)

func TestPlaceRollsBackOverwrittenBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relies on ENAMETOOLONG for a late rename failure")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(binDir, "aaa")
	if err := os.WriteFile(oldPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	srcA := filepath.Join(dir, "src-a")
	srcB := filepath.Join(dir, "src-b")
	os.WriteFile(srcA, []byte("new-a"), 0o755)
	os.WriteFile(srcB, []byte("new-b"), 0o755)

	st := state.New()
	m := &Manager{BinDir: binDir, State: st}
	long := strings.Repeat("x", 300)
	_, err := m.Place("pkg", []archive.Bin{
		{Name: "aaa", Path: srcA},
		{Name: long, Path: srcB},
	})
	if err == nil {
		t.Fatal("expected the long destination name to fail")
	}
	got, readErr := os.ReadFile(oldPath)
	if readErr != nil {
		t.Fatalf("pre-existing binary was not restored: %v", readErr)
	}
	if string(got) != "old" {
		t.Fatalf("pre-existing binary content = %q, want old", got)
	}
	if len(st.Provides) != 0 {
		t.Fatalf("ownership was recorded for a failed install: %+v", st.Provides)
	}
}

func TestStartupCleanupPreservesRecoveryBackups(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	tmpDir := filepath.Join(binDir, TmpDirName)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "stage-old"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(tmpDir, "bk-keep")
	if err := os.WriteFile(keep, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := &Manager{BinDir: binDir, State: state.New()}
	if _, err := m.Place("pkg", []archive.Bin{{Name: "tool", Path: src}}); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("recovery backup was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "stage-old")); !os.IsNotExist(err) {
		t.Fatalf("stale staging file survived: %v", err)
	}
}
