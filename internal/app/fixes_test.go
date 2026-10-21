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
	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/platform"
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
