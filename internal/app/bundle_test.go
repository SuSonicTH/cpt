package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SuSonicTH/cpt/internal/bundle"
	"github.com/SuSonicTH/cpt/internal/state"
)

func TestBuildBundleAndRunIt(t *testing.T) {
	content := tarGz(t, map[string]string{"tool": "#!/bin/sh\necho bundled\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	a, root := fixture(t, urlPackage("tool", srv.URL+"/tool.tar.gz", "tool"))
	base := filepath.Join(t.TempDir(), "base")
	if err := os.WriteFile(base, []byte("FAKE-BASE-BINARY\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "devbox")
	res, err := a.BuildBundle(context.Background(), BundleOptions{
		Name:     "devbox",
		Packages: []string{"tool"},
		Frozen:   true,
		BasePath: base,
		Output:   out,
	})
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	if res.Size == 0 || res.SHA256 == "" || !res.Frozen {
		t.Fatalf("unexpected result: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "bundles", "devbox.json")); err != nil {
		t.Fatalf("manifest not saved: %v", err)
	}

	infos, err := a.Bundles()
	if err != nil || len(infos) != 1 || infos[0].Packages != 1 {
		t.Fatalf("Bundles: %v %+v", err, infos)
	}
	if len(infos[0].PackageNames) != 1 || infos[0].PackageNames[0] != "tool" {
		t.Fatalf("bundle package names: %+v", infos[0].PackageNames)
	}

	dest := filepath.Join(t.TempDir(), ".cpt")
	run, err := bundle.Run(context.Background(), res.Path, bundle.RunOptions{Dest: dest})
	if err != nil {
		t.Fatalf("bundle.Run: %v", err)
	}
	if run.Packages[0].Version != "1.0.0" {
		t.Fatalf("bundle version: %+v", run.Packages)
	}
	data, err := os.ReadFile(filepath.Join(dest, "bin", binName("tool")))
	if err != nil || string(data) != "#!/bin/sh\necho bundled\n" {
		t.Fatalf("installed binary: %v %q", err, data)
	}

	// A frozen package explains itself instead of updating.
	a2, err := Open(Options{Root: dest, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Open dest: %v", err)
	}
	defer a2.Close()
	results, err := a2.Update(context.Background(), []string{"tool"}, false, false)
	if err != nil {
		t.Fatalf("Update on frozen: %v", err)
	}
	if len(results) != 1 || results[0].Action != "skipped (frozen)" {
		t.Fatalf("frozen update result: %+v", results)
	}
	if changed := a2.Unfreeze([]string{"test"}); len(changed) != 1 {
		t.Fatalf("Unfreeze: %v", changed)
	}
}

func TestBuildBundleUsesStatePin(t *testing.T) {
	content := tarGz(t, map[string]string{"tool": "#!/bin/sh\necho pinned\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	a, root := fixture(t, urlPackage("tool", srv.URL+"/tool.tar.gz", "tool"))
	a.State.Packages["tool"] = &state.Package{Version: "1.0.0", Pinned: "0.9.0"}
	base := filepath.Join(t.TempDir(), "base")
	if err := os.WriteFile(base, []byte("BASE"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BuildBundle(context.Background(), BundleOptions{
		Name: "pinned", Packages: []string{"tool"}, BasePath: base,
	}); err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	m, err := bundle.LoadManifest(filepath.Join(root, "bundles", "pinned.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Packages) != 1 || m.Packages[0].Version != "0.9.0" {
		t.Fatalf("bundle ignored the state pin: %+v", m.Packages)
	}
}

func TestRebuildPreservesShellAndSnapshot(t *testing.T) {
	content := tarGz(t, map[string]string{"tool": "#!/bin/sh\necho shell\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	pkg := fmt.Sprintf(`{"name":"tool","version":"1.0.0","source":{"type":"url","url":%q,"bins":["tool"]},"shell":{"bash":{"functions":"export BUNDLED=1"}}}`, srv.URL+"/tool.tar.gz")
	a, root := fixture(t, pkg)
	a.State.Shell["tool"] = &state.Shell{
		Approved: true,
		Live: &state.ShellSnippet{
			Version: "1.0.0",
			Bash:    "export BUNDLED=1\n",
			Pwsh:    "$env:BUNDLED = 1\n",
		},
	}
	base := filepath.Join(t.TempDir(), "base")
	if err := os.WriteFile(base, []byte("BASE"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BuildBundle(context.Background(), BundleOptions{
		Name: "shellb", Packages: []string{"tool"}, BasePath: base,
	}); err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	m, err := bundle.LoadManifest(filepath.Join(root, "bundles", "shellb.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Packages[0].Shell == nil {
		t.Fatal("shell snippet missing from the manifest")
	}
	// Rebuild must not depend on the builder still having the snippet/state.
	a.State.Shell = map[string]*state.Shell{}
	if _, err := a.RebuildBundle(context.Background(), "shellb"); err != nil {
		t.Fatalf("RebuildBundle: %v", err)
	}
	m, err = bundle.LoadManifest(filepath.Join(root, "bundles", "shellb.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Packages[0].Shell == nil || !strings.Contains(m.Packages[0].Shell.Bash, "BUNDLED") {
		t.Fatalf("rebuild dropped the shell snippet: %+v", m.Packages[0].Shell)
	}
}

func TestRebuildBundleUsesSavedVersions(t *testing.T) {
	content := tarGz(t, map[string]string{"tool": "#!/bin/sh\necho v1\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	a, _ := fixture(t, urlPackage("tool", srv.URL+"/tool.tar.gz", "tool"))
	base := filepath.Join(t.TempDir(), "base")
	if err := os.WriteFile(base, []byte("BASE"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BuildBundle(context.Background(), BundleOptions{
		Name: "devbox", Packages: []string{"tool"}, BasePath: base,
	}); err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	res, err := a.RebuildBundle(context.Background(), "devbox")
	if err != nil {
		t.Fatalf("RebuildBundle: %v", err)
	}
	if len(res.Packages) != 1 {
		t.Fatalf("rebuilt packages: %+v", res.Packages)
	}
	if _, err := a.RemoveBundle("devbox"); err != nil {
		t.Fatalf("RemoveBundle: %v", err)
	}
	if infos, _ := a.Bundles(); len(infos) != 0 {
		t.Fatalf("bundle still listed: %+v", infos)
	}
}
