package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SuSonicTH/cpt/internal/fsutil"
	"github.com/SuSonicTH/cpt/internal/shellinit"
	"github.com/SuSonicTH/cpt/internal/state"
)

func shellPackage(name, url, functions string) string {
	return fmt.Sprintf(`{"name":%q,"version":"1.0.0","source":{"type":"url","url":%q,"bins":[%q]},
	  "shell":{"bash":{"functions":%q,"env":{"%s_HOME":"/opt/%s"},"path":["/opt/%s/bin"]}}}`,
		name, url, name, functions, strings.ToUpper(name), name, name)
}

func readInit(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func TestInstallApprovesFirstShellSnippet(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "#!/bin/sh\necho mytool\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool='mytool --fast'\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}

	sh := a.State.Shell["mytool"]
	if sh == nil || sh.Live == nil || sh.Pending != nil {
		t.Fatalf("snippet was not approved on first install: %+v", sh)
	}
	if !sh.Approved || sh.SnippetSHA256 == "" {
		t.Fatalf("unexpected approval state: %+v", sh)
	}
	if sh.Live.Version != "1.0.0" {
		t.Fatalf("live version = %q", sh.Live.Version)
	}

	script := readInit(t, root, "cptinit.sh")
	for _, want := range []string{
		"# >>> cpt:mytool v1.0.0 sha256:",
		"alias mytool='mytool --fast'",
		"export MYTOOL_HOME='/opt/mytool'",
		"/opt/mytool/bin",
		"# <<< cpt:mytool",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("cptinit.sh is missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(readInit(t, root, "cptinit.ps1"), "cpt:mytool") {
		t.Fatal("bash snippet leaked into the pwsh script")
	}
	if !strings.Contains(a.InitScript(shellinit.FlavorBash), "cpt:mytool") {
		t.Fatal("InitScript does not render the approved snippet")
	}
}

func TestInstallShellSnippetFromURL(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	snippet := []byte("alias mytool='from-url'\n")
	sum := sha256.Sum256(snippet)
	mux := http.NewServeMux()
	mux.HandleFunc("/t.tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(content) })
	mux.HandleFunc("/snippet.sh", func(w http.ResponseWriter, r *http.Request) { w.Write(snippet) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pkg := fmt.Sprintf(`{"name":"mytool","version":"1.0.0",
	  "source":{"type":"url","url":%q,"bins":["mytool"]},
	  "shell":{"bash":{"functions_url":%q,"functions_sha256":%q}}}`,
		srv.URL+"/t.tar.gz", srv.URL+"/snippet.sh", hex.EncodeToString(sum[:]))
	a, root := fixture(t, pkg)
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	sh := a.State.Shell["mytool"]
	if sh == nil || sh.Live == nil || !strings.Contains(sh.Live.Bash, "from-url") {
		t.Fatalf("URL snippet was not approved: %+v", sh)
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "alias mytool='from-url'") {
		t.Fatalf("URL snippet was not rendered:\n%s", got)
	}
	cached, err := os.ReadFile(filepath.Join(root, "cache", "shell", hex.EncodeToString(sum[:])))
	if err != nil || string(cached) != string(snippet) {
		t.Fatalf("snippet was not cached: %v", err)
	}
}

func TestListRefreshQueuesChangedSnippetUntilApproved(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	mux := http.NewServeMux()
	mux.HandleFunc("/t.tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(content) })
	var doc, base atomic.Value
	doc.Store(shellListDoc("alias mytool='old'\n"))
	base.Store("")
	mux.HandleFunc("/cpt.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.ReplaceAll(doc.Load().(string), "SERVER", base.Load().(string))))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base.Store(srv.URL)

	root := t.TempDir()
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	initial := strings.ReplaceAll(doc.Load().(string), "SERVER", base.Load().(string))
	if err := os.WriteFile(filepath.Join(root, "lists", "remote.json"), []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	a.State.Lists["remote"] = &state.List{Origin: srv.URL + "/cpt.json", FetchedAt: time.Now().UTC(), Packages: 1}
	if err := a.State.Save(filepath.Join(root, "state.json")); err != nil {
		t.Fatal(err)
	}

	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "alias mytool='old'") {
		t.Fatalf("initial snippet missing:\n%s", got)
	}

	doc.Store(shellListDoc("alias mytool='new'\n"))
	if err := a.WithLock(func() error {
		_, err := a.Refresh(context.Background(), []string{"remote"}, true)
		return err
	}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	sh := a.State.Shell["mytool"]
	if sh == nil || sh.Pending == nil || sh.Approved {
		t.Fatalf("changed snippet was not queued: %+v", sh)
	}
	if !strings.Contains(sh.Live.Bash, "old") || !strings.Contains(sh.Pending.Bash, "new") {
		t.Fatalf("wrong live/pending split: live=%q pending=%q", sh.Live.Bash, sh.Pending.Bash)
	}
	script := readInit(t, root, "cptinit.sh")
	if !strings.Contains(script, "alias mytool='old'") || strings.Contains(script, "alias mytool='new'") {
		t.Fatalf("pending snippet was sourced before approval:\n%s", script)
	}
	if n := a.PendingShellCount(); n != 1 {
		t.Fatalf("PendingShellCount = %d", n)
	}
	infos := a.ShellInfos()
	if len(infos) != 1 || !infos[0].Pending || !infos[0].Live {
		t.Fatalf("ShellInfos = %+v", infos)
	}
	diff, err := a.ShellDiff("mytool")
	if err != nil {
		t.Fatalf("ShellDiff: %v", err)
	}
	if !strings.Contains(diff, "-alias mytool='old'") || !strings.Contains(diff, "+alias mytool='new'") {
		t.Fatalf("unexpected diff:\n%s", diff)
	}

	if err := a.WithLock(func() error {
		_, err := a.ApproveShell([]string{"mytool"}, false)
		return err
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	sh = a.State.Shell["mytool"]
	if sh.Pending != nil || !sh.Approved || !strings.Contains(sh.Live.Bash, "new") {
		t.Fatalf("approval did not promote the snippet: %+v", sh)
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "alias mytool='new'") {
		t.Fatalf("approved snippet was not rendered:\n%s", got)
	}

	// Reject discards a later change and keeps the approved snippet.
	doc.Store(shellListDoc("alias mytool='third'\n"))
	if err := a.WithLock(func() error {
		_, err := a.Refresh(context.Background(), []string{"remote"}, true)
		return err
	}); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if a.State.Shell["mytool"].Pending == nil {
		t.Fatal("second change was not queued")
	}
	if err := a.WithLock(func() error {
		_, err := a.RejectShell(nil, true)
		return err
	}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	sh = a.State.Shell["mytool"]
	if sh.Pending != nil || !strings.Contains(sh.Live.Bash, "new") {
		t.Fatalf("reject changed the live snippet: %+v", sh)
	}
	if got := readInit(t, root, "cptinit.sh"); strings.Contains(got, "third") {
		t.Fatalf("rejected snippet was rendered:\n%s", got)
	}
}

func shellListDoc(functions string) string {
	return fmt.Sprintf(`{"schema":1,"name":"remote","packages":[{"name":"mytool","version":"1.0.0",
	  "source":{"type":"url","url":"%s/t.tar.gz","bins":["mytool"]},
	  "shell":{"bash":{"functions":%q}}}]}`, "SERVER", functions)
}

func TestRemoveDeletesShellSnippet(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := a.WithLock(func() error {
		_, err := a.Remove(context.Background(), []string{"mytool"}, false)
		return err
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := a.State.Shell["mytool"]; ok {
		t.Fatal("shell state survived removal")
	}
	if got := readInit(t, root, "cptinit.sh"); strings.Contains(got, "cpt:mytool") {
		t.Fatalf("removed snippet is still rendered:\n%s", got)
	}
}

func TestAllowShellPackagesKillSwitch(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	pkg := shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n")
	doc := fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`, pkg)
	if err := os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	sh := a.State.Shell["mytool"]
	if sh == nil || !sh.SyncPending || sh.Live != nil {
		t.Fatalf("disabled install did not record a pending sync: %+v", sh)
	}
	if got := readInit(t, root, "cptinit.sh"); strings.Contains(got, "cpt:mytool") {
		t.Fatalf("snippet was rendered while disabled:\n%s", got)
	}

	// Enabling the kill switch again does not execute unapproved remote code.
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a.Cfg.AllowShellPackages = true
	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	if sh := a.State.Shell["mytool"]; sh == nil || sh.Pending == nil || sh.Live != nil {
		t.Fatalf("expected a pending first snippet, got %+v", sh)
	}
	if err := a.WithLock(func() error {
		_, err := a.ApproveShell([]string{"mytool"}, false)
		return err
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "cpt:mytool") {
		t.Fatalf("approved snippet was not rendered:\n%s", got)
	}

	a.Cfg.AllowShellPackages = false
	if err := a.RegenerateInit(); err != nil {
		t.Fatal(err)
	}
	if got := readInit(t, root, "cptinit.sh"); strings.Contains(got, "cpt:mytool") {
		t.Fatalf("kill switch did not remove rendered snippets:\n%s", got)
	}
}

func writeListDoc(t *testing.T, root, name, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "lists", name+".json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUpToDateInstallApprovesFirstSnippet(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, urlPackage("mytool", srv.URL+"/t.tar.gz", "mytool"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.State.Shell["mytool"]; ok {
		t.Fatal("package unexpectedly has shell state")
	}

	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n")))
	results, err := installOne(t, a, "mytool", InstallOptions{SkipCurrent: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Action != "up-to-date" {
		t.Fatalf("expected up-to-date, got %+v", results)
	}
	if sh := a.State.Shell["mytool"]; sh == nil || sh.Live == nil {
		t.Fatalf("explicit up-to-date install did not approve the first snippet: %+v", sh)
	}
}

func TestRejectedSnippetIsNotRequeued(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "old\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackage("mytool", srv.URL+"/t.tar.gz", "new\n")))
	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	if err := a.WithLock(func() error {
		_, err := a.RejectShell([]string{"mytool"}, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sh := a.State.Shell["mytool"]
	if sh.Pending != nil || sh.RejectedSHA256 == "" {
		t.Fatalf("rejection was not recorded: %+v", sh)
	}

	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	if sh := a.State.Shell["mytool"]; sh.Pending != nil {
		t.Fatal("rejected candidate was queued again")
	}
	if _, err := installOne(t, a, "mytool", InstallOptions{SkipCurrent: true}); err != nil {
		t.Fatal(err)
	}
	if sh := a.State.Shell["mytool"]; sh.Pending != nil {
		t.Fatal("an unrelated install re-queued the rejected candidate")
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "old") {
		t.Fatalf("rejection changed the live snippet:\n%s", got)
	}

	// reset clears the rejection and offers the same candidate again.
	if err := a.WithLock(func() error {
		_, err := a.ResetShellRejection(context.Background(), []string{"mytool"}, false)
		return err
	}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if sh := a.State.Shell["mytool"]; sh.Pending == nil || !strings.Contains(sh.Pending.Bash, "new\n") {
		t.Fatalf("reset did not re-offer the candidate: %+v", sh)
	}

	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackage("mytool", srv.URL+"/t.tar.gz", "newer\n")))
	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	if sh := a.State.Shell["mytool"]; sh.Pending == nil || !strings.Contains(sh.Pending.Bash, "newer") {
		t.Fatal("a genuinely new candidate was not offered")
	}
}

func TestRemovedSnippetRequiresApproval(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		urlPackage("mytool", srv.URL+"/t.tar.gz", "mytool")))
	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	sh := a.State.Shell["mytool"]
	if sh == nil || sh.Pending == nil {
		t.Fatalf("removal was not queued: %+v", sh)
	}
	infos := a.ShellInfos()
	if len(infos) != 1 || !infos[0].Removal || infos[0].Approved {
		t.Fatalf("removal not visible in status: %+v", infos)
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "cpt:mytool") {
		t.Fatalf("removal ran before approval:\n%s", got)
	}

	if err := a.WithLock(func() error {
		_, err := a.ApproveShell([]string{"mytool"}, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.State.Shell["mytool"]; ok {
		t.Fatal("approved removal left shell state behind")
	}
	if got := readInit(t, root, "cptinit.sh"); strings.Contains(got, "cpt:mytool") {
		t.Fatalf("removal was not rendered:\n%s", got)
	}
}

func TestRemoveListQueuesSnippetRemoval(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n"))
	a.State.Lists["test"] = &state.List{Origin: "https://example.invalid/cpt.json", FetchedAt: time.Now().UTC(), Packages: 1}
	if err := a.State.Save(filepath.Join(root, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := a.WithLock(func() error {
		if _, err := a.RemoveList(context.Background(), "test"); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if sh := a.State.Shell["mytool"]; sh == nil || sh.Pending == nil {
		t.Fatalf("removing the list did not queue snippet removal: %+v", sh)
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "cpt:mytool") {
		t.Fatalf("removal ran before approval:\n%s", got)
	}
}

func TestPinnedUpdateStillQueuesSnippetRemoval(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	a.State.Packages["mytool"].Pinned = "1.0.0"
	if err := a.State.Save(filepath.Join(root, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "lists", "test.json")); err != nil {
		t.Fatal(err)
	}
	if err := a.WithLock(func() error {
		_, err := a.Update(context.Background(), nil, false, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if sh := a.State.Shell["mytool"]; sh == nil || sh.Pending == nil {
		t.Fatalf("pinned update skipped shell sync: %+v", sh)
	}
}

func TestRejectedRemovalKeepsLiveSnippet(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		urlPackage("mytool", srv.URL+"/t.tar.gz", "mytool")))
	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	if err := a.WithLock(func() error {
		_, err := a.RejectShell([]string{"mytool"}, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	sh := a.State.Shell["mytool"]
	if sh.Pending != nil || sh.Live == nil {
		t.Fatalf("rejected removal changed live state: %+v", sh)
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "cpt:mytool") {
		t.Fatalf("live snippet disappeared after rejecting removal:\n%s", got)
	}
}

func TestShellResolutionErrorIsReportedAndRecovers(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	mux := http.NewServeMux()
	mux.HandleFunc("/t.tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(content) })
	mux.HandleFunc("/snippet.sh", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("old\n")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sum := sha256.Sum256([]byte("old\n"))
	a, root := fixture(t, shellPackageURL("mytool", srv.URL+"/t.tar.gz", srv.URL+"/snippet.sh", hex.EncodeToString(sum[:])))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if sh := a.State.Shell["mytool"]; sh == nil || sh.Live == nil {
		t.Fatalf("install did not approve the snippet: %+v", sh)
	}

	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackageURL("mytool", srv.URL+"/t.tar.gz", srv.URL+"/missing.sh", strings.Repeat("b", 64))))
	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	sh := a.State.Shell["mytool"]
	if sh.LastError == "" || sh.Live == nil {
		t.Fatalf("resolution failure was not recorded: %+v", sh)
	}
	infos := a.ShellInfos()
	if len(infos) != 1 || infos[0].LastError == "" {
		t.Fatalf("status does not expose the error: %+v", infos)
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "old") {
		t.Fatalf("live snippet was lost:\n%s", got)
	}

	// Pointing the list back at the live body clears the error.
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackageURL("mytool", srv.URL+"/t.tar.gz", srv.URL+"/snippet.sh", hex.EncodeToString(sum[:]))))
	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	if sh := a.State.Shell["mytool"]; sh.LastError != "" {
		t.Fatalf("error was not cleared: %+v", sh)
	}
}

func TestResolutionErrorClearsStalePending(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		urlPackage("mytool", srv.URL+"/t.tar.gz", "mytool")))
	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	if sh := a.State.Shell["mytool"]; sh == nil || sh.Pending == nil {
		t.Fatalf("removal was not queued: %+v", sh)
	}

	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackageURL("mytool", srv.URL+"/t.tar.gz", srv.URL+"/missing.sh", strings.Repeat("c", 64))))
	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	sh := a.State.Shell["mytool"]
	if sh.LastError == "" {
		t.Fatal("resolution error was not recorded")
	}
	if sh.Pending != nil {
		t.Fatalf("stale removal remained approvable: %+v", sh)
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "cpt:mytool") {
		t.Fatalf("live snippet was lost on resolution error:\n%s", got)
	}
}

func shellPackageURL(name, assetURL, snippetURL, sha string) string {
	return fmt.Sprintf(`{"name":%q,"version":"1.0.0","source":{"type":"url","url":%q,"bins":[%q]},
	  "shell":{"bash":{"functions_url":%q,"functions_sha256":%q}}}`,
		name, assetURL, name, snippetURL, sha)
}

func TestDryRunDoesNotQueueShellSnippets(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, urlPackage("mytool", srv.URL+"/t.tar.gz", "mytool"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n")))
	if err := a.WithLock(func() error {
		_, err := a.Install(context.Background(), []string{"mytool"}, InstallOptions{
			DryRun: true, SkipCurrent: true, ApproveNewShell: true,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.State.Shell["mytool"]; ok {
		t.Fatal("dry-run mutated shell approval state")
	}
}

func TestOpenDoesNotOverwriteExistingInit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cptinit.sh")
	if err := os.WriteFile(path, []byte("# custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if got := readInit(t, root, "cptinit.sh"); got != "# custom\n" {
		t.Fatalf("Open overwrote an existing script: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "cptinit.ps1")); err != nil {
		t.Fatalf("missing init file was not created: %v", err)
	}
}

func TestOpenReplacesDanglingInitSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	root := t.TempDir()
	link := filepath.Join(root, "cptinit.sh")
	if err := os.Symlink(filepath.Join(root, "missing-target"), link); err != nil {
		t.Fatal(err)
	}
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("dangling symlink was not replaced")
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "# generated by cpt") {
		t.Fatalf("generated script missing:\n%s", got)
	}
}

func TestOpenReconcilesKillSwitch(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "cpt:mytool") {
		t.Fatalf("snippet was not rendered:\n%s", got)
	}
	a.Close()

	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a2, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if got := readInit(t, root, "cptinit.sh"); strings.Contains(got, "cpt:mytool") {
		t.Fatalf("a read-only open did not apply the kill switch:\n%s", got)
	}
	a2.Close()

	// A list change made while snippets were disabled must be queued when they
	// are enabled again, not silently ignored.
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool='changed'\n")))
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a3, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a3.Close()
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "cpt:mytool") {
		t.Fatalf("re-enabling snippets did not restore them:\n%s", got)
	}
	if sh := a3.State.Shell["mytool"]; sh == nil || sh.Pending == nil || !strings.Contains(sh.Pending.Bash, "changed") {
		t.Fatalf("change made while disabled was lost: %+v", sh)
	}
}

func TestConfigChangeDuringInstallDisablesApproval(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	// Open with the default enabled config, then flip the switch while the
	// process is running but before install syncs snippets.
	root := t.TempDir()
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n")))
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if sh := a.State.Shell["mytool"]; sh == nil || sh.Live != nil || !sh.SyncPending {
		t.Fatalf("install auto-approved a snippet after the kill switch was enabled: %+v", sh)
	}
	if got := readInit(t, root, "cptinit.sh"); strings.Contains(got, "cpt:mytool") {
		t.Fatalf("snippet was rendered after the kill switch was enabled:\n%s", got)
	}
}

func TestConfigChangeDuringSnippetResolution(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	newSnippet := []byte("new\n")
	sum := sha256.Sum256(newSnippet)
	started := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/t.tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(content) })
	mux.HandleFunc("/slow.sh", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Write(newSnippet)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "old\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackageURL("mytool", srv.URL+"/t.tar.gz", srv.URL+"/slow.sh", hex.EncodeToString(sum[:]))))

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.WithLock(func() error { return a.SyncShell(context.Background(), nil) })
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("snippet fetch did not start")
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("sync: %v", err)
	}

	saved, err := state.Load(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	sh := saved.Shell["mytool"]
	if sh == nil || sh.Live == nil || sh.Pending != nil || !strings.Contains(sh.Live.Bash, "old") {
		t.Fatalf("a snippet approved after the kill switch was enabled: %+v", sh)
	}
	if !sh.SyncPending {
		t.Fatalf("flip during resolution did not mark state dirty: %+v", sh)
	}
	if got := readInit(t, root, "cptinit.sh"); strings.Contains(got, "cpt:mytool") {
		t.Fatalf("snippet was rendered after the flip:\n%s", got)
	}
}

func TestKillSwitchLockContentionLeavesUnsynced(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := state.Acquire(filepath.Join(root, ".lock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	oldTimeout := reconcileLockTimeout
	reconcileLockTimeout = 50 * time.Millisecond
	defer func() { reconcileLockTimeout = oldTimeout }()
	a2, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if sh := a2.State.Shell["mytool"]; sh == nil || !sh.SyncPending {
		t.Fatalf("lock contention did not mark the snippet unsynced: %+v", sh)
	}
	infos := a2.ShellInfos()
	if len(infos) != 1 || !infos[0].SyncPending {
		t.Fatalf("status does not show unsynced state: %+v", infos)
	}
	a2.Close()
	lock.Release()
	reconcileLockTimeout = oldTimeout

	a3, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a3.Close()
	if got := readInit(t, root, "cptinit.sh"); strings.Contains(got, "cpt:mytool") {
		t.Fatalf("kill switch was not applied after the lock was released:\n%s", got)
	}
}

func TestKillSwitchQueuesRemovalOnReenable(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a2, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "lists", "test.json")); err != nil {
		t.Fatal(err)
	}
	a2.Close()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a3, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a3.Close()
	if sh := a3.State.Shell["mytool"]; sh == nil || sh.Pending == nil {
		t.Fatalf("removal while disabled was not queued on re-enable: %+v", sh)
	}
	if got := readInit(t, root, "cptinit.sh"); !strings.Contains(got, "cpt:mytool") {
		t.Fatalf("live snippet disappeared before removal approval:\n%s", got)
	}
}

func TestRejectWhileDisabledKeepsSyncPending(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	a, root := fixture(t, shellPackage("mytool", srv.URL+"/t.tar.gz", "old\n"))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackage("mytool", srv.URL+"/t.tar.gz", "candidate-b\n")))
	if err := a.WithLock(func() error { return a.SyncShell(context.Background(), nil) }); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a2, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackage("mytool", srv.URL+"/t.tar.gz", "candidate-c\n")))
	if err := a2.WithLock(func() error {
		_, err := a2.RejectShell([]string{"mytool"}, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if sh := a2.State.Shell["mytool"]; sh == nil || !sh.SyncPending {
		t.Fatalf("reject while disabled cleared the dirty marker: %+v", sh)
	}
	a2.Close()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a3, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a3.Close()
	sh := a3.State.Shell["mytool"]
	if sh == nil || sh.Pending == nil || !strings.Contains(sh.Pending.Bash, "candidate-c") {
		t.Fatalf("list change made while disabled was lost after reject: %+v", sh)
	}
}

func TestKillSwitchSyncErrorRetriesLater(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	mux := http.NewServeMux()
	mux.HandleFunc("/t.tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(content) })
	mux.HandleFunc("/snippet.sh", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("old\n")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sum := sha256.Sum256([]byte("old\n"))
	a, root := fixture(t, shellPackageURL("mytool", srv.URL+"/t.tar.gz", srv.URL+"/snippet.sh", hex.EncodeToString(sum[:])))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	disabled, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	disabled.Close()
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackageURL("mytool", srv.URL+"/t.tar.gz", srv.URL+"/missing.sh", strings.Repeat("d", 64))))
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	failed, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sh := failed.State.Shell["mytool"]
	if sh == nil || !sh.SyncPending || sh.LastError == "" {
		t.Fatalf("transient failure dropped the dirty marker: %+v", sh)
	}
	// Simulate the retry backoff expiring after the server recovers.
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackageURL("mytool", srv.URL+"/t.tar.gz", srv.URL+"/snippet.sh", hex.EncodeToString(sum[:]))))
	sh.SyncAttempt = time.Now().Add(-2 * time.Minute)
	if err := failed.State.Save(filepath.Join(root, "state.json")); err != nil {
		t.Fatal(err)
	}
	failed.Close()
	recovered, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	sh = recovered.State.Shell["mytool"]
	if sh == nil || sh.SyncPending || sh.LastError != "" {
		t.Fatalf("retry did not clear the transient error: %+v", sh)
	}
}

func TestKillSwitchQueuesNewSnippetOnReenable(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	writeListDoc(t, root, "test", fmt.Sprintf(`{"schema":1,"name":"test","packages":[%s]}`,
		shellPackage("mytool", srv.URL+"/t.tar.gz", "alias mytool=mytool\n")))
	if _, err := installOne(t, a, "mytool", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if sh := a.State.Shell["mytool"]; sh == nil || !sh.SyncPending {
		t.Fatalf("install while disabled did not mark shell sync: %+v", sh)
	}
	a.Close()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"allow_shell_packages":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a2, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	sh := a2.State.Shell["mytool"]
	if sh == nil || sh.Pending == nil || !strings.Contains(sh.Pending.Bash, "mytool") {
		t.Fatalf("first snippet was not queued on re-enable: %+v", sh)
	}
}

func TestShellStateLoadDropsEmptyEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	doc := `{"schema":1,"shell":{"ghost":{},"kept":{"snippet_sha256":"x","approved":true,"live":{"version":"1.0.0","bash":"export A=1\n"}}}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Shell["ghost"]; ok {
		t.Fatal("empty shell entry was kept")
	}
	if st.Shell["kept"] == nil {
		t.Fatal("populated shell entry was dropped")
	}
}
