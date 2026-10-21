package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/config"
	"github.com/SuSonicTH/cpt/internal/install"
	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/platform"
	"github.com/SuSonicTH/cpt/internal/state"
)

// fixture builds an App over a temp root with one URL-based package per entry.
func fixture(t *testing.T, packages string) (*App, string) {
	t.Helper()
	root := t.TempDir()
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(a.Close)
	doc := fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`, packages)
	if err := os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return a, root
}

func urlPackage(name, url string, bins ...string) string {
	quoted := make([]string, 0, len(bins))
	for _, b := range bins {
		quoted = append(quoted, fmt.Sprintf("%q", b))
	}
	return fmt.Sprintf(`{"name":%q,"version":"1.0.0","source":{"type":"url","url":%q,"bins":[%s]}}`,
		name, url, joinStrings(quoted, ","))
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func installOne(t *testing.T, a *App, name string, opt InstallOptions) ([]Result, error) {
	t.Helper()
	var results []Result
	err := a.WithLock(func() error {
		var err error
		results, err = a.Install(context.Background(), []string{name}, opt)
		if err == nil {
			err = ResultsError(results)
		}
		return err
	})
	return results, err
}

func TestForceOwnershipTransferKeepsSharedBinary(t *testing.T) {
	content := tarGz(t, map[string]string{"tool": "#!/bin/sh\necho tool\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	a, root := fixture(t, urlPackage("alpha", srv.URL+"/a.tar.gz", "tool")+","+urlPackage("beta", srv.URL+"/b.tar.gz", "tool"))

	if _, err := installOne(t, a, "alpha", InstallOptions{}); err != nil {
		t.Fatalf("install alpha: %v", err)
	}
	if _, err := installOne(t, a, "beta", InstallOptions{}); err == nil || cerr.KindOf(err) != cerr.Conflict {
		t.Fatalf("expected a conflict, got %v", err)
	}
	if _, err := installOne(t, a, "beta", InstallOptions{Force: true}); err != nil {
		t.Fatalf("force install beta: %v", err)
	}
	if owner := a.State.Provides["tool"]; owner != "beta" {
		t.Fatalf("owner = %q, want beta", owner)
	}
	if _, ok := a.State.Packages["alpha"].Bins["tool"]; ok {
		t.Fatal("alpha still claims the transferred binary")
	}

	if err := a.WithLock(func() error {
		_, err := a.Remove(context.Background(), []string{"alpha"}, false)
		return err
	}); err != nil {
		t.Fatalf("remove alpha: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "tool")); err != nil {
		t.Fatalf("beta's binary was deleted: %v", err)
	}
	if a.State.Packages["beta"] == nil {
		t.Fatal("beta was removed")
	}
}

func TestPreflightConflictLeavesNoOrphan(t *testing.T) {
	content := tarGz(t, map[string]string{"aaa": "a", "zzz": "z"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, urlPackage("gamma", srv.URL+"/g.tar.gz", "aaa", "zzz"))
	if err := os.WriteFile(filepath.Join(root, "bin", "zzz"), []byte("unmanaged"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := installOne(t, a, "gamma", InstallOptions{})
	if err == nil || cerr.KindOf(err) != cerr.Conflict {
		t.Fatalf("expected a conflict, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "aaa")); !os.IsNotExist(err) {
		t.Fatalf("aaa was written despite the preflight failure: %v", err)
	}
	if _, ok := a.State.Provides["aaa"]; ok {
		t.Fatal("orphan provides entry for aaa")
	}
	if a.State.Packages["gamma"] != nil {
		t.Fatal("failed package was recorded")
	}
}

func TestRemoveDuplicateNamesDoesNotPanic(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, _ := fixture(t, urlPackage("mytool", srv.URL+"/t.tar.gz", "mytool"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := a.WithLock(func() error {
		_, err := a.Remove(context.Background(), []string{"mytool", "mytool"}, false)
		return err
	}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(a.State.Packages) != 0 {
		t.Fatalf("state still has packages: %+v", a.State.Packages)
	}
}

func TestRemoveModifiedRequiresForce(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "original"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, urlPackage("mytool", srv.URL+"/t.tar.gz", "mytool"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin", "mytool")
	if err := os.WriteFile(bin, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.WithLock(func() error {
		_, err := a.Remove(context.Background(), []string{"mytool"}, false)
		return err
	}); err == nil {
		t.Fatal("expected removal of a modified binary to fail")
	}
	if err := a.WithLock(func() error {
		_, err := a.Remove(context.Background(), []string{"mytool"}, true)
		return err
	}); err != nil {
		t.Fatalf("forced Remove: %v", err)
	}
}

func TestNilStateEntriesAreDropped(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "lists"), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := `{"schema":1,"packages":{"ghost":null},"lists":{"core":null,"other":null},"provides":{"tool":""}}`
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	if _, err := a.Doctor(); err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if _, err := a.Verify(nil); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	_ = a.Lists()
	if a.State.Packages["ghost"] != nil {
		t.Fatal("nil package entry was kept")
	}
}

func TestOfflineInstallRefusesDownload(t *testing.T) {
	root := t.TempDir()
	a, err := Open(Options{Root: root, Quiet: true, Offline: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	doc := `{"schema":1,"name":"test","packages":[{"name":"mytool","version":"1.0.0","source":{"type":"url","url":"https://example.invalid/t.tar.gz","bins":["mytool"]}}]}`
	os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(doc), 0o644)

	err = a.WithLock(func() error {
		_, err := a.Install(context.Background(), []string{"mytool"}, InstallOptions{})
		return err
	})
	if err == nil {
		t.Fatal("offline install should fail")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("offline")) {
		t.Fatalf("error does not mention offline: %v", err)
	}
}

func TestReinstallIsNoOpButRepairsMissingBinary(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, urlPackage("mytool", srv.URL+"/t.tar.gz", "mytool"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	results, err := installOne(t, a, "mytool", InstallOptions{SkipCurrent: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Action != "up-to-date" {
		t.Fatalf("expected up-to-date, got %+v", results)
	}

	if err := os.Remove(filepath.Join(root, "bin", "mytool")); err != nil {
		t.Fatal(err)
	}
	results, err = installOne(t, a, "mytool", InstallOptions{SkipCurrent: true})
	if err != nil {
		t.Fatalf("repair install: %v", err)
	}
	if results[0].Action == "up-to-date" {
		t.Fatal("missing binary was not repaired")
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "mytool")); err != nil {
		t.Fatalf("binary not restored: %v", err)
	}
}

func TestRowsReportsConflicts(t *testing.T) {
	root := t.TempDir()
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	pkg := `{"name":"mytool","version":"1.0.0","source":{"type":"url","url":"https://x/y","bins":["mytool"]}}`
	for _, name := range []string{"one", "two"} {
		doc := fmt.Sprintf(`{"schema":1,"name":%q,"packages":[%s]}`, name, pkg)
		os.WriteFile(filepath.Join(root, "lists", name+".json"), []byte(doc), 0o644)
	}
	rows, err := a.Rows(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Name == "mytool" && r.Conflict {
			found = true
		}
	}
	if !found {
		t.Fatalf("conflict not reported: %+v", rows)
	}
}

func TestLintTreatsHTTPErrorsAsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	a := &App{
		Cfg:    &config.Config{Parallel: 1, HTTP: config.HTTP{Retries: 1, TimeoutSeconds: 5}},
		Client: srv.Client(),
	}
	entry := list.Entry{Pkg: &list.Package{
		Name:    "mytool",
		Version: "1.0.0",
		Source:  list.Source{Type: "url", URL: srv.URL + "/asset", Bins: []string{"mytool"}},
	}}
	res := a.lintOne(context.Background(), entry, "linux/amd64", platform.Platform{OS: "linux", Arch: "amd64"})
	if res.Error == "" {
		t.Fatalf("403 was treated as success: %+v", res)
	}
	if res.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Status)
	}
}

func TestRemovePreflightsAllBinaries(t *testing.T) {
	content := tarGz(t, map[string]string{"aaa": "a", "zzz": "z"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, urlPackage("mytool", srv.URL+"/t.tar.gz", "aaa", "zzz"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "zzz"), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.WithLock(func() error {
		_, err := a.Remove(context.Background(), []string{"mytool"}, false)
		return err
	}); err == nil {
		t.Fatal("expected removal to fail")
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "aaa")); err != nil {
		t.Fatalf("aaa was deleted before preflight finished: %v", err)
	}
	if a.State.Packages["mytool"] == nil {
		t.Fatal("state was partially updated")
	}
}

func TestInstallRollsBackOnLateFailure(t *testing.T) {
	content := tarGz(t, map[string]string{"aaa": "a", "zzz": "z"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, urlPackage("mytool", srv.URL+"/t.tar.gz", "aaa", "zzz"))
	// A directory at a destination cannot be replaced even with --force.
	if err := os.Mkdir(filepath.Join(root, "bin", "zzz"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := installOne(t, a, "mytool", InstallOptions{Force: true})
	if err == nil {
		t.Fatal("expected install to fail")
	}
	if _, statErr := os.Stat(filepath.Join(root, "bin", "aaa")); !os.IsNotExist(statErr) {
		t.Fatalf("aaa was left behind after rollback: %v", statErr)
	}
	if a.State.Packages["mytool"] != nil {
		t.Fatal("failed install was recorded")
	}
}

func TestLintFallsBackFrom405(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Write([]byte("a large enough body"))
	}))
	defer srv.Close()

	a := &App{Cfg: &config.Config{Parallel: 1, HTTP: config.HTTP{Retries: 1, TimeoutSeconds: 5}}, Client: srv.Client()}
	entry := list.Entry{Pkg: &list.Package{
		Name:    "mytool",
		Version: "1.0.0",
		Source:  list.Source{Type: "url", URL: srv.URL + "/asset", Bins: []string{"mytool"}},
	}}
	res := a.lintOne(context.Background(), entry, "linux/amd64", platform.Platform{OS: "linux", Arch: "amd64"})
	if res.Error != "" {
		t.Fatalf("405 fallback failed: %+v", res)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Status)
	}
}

func TestOfflineReinstallWorksWithoutProbeCache(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"bin", "lists", "cache/probe", "logs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	binPath := filepath.Join(root, "bin", "mytool")
	if err := os.WriteFile(binPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	hash, err := install.FileSHA256(binPath)
	if err != nil {
		t.Fatal(err)
	}
	st := state.New()
	st.Packages["mytool"] = &state.Package{
		Version: "1.0.0",
		Tag:     "v1.0.0",
		Bins:    map[string]string{"mytool": hash},
	}
	st.Provides["mytool"] = "mytool"
	if err := st.Save(filepath.Join(root, "state.json")); err != nil {
		t.Fatal(err)
	}
	plat := platform.Current()
	doc := fmt.Sprintf(`{"schema":1,"name":"test","packages":[{"name":"mytool","source":{"type":"github","repo":"acme/tool","bins":["mytool"],"asset":{%q:"tool-{tag}"}}}]}`, plat.String())
	if err := os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := Open(Options{Root: root, Offline: true, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	results, err := installOne(t, a, "mytool", InstallOptions{SkipCurrent: true})
	if err != nil {
		t.Fatalf("offline reinstall should be a no-op: %v", err)
	}
	if len(results) != 1 || results[0].Action != "up-to-date" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestVersionlessFallbackNeverOverridesAPin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/acme/tool/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/acme/tool/releases/tag/v2.0.0", http.StatusFound)
	})
	mux.HandleFunc("/acme/tool/releases/download/v2.0.0/tool", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("latest-bytes"))
	})
	mux.HandleFunc("GET /acme/tool/releases/latest/download/tool", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("latest-bytes"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	doc := fmt.Sprintf(`{"schema":1,"name":"test","packages":[{"name":"tool","source":{"type":"github","repo":"acme/tool","bins":["tool"],"asset":{%q:"tool"}}}]}`, platform.Current().String())

	root := t.TempDir()
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Prober.BaseURL = srv.URL
	a.Prober.Client = srv.Client()
	a.Client = srv.Client()
	if err := os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = installOne(t, a, "tool@v1.0.0", InstallOptions{})
	if err == nil {
		t.Fatal("a pinned version-less install must not fall back to latest")
	}
	if _, statErr := os.Stat(filepath.Join(root, "bin", "tool")); !os.IsNotExist(statErr) {
		t.Fatalf("latest bytes were installed for a pinned version: %v", statErr)
	}
}
