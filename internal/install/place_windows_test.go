//go:build windows

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/SuSonicTH/cpt/internal/archive"
	"github.com/SuSonicTH/cpt/internal/state"
)

// TestPlaceStagesLockedBinary exercises the real Windows branch: a destination
// opened without delete sharing cannot be replaced, so Place must write the
// new bytes as <name>.new.exe and SwapPending must activate them later.
func TestPlaceStagesLockedBinary(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(binDir, "tool.exe")
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("fresh"), 0o755); err != nil {
		t.Fatal(err)
	}

	name, err := windows.UTF16PtrFromString(dest)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = windows.CloseHandle(h)
		}
	}()

	m := &Manager{BinDir: binDir, State: state.New(), Force: true}
	installed, err := m.Place("tool", []archive.Bin{{Name: "tool.exe", Path: src}})
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	// The caller normally records the package after Place; SwapPending only
	// considers recorded binaries.
	m.State.Packages["tool"] = &state.Package{Version: "1.0.0", Bins: installed}
	pending := PendingPath(dest)
	if got := mustReadFile(t, pending); got != "fresh" {
		t.Fatalf("staged content = %q", got)
	}
	if installed["tool.exe"] == "" {
		t.Fatal("staged binary has no recorded hash")
	}
	if len(m.Warnings) == 0 || !strings.Contains(m.Warnings[0], "staged") {
		t.Fatalf("missing staging warning: %v", m.Warnings)
	}

	if err := windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
	closed = true
	swapped, warnings := SwapPending(binDir, m.State)
	if len(warnings) != 0 || len(swapped) != 1 || swapped[0] != "tool.exe" {
		t.Fatalf("SwapPending: swapped=%v warnings=%v", swapped, warnings)
	}
	if got := mustReadFile(t, dest); got != "fresh" {
		t.Fatalf("destination = %q", got)
	}
}
