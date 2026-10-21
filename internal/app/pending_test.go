package app

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SuSonicTH/cpt/internal/config"
	"github.com/SuSonicTH/cpt/internal/install"
	"github.com/SuSonicTH/cpt/internal/shellinit"
	"github.com/SuSonicTH/cpt/internal/state"
)

func TestOpenActivatesStagedBinary(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("staged .new files are only produced on Windows")
	}
	root := t.TempDir()
	paths := config.NewPaths(root)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(paths.Bin, "tool")
	pending := install.PendingPath(dest)
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	hash, err := install.FileSHA256(pending)
	if err != nil {
		t.Fatal(err)
	}
	st := state.New()
	st.Packages["tool"] = &state.Package{Version: "2.0.0", Bins: map[string]string{"tool": hash}}
	st.Provides["tool"] = "tool"
	if err := st.Save(paths.StateFile); err != nil {
		t.Fatal(err)
	}

	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("staged binary was not activated: %q", data)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("staged file still present: %v", err)
	}

	verify, err := a.Verify([]string{"tool"})
	if err != nil {
		t.Fatal(err)
	}
	if len(verify) != 1 || verify[0].Status != "ok" {
		t.Fatalf("unexpected verify result: %+v", verify)
	}
}

func TestVerifyAndDoctorReportPendingBinary(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("staged .new files are only produced on Windows")
	}
	root := t.TempDir()
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	dest := filepath.Join(a.BinDir(), "tool")
	pending := install.PendingPath(dest)
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	hash, err := install.FileSHA256(pending)
	if err != nil {
		t.Fatal(err)
	}
	a.State.Packages["tool"] = &state.Package{Version: "2.0.0", Bins: map[string]string{"tool": hash}}
	a.State.SetOwner("tool", "tool")

	verify, err := a.Verify([]string{"tool"})
	if err != nil {
		t.Fatal(err)
	}
	if len(verify) != 1 || verify[0].Status != "pending" {
		t.Fatalf("unexpected verify result: %+v", verify)
	}
	checks, err := a.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range checks {
		if c.Name == "pending-swap" && strings.Contains(c.Detail, "staged update") {
			found = true
		}
	}
	if !found {
		t.Fatalf("doctor did not report the staged update: %+v", checks)
	}
}

func TestGeneratedPowerShellInitHasBOM(t *testing.T) {
	root := t.TempDir()
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	data, err := os.ReadFile(a.InitScriptPath(shellinit.FlavorPwsh))
	if err != nil {
		t.Fatal(err)
	}
	if !shellinit.HasUTF8BOM(data) {
		t.Fatalf("cptinit.ps1 has no UTF-8 BOM: %x", data[:minInt(len(data), 4)])
	}
	if got, want := shellinit.DecodeScript(data), a.InitScript(shellinit.FlavorPwsh); got != want {
		t.Fatalf("cptinit.ps1 content differs:\n%q\n%q", got, want)
	}
	bash, err := os.ReadFile(a.InitScriptPath(shellinit.FlavorBash))
	if err != nil {
		t.Fatal(err)
	}
	if shellinit.HasUTF8BOM(bash) {
		t.Fatal("cptinit.sh should not have a BOM")
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
