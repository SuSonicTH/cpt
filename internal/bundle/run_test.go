package bundle

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SuSonicTH/cpt/internal/install"
	"github.com/SuSonicTH/cpt/internal/state"
)

func runnableBundle(t *testing.T, frozen bool) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	body := "#!/bin/sh\necho bundled\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	listData := []byte(`{"schema":1,"name":"core","packages":[]}`)
	m := testManifest("devbox", frozen)
	m.Packages = []ManifestPackage{{
		Name: "tool", List: "core", Version: "1.0.0", Tag: "v1.0.0",
		Asset: "tool.tar.gz", DownloadSHA256: strings.Repeat("a", 64),
		Bins: []string{binName("tool")}, ChecksumStatus: "tofu",
	}}
	m.Lists = map[string]ManifestList{"core": {SHA256: hashFor(listData), Origin: "https://example.com/core.json"}}
	return buildTestFile(t, dir, "BASE-EXECUTABLE\n", Payload{
		Manifest: m,
		Lists:    map[string][]byte{"core": listData},
		Bins:     []PayloadBin{{Name: binName("tool"), Path: bin}},
	})
}

func TestRunInstallsAndIsIdempotent(t *testing.T) {
	path := runnableBundle(t, true)
	dest := filepath.Join(t.TempDir(), "home", ".cpt")

	res, err := Run(context.Background(), path, RunOptions{Dest: dest})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Frozen || len(res.Packages) != 1 || res.Packages[0].Action != "installed" {
		t.Fatalf("unexpected result: %+v", res)
	}

	data, err := os.ReadFile(filepath.Join(dest, "bin", binName("tool")))
	if err != nil || string(data) != "#!/bin/sh\necho bundled\n" {
		t.Fatalf("binary: %v %q", err, data)
	}
	cpt, err := os.ReadFile(filepath.Join(dest, "bin", cptName()))
	if err != nil || string(cpt) != "BASE-EXECUTABLE\n" {
		t.Fatalf("cpt base: %v %q", err, cpt)
	}
	if _, err := os.Stat(filepath.Join(dest, "cptinit.sh")); err != nil {
		t.Fatalf("cptinit.sh missing: %v", err)
	}

	st, err := state.Load(filepath.Join(dest, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	p := st.Packages["tool"]
	if p == nil || p.Origin != "bundle" || p.Version != "1.0.0" {
		t.Fatalf("package state: %+v", p)
	}
	if meta := st.Lists["core"]; meta == nil || !meta.Readonly {
		t.Fatalf("frozen list metadata: %+v", meta)
	}

	res2, err := Run(context.Background(), path, RunOptions{Dest: dest})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.Packages[0].Action != "unchanged" {
		t.Fatalf("second run action = %q, want unchanged", res2.Packages[0].Action)
	}
}

func TestRunPreservesUnrelatedTmpFiles(t *testing.T) {
	path := runnableBundle(t, false)
	dest := filepath.Join(t.TempDir(), ".cpt")
	if _, err := Run(context.Background(), path, RunOptions{Dest: dest}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	victim := filepath.Join(dest, "tmp", "victim.txt")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), path, RunOptions{Dest: dest}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("unrelated tmp file was removed: %v", err)
	}
}

func TestRunRefusesUnrelatedNonEmptyDest(t *testing.T) {
	path := runnableBundle(t, false)
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "important.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), path, RunOptions{Dest: dest}); err == nil {
		t.Fatal("bundle installed into an unrelated non-empty directory")
	}
	if _, err := os.Stat(filepath.Join(dest, "important.txt")); err != nil {
		t.Fatalf("existing file was touched: %v", err)
	}
}

func TestRunRejectsTargetMismatchBeforeWriting(t *testing.T) {
	path := runnableBundle(t, false)
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the manifest target by rebuilding the file with a foreign target.
	c.Close()
	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	listData := []byte(`{"schema":1,"name":"core","packages":[]}`)
	m := testManifest("foreign", false)
	if runtime.GOOS == "windows" {
		m.Target = "linux/amd64"
	} else {
		m.Target = "windows/amd64"
	}
	m.Packages = []ManifestPackage{{Name: "tool", List: "core", Version: "1", Tag: "v1", Asset: "a", DownloadSHA256: strings.Repeat("b", 64), Bins: []string{binName("tool")}}}
	m.Lists = map[string]ManifestList{"core": {SHA256: hashFor(listData)}}
	foreign := buildTestFile(t, dir, "base", Payload{
		Manifest: m,
		Lists:    map[string][]byte{"core": listData},
		Bins:     []PayloadBin{{Name: binName("tool"), Path: bin}},
	})
	dest := filepath.Join(t.TempDir(), "missing")
	if _, err := Run(context.Background(), foreign, RunOptions{Dest: dest}); err == nil {
		t.Fatal("foreign target accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("target check wrote to the destination: %v", err)
	}
}

func TestRunDryRunAndListWriteNothing(t *testing.T) {
	path := runnableBundle(t, true)
	dest := filepath.Join(t.TempDir(), "unused")
	for _, opt := range []RunOptions{{Dest: dest, List: true}, {Dest: dest, DryRun: true}} {
		res, err := Run(context.Background(), path, opt)
		if err != nil {
			t.Fatalf("Run(%+v): %v", opt, err)
		}
		if len(res.Packages) != 1 {
			t.Fatalf("inspect result: %+v", res)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatalf("inspection created %s", dest)
		}
	}
}

func cptName() string {
	if runtime.GOOS == "windows" {
		return "cpt.exe"
	}
	return "cpt"
}

func binName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func TestSwapPendingSelfPath(t *testing.T) {
	dir := t.TempDir()
	for _, legacy := range []bool{false, true} {
		name := "cpt.exe"
		if legacy {
			name = "cpt-legacy.exe"
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		pending := install.PendingPath(path)
		other := path + ".new"
		if legacy {
			pending, other = path+".new", install.PendingPath(path)
		}
		if err := os.WriteFile(pending, []byte("new"), 0o755); err != nil {
			t.Fatal(err)
		}
		if !legacy {
			// A stale legacy candidate must not survive a successful swap.
			if err := os.WriteFile(other, []byte("stale"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := swapPendingSelfPath(path); err != nil {
			t.Fatalf("swapPendingSelfPath(legacy=%v): %v", legacy, err)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "new" {
			t.Fatalf("legacy=%v content = %q, err = %v", legacy, data, err)
		}
		if _, err := os.Stat(pending); !os.IsNotExist(err) {
			t.Fatalf("pending still present: %v", err)
		}
		if !legacy {
			if _, err := os.Stat(other); !os.IsNotExist(err) {
				t.Fatalf("stale alternate pending survived: %v", err)
			}
		}
	}
}
