package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SuSonicTH/cpt/internal/bundle"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/sshtarget"
)

func writeTestBundle(t *testing.T, path string) {
	t.Helper()
	writeTestBundlePayload(t, path, "payload")
}

func writeTestBundlePayload(t *testing.T, path, payloadText string) {
	t.Helper()
	base := []byte("base")
	payload := []byte(payloadText)
	footer := bundle.Footer{
		PayloadOffset: uint64(len(base)),
		PayloadLen:    uint64(len(payload)),
		Version:       bundle.FormatVersion,
	}
	data := append(append(append([]byte{}, base...), payload...), footer.Marshal()...)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func sshTestApp(t *testing.T) (*App, string) {
	t.Helper()
	root := t.TempDir()
	var out, errb bytes.Buffer
	a, err := Open(Options{Root: root, Quiet: true, Stdout: &out, Stderr: &errb})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(a.Close)
	m := bundle.Manifest{
		Schema:     bundle.ManifestSchema,
		Name:       "devbox",
		Target:     "linux/amd64",
		CptVersion: "test",
		Packages: []bundle.ManifestPackage{{
			Name:    "fzf",
			Tag:     "v1.0.0",
			Version: "1.0.0",
			Asset:   "fzf.tar.gz",
			Bins:    []string{"fzf"},
		}},
		Lists: map[string]bundle.ManifestList{},
	}
	manifestPath := filepath.Join(root, "bundles", bundle.FileName("devbox"))
	if err := m.Save(manifestPath); err != nil {
		t.Fatal(err)
	}
	writeTestBundle(t, m.OutputPath(filepath.Join(root, "bundles")))
	return a, root
}

func installFakeSSHTools(t *testing.T) string {
	t.Helper()
	logPath, _ := installFakeSSHToolsDir(t)
	return logPath
}

func installFakeSSHToolsDir(t *testing.T) (logPath, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh/scp shell scripts are POSIX-only")
	}
	dir = t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	write := func(name, body string) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("scp", `#!/bin/sh
{
  printf 'scp'
  for a in "$@"; do printf '\t%s' "$a"; done
  printf '\n'
} >> "$CPT_FAKE_LOG"
exit "${CPT_FAKE_SCP_EXIT:-0}"
`)
	write("ssh", `#!/bin/sh
{
  printf 'ssh'
  for a in "$@"; do printf '\t%s' "$a"; done
  printf '\n'
} >> "$CPT_FAKE_LOG"
for a in "$@"; do
  if [ "$a" = "-tt" ]; then
    exit "${CPT_FAKE_SHELL_EXIT:-${CPT_FAKE_SSH_EXIT:-0}}"
  fi
done
exit "${CPT_FAKE_SSH_EXIT:-0}"
`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CPT_FAKE_LOG", logPath)
	return logPath, dir
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSSHConnectCopiesThenUsesCache(t *testing.T) {
	a, _ := sshTestApp(t)
	logPath := installFakeSSHTools(t)
	if err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceConfig, Host: "staging-box"},
		Shell:  sshtarget.ShellAuto,
	}); err != nil {
		t.Fatal(err)
	}
	opt := SSHConnectOptions{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	res, err := a.SSHConnect(context.Background(), "staging", opt)
	if err != nil {
		t.Fatalf("first connect: %v", err)
	}
	if !res.Copied || res.Mode != sshtarget.ModeInstall {
		t.Fatalf("first connect result = %+v, want copied install", res)
	}
	log := readLog(t, logPath)
	if !strings.Contains(log, "scp\t") {
		t.Fatalf("first connect did not upload:\n%s", log)
	}
	if !strings.Contains(log, "sh -c") {
		t.Fatalf("first session did not run the bootstrap:\n%s", log)
	}
	if strings.Contains(log, "--install-init") {
		t.Fatalf("remote command passed --install-init:\n%s", log)
	}
	target, err := a.SSHTarget("staging")
	if err != nil {
		t.Fatal(err)
	}
	if target.LastCopied.IsZero() || target.CopiedSHA256 == "" {
		t.Fatalf("copy cache was not recorded: %+v", target)
	}

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = a.SSHConnect(context.Background(), "staging", opt)
	if err != nil {
		t.Fatalf("second connect: %v", err)
	}
	if res.Copied || res.Mode != sshtarget.ModeShell {
		t.Fatalf("second connect result = %+v, want shell-only", res)
	}
	log = readLog(t, logPath)
	if strings.Contains(log, "scp\t") {
		t.Fatalf("fresh cache uploaded again:\n%s", log)
	}
	if !strings.Contains(log, "sh -c") {
		t.Fatalf("second session did not run the bootstrap:\n%s", log)
	}
	if got := strings.Count(log, "ssh\t"); got != 2 {
		t.Fatalf("cached connect made %d ssh calls, want a prepare plus a session:\n%s", got, log)
	}
}

func TestSSHConnectRebuiltBundleInvalidatesCache(t *testing.T) {
	a, root := sshTestApp(t)
	logPath := installFakeSSHTools(t)
	if err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "10.0.0.7"},
	}); err != nil {
		t.Fatal(err)
	}
	opt := SSHConnectOptions{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if _, err := a.SSHConnect(context.Background(), "staging", opt); err != nil {
		t.Fatal(err)
	}
	writeTestBundlePayload(t, filepath.Join(root, "bundles", "devbox-linux-amd64"), "rebuilt-payload")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := a.SSHConnect(context.Background(), "staging", opt)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Copied || res.Mode != sshtarget.ModeInstall {
		t.Fatalf("rebuilt bundle reused cache: %+v", res)
	}
	if !strings.Contains(readLog(t, logPath), "scp\t") {
		t.Fatal("rebuilt bundle was not uploaded")
	}
}

func TestSSHCleanupTargetAlwaysCopies(t *testing.T) {
	a, _ := sshTestApp(t)
	installFakeSSHTools(t)
	if err := a.SaveSSHTarget(&sshtarget.Target{
		Name:          "clean",
		Bundle:        "devbox",
		SSH:           sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "box"},
		CleanupOnExit: true,
	}); err != nil {
		t.Fatal(err)
	}
	opt := SSHConnectOptions{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	for i := 0; i < 2; i++ {
		res, err := a.SSHConnect(context.Background(), "clean", opt)
		if err != nil {
			t.Fatalf("connect %d: %v", i, err)
		}
		if !res.Copied || res.Mode != sshtarget.ModeInstall {
			t.Fatalf("cleanup target did not copy on connect %d: %+v", i, res)
		}
	}
}

func TestSSHConnectErrors(t *testing.T) {
	a, _ := sshTestApp(t)
	installFakeSSHTools(t)
	if err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "box"},
	}); err != nil {
		t.Fatal(err)
	}
	opt := SSHConnectOptions{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}

	a.Offline = true
	if _, err := a.SSHConnect(context.Background(), "staging", opt); err == nil {
		t.Fatal("offline connect succeeded")
	}
	a.Offline = false

	t.Setenv("CPT_FAKE_SSH_EXIT", "241")
	if _, err := a.SSHConnect(context.Background(), "staging", opt); err == nil || cerr.KindOf(err) != cerr.Conflict {
		t.Fatalf("platform mismatch error = %v, kind = %v", err, cerr.KindOf(err))
	}

	t.Setenv("CPT_FAKE_SSH_EXIT", "242")
	if _, err := a.SSHConnect(context.Background(), "staging", opt); err == nil || cerr.KindOf(err) != cerr.Usage {
		t.Fatalf("unsupported shell error = %v, kind = %v", err, cerr.KindOf(err))
	}

	t.Setenv("CPT_FAKE_SSH_EXIT", "0")
	t.Setenv("CPT_FAKE_SHELL_EXIT", "7")
	_, err := a.SSHConnect(context.Background(), "staging", opt)
	if err == nil {
		t.Fatal("session failure was ignored")
	}
	if code := cerr.ExitCode(err); code != 7 {
		t.Fatalf("exit code = %d, want 7 (err=%v)", code, err)
	}
}

func TestSaveSSHTargetRequiresBundle(t *testing.T) {
	a, _ := sshTestApp(t)
	err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "missing",
		Bundle: "nope",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "box"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown bundle") {
		t.Fatalf("missing bundle error = %v", err)
	}
}

func TestSSHConnectRetriesWhenRemoteCacheIsMissing(t *testing.T) {
	a, _ := sshTestApp(t)
	logPath, dir := installFakeSSHToolsDir(t)
	if err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "box"},
	}); err != nil {
		t.Fatal(err)
	}
	opt := SSHConnectOptions{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if _, err := a.SSHConnect(context.Background(), "staging", opt); err != nil {
		t.Fatal(err)
	}

	counter := filepath.Join(dir, "ssh-count")
	seqSSH := `#!/bin/sh
{
  printf 'ssh'
  for a in "$@"; do printf '\t%s' "$a"; done
  printf '\n'
} >> "$CPT_FAKE_LOG"
if [ -f "$CPT_FAKE_COUNTER" ]; then
  exit 0
fi
: > "$CPT_FAKE_COUNTER"
exit 240
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(seqSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPT_FAKE_COUNTER", counter)
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := a.SSHConnect(context.Background(), "staging", opt)
	if err != nil {
		t.Fatalf("fallback connect: %v", err)
	}
	if !res.Copied || res.Mode != sshtarget.ModeInstall {
		t.Fatalf("fallback result = %+v", res)
	}
	log := readLog(t, logPath)
	if strings.Count(log, "ssh\t") != 3 {
		t.Fatalf("expected cache check, install prepare, and session:\n%s", log)
	}
	if !strings.Contains(log, "scp\t") {
		t.Fatalf("fallback did not upload:\n%s", log)
	}
}

func TestSSHConnectSettingsChangeInvalidatesCache(t *testing.T) {
	a, _ := sshTestApp(t)
	installFakeSSHTools(t)
	if err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "one"},
	}); err != nil {
		t.Fatal(err)
	}
	opt := SSHConnectOptions{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if _, err := a.SSHConnect(context.Background(), "staging", opt); err != nil {
		t.Fatal(err)
	}
	target, err := a.SSHTarget("staging")
	if err != nil {
		t.Fatal(err)
	}
	target.SSH.Host = "two"
	if err := a.SaveSSHTarget(target); err != nil {
		t.Fatal(err)
	}
	updated, err := a.SSHTarget("staging")
	if err != nil {
		t.Fatal(err)
	}
	if updated.CopiedSHA256 != "" || updated.CopiedKey != "" || !updated.LastCopied.IsZero() {
		t.Fatalf("settings change kept cache: %+v", updated)
	}
	res, err := a.SSHConnect(context.Background(), "staging", opt)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Copied || res.Mode != sshtarget.ModeInstall {
		t.Fatalf("changed settings reused cache: %+v", res)
	}
}

func TestSSHTargetRejectsInvalidBundle(t *testing.T) {
	a, root := sshTestApp(t)
	if err := os.WriteFile(filepath.Join(root, "bundles", "devbox-linux-amd64"), []byte("not-a-bundle"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "box"},
	})
	if err == nil || !strings.Contains(err.Error(), "valid executable") {
		t.Fatalf("invalid bundle accepted: %v", err)
	}

	writeTestBundle(t, filepath.Join(root, "bundles", "devbox-linux-amd64"))
	if err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "box"},
	}); err != nil {
		t.Fatal(err)
	}
	installFakeSSHTools(t)
	if err := os.WriteFile(filepath.Join(root, "bundles", "devbox-linux-amd64"), []byte("corrupt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SSHConnect(context.Background(), "staging", SSHConnectOptions{
		Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	}); err == nil || cerr.KindOf(err) != cerr.Integrity {
		t.Fatalf("corrupt bundle connect error = %v kind=%v", err, cerr.KindOf(err))
	}
}

func TestSSHConnectCopyFailureIsNetworkError(t *testing.T) {
	a, _ := sshTestApp(t)
	installFakeSSHTools(t)
	if err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "box"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPT_FAKE_SCP_EXIT", "1")
	_, err := a.SSHConnect(context.Background(), "staging", SSHConnectOptions{
		Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if err == nil || cerr.KindOf(err) != cerr.Network {
		t.Fatalf("copy failure = %v kind=%v", err, cerr.KindOf(err))
	}
	if code := cerr.ExitCode(err); code != int(cerr.Network) {
		t.Fatalf("copy failure exit code = %d, want %d; the wrapped scp status must not override the documented network code", code, int(cerr.Network))
	}
}

// TestSSHConnectSessionStatusIsNotASentinel guards the split-transport fix: a
// user's shell can exit with 240/241/242 without being mistaken for a prepare
// sentinel, so it never triggers a reinstall or a bogus platform error.
func TestSSHConnectSessionStatusIsNotASentinel(t *testing.T) {
	a, _ := sshTestApp(t)
	logPath := installFakeSSHTools(t)
	if err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "box"},
	}); err != nil {
		t.Fatal(err)
	}
	opt := SSHConnectOptions{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if _, err := a.SSHConnect(context.Background(), "staging", opt); err != nil {
		t.Fatalf("prime connect: %v", err)
	}
	for _, code := range []string{"240", "241", "242"} {
		code := code
		t.Run(code, func(t *testing.T) {
			if err := os.WriteFile(logPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CPT_FAKE_SHELL_EXIT", code)
			_, err := a.SSHConnect(context.Background(), "staging", opt)
			var se *SSHSessionError
			if !errors.As(err, &se) {
				t.Fatalf("session status %s was not reported as a session status: %v", code, err)
			}
			if strconv.Itoa(se.Code) != code {
				t.Fatalf("session status = %d, want %s", se.Code, code)
			}
			if got := cerr.ExitCode(err); got != se.Code {
				t.Fatalf("cerr.ExitCode = %d, want the session status %d", got, se.Code)
			}
			log := readLog(t, logPath)
			if strings.Contains(log, "scp\t") {
				t.Fatalf("session status %s triggered a reinstall:\n%s", code, log)
			}
			if got := strings.Count(log, "ssh\t"); got != 2 {
				t.Fatalf("session status %s made %d ssh calls, want prepare plus session:\n%s", code, got, log)
			}
		})
	}
}

// TestSSHConnectFailedInstallLeavesNoCache records the copy only after the
// prepare phase succeeds, so a failed install cannot claim a fresh cache or a
// completed connection.
func TestSSHConnectFailedInstallLeavesNoCache(t *testing.T) {
	a, _ := sshTestApp(t)
	installFakeSSHTools(t)
	if err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "box"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPT_FAKE_SSH_EXIT", "9")
	_, err := a.SSHConnect(context.Background(), "staging", SSHConnectOptions{
		Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if err == nil || cerr.KindOf(err) == cerr.Network {
		t.Fatalf("prepare failure = %v kind=%v, want an install error", err, cerr.KindOf(err))
	}
	target, err := a.SSHTarget("staging")
	if err != nil {
		t.Fatal(err)
	}
	if !target.LastCopied.IsZero() || target.CopiedSHA256 != "" || target.CopiedKey != "" {
		t.Fatalf("failed install recorded a copy cache: %+v", target)
	}
	if !target.LastConnected.IsZero() {
		t.Fatalf("failed install recorded a connection: %+v", target)
	}
	infos, err := a.SSHTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].CacheFresh {
		t.Fatalf("failed install reported a fresh cache: %+v", infos)
	}
}

// TestSSHConnectCancellationIsNotGeneric covers the cancelled prepare path: it
// must report the conventional signal status and must not record a cache that
// was never written.
func TestSSHConnectCancellationIsNotGeneric(t *testing.T) {
	a, _ := sshTestApp(t)
	_, dir := installFakeSSHToolsDir(t)
	// A hanging ssh lets the parent context cancel mid-phase.
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nsleep 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "box"},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err := a.SSHConnect(ctx, "staging", SSHConnectOptions{
		Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("cancelled connect returned no error")
	}
	code := cerr.ExitCode(err)
	if code != 130 && code != 137 && code != 143 {
		t.Fatalf("cancelled connect exit = %d, want a signal status (130/137/143); err=%v", code, err)
	}
	target, err := a.SSHTarget("staging")
	if err != nil {
		t.Fatal(err)
	}
	if !target.LastCopied.IsZero() || !target.LastConnected.IsZero() {
		t.Fatalf("cancelled prepare recorded progress: %+v", target)
	}
}
