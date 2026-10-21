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

func TestPendingPath(t *testing.T) {
	cases := map[string]string{
		"fzf.exe":          "fzf.new.exe",
		"fzf":              "fzf.new",
		"tool.v2.exe":      "tool.v2.new.exe",
		"no-extension-bin": "no-extension-bin.new",
	}
	for in, want := range cases {
		if got := PendingPath(in); got != want {
			t.Errorf("PendingPath(%q) = %q, want %q", in, got, want)
		}
	}
	if !IsPendingName("fzf.new.exe") || !IsPendingName("tool.new") {
		t.Fatal("staged names were not recognised")
	}
	if IsPendingName("fzf.exe") || IsPendingName("newtool.exe") {
		t.Fatal("regular names were treated as staged")
	}
	got := PendingCandidates("fzf.exe")
	if len(got) != 2 || got[0] != "fzf.new.exe" || got[1] != "fzf.exe.new" {
		t.Fatalf("PendingCandidates = %v", got)
	}
}

func TestSwapLegacyPendingWithoutRecordedHash(t *testing.T) {
	binDir := t.TempDir()
	dest := filepath.Join(binDir, "fzf.exe")
	legacy := dest + ".new"
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy-new"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := state.New()
	st.Packages["fzf"] = &state.Package{Bins: map[string]string{"fzf.exe": ""}}

	swapped, warnings := SwapPending(binDir, st)
	if len(warnings) != 0 || len(swapped) != 1 {
		t.Fatalf("legacy swap: swapped=%v warnings=%v", swapped, warnings)
	}
	if got := mustReadFile(t, dest); got != "legacy-new" {
		t.Fatalf("destination = %q", got)
	}
	if hash := st.Packages["fzf"].Bins["fzf.exe"]; hash == "" {
		t.Fatal("legacy swap did not adopt a hash")
	}
}

func TestSwapPending(t *testing.T) {
	binDir := t.TempDir()
	dest := filepath.Join(binDir, "fzf.exe")
	pending := PendingPath(dest)
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	hash, err := FileSHA256(pending)
	if err != nil {
		t.Fatal(err)
	}
	st := state.New()
	st.Packages["fzf"] = &state.Package{Bins: map[string]string{"fzf.exe": hash}}

	swapped, warnings := SwapPending(binDir, st)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(swapped) != 1 || swapped[0] != "fzf.exe" {
		t.Fatalf("swapped = %v", swapped)
	}
	if got := mustReadFile(t, dest); got != "new" {
		t.Fatalf("destination = %q, want new", got)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("staged file still present: %v", err)
	}
	if HasPending(binDir, st) {
		t.Fatal("HasPending still reports work")
	}
}

func TestSwapPendingRefusesMismatchedHash(t *testing.T) {
	binDir := t.TempDir()
	dest := filepath.Join(binDir, "fzf")
	pending := PendingPath(dest)
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := state.New()
	st.Packages["fzf"] = &state.Package{Bins: map[string]string{"fzf": strings.Repeat("0", 64)}}

	swapped, warnings := SwapPending(binDir, st)
	if len(swapped) != 0 || len(warnings) == 0 {
		t.Fatalf("mismatched staged file was activated: swapped=%v warnings=%v", swapped, warnings)
	}
	if got := mustReadFile(t, dest); got != "old" {
		t.Fatalf("destination changed to %q", got)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("staged file should remain for inspection: %v", err)
	}
}

func TestPlaceRemovesStalePending(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("staged .new files are only produced on Windows")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(binDir, "fzf.exe")
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PendingPath(dest), []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("fresh"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := &Manager{BinDir: binDir, State: state.New(), Force: true}
	if _, err := m.Place("fzf", []archive.Bin{{Name: "fzf.exe", Path: src}}); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if got := mustReadFile(t, dest); got != "fresh" {
		t.Fatalf("destination = %q", got)
	}
	if _, err := os.Stat(PendingPath(dest)); !os.IsNotExist(err) {
		t.Fatalf("stale staged file survived: %v", err)
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
