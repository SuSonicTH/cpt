package sshtarget

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func validTarget() *Target {
	return &Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH: SSH{
			Source: SourceConfig,
			Host:   "staging-box",
		},
		Shell: ShellAuto,
	}
}

func TestValidateTarget(t *testing.T) {
	tg := validTarget()
	if err := tg.Validate(); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}
	tg.RemoteRoot = "relative/path"
	if err := tg.Validate(); err == nil {
		t.Fatal("relative remote root accepted")
	}
	tg = validTarget()
	tg.SSH.Host = "-oProxyCommand=bad"
	if err := tg.Validate(); err == nil {
		t.Fatal("leading-dash host accepted")
	}
	tg = validTarget()
	tg.Shell = "fish"
	if err := tg.Validate(); err == nil {
		t.Fatal("fish shell accepted")
	}
	tg = validTarget()
	tg.RemoteRoot = "/srv/cpt/../safe"
	if err := tg.Validate(); err == nil {
		t.Fatal("remote root containing '..' was accepted")
	}
	tg = validTarget()
	tg.RemoteRoot = "/srv/cpt/."
	if err := tg.Validate(); err != nil || tg.RemoteRoot != "/srv/cpt" {
		t.Fatalf("remote root was not cleaned: %q err=%v", tg.RemoteRoot, err)
	}
	tg = validTarget()
	tg.RemoteRoot = "/tmp"
	tg.CleanupOnExit = true
	if err := tg.Validate(); err == nil {
		t.Fatal("cleanup accepted a top-level remote root")
	}
}

func TestTargetFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ssh.json")
	f := NewFile()
	tg := validTarget()
	tg.RemoteRoot = "/srv/cpt"
	tg.CleanupOnExit = true
	tg.Shell = ShellZsh
	if err := f.Put(tg); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(file); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("ssh.json mode = %o, want 600", st.Mode().Perm())
	}
	loaded, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loaded.Get("staging")
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteRoot != "/srv/cpt" || !got.CleanupOnExit || got.Shell != ShellZsh {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if _, err := loaded.Remove("staging"); err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.Get("staging"); err == nil {
		t.Fatal("removed target still present")
	}
}

func TestCacheFresh(t *testing.T) {
	tg := validTarget()
	sha := strings.Repeat("a", 64)
	now := time.Now().UTC()
	tg.LastCopied = now.Add(-time.Hour)
	tg.CopiedSHA256 = sha
	tg.CopiedKey = tg.CopyKey(sha)
	if !tg.CacheFresh(now, sha) {
		t.Fatal("recent matching copy is not fresh")
	}
	if tg.CacheFresh(now, strings.Repeat("b", 64)) {
		t.Fatal("changed bundle hash reused cache")
	}
	tg.CleanupOnExit = true
	if tg.CacheFresh(now, sha) {
		t.Fatal("cleanup target reused cache")
	}
	tg.CleanupOnExit = false
	tg.LastCopied = now.Add(-CacheTTL - time.Minute)
	if tg.CacheFresh(now, sha) {
		t.Fatal("expired copy reused cache")
	}
	tg.LastCopied = now
	tg.CopiedKey = "other"
	if tg.CacheFresh(now, sha) {
		t.Fatal("changed copy settings reused cache")
	}
}

func TestTargetsRequireValidBundleName(t *testing.T) {
	tg := validTarget()
	tg.Bundle = "../escape"
	if err := tg.Validate(); err == nil {
		t.Fatal("invalid bundle name accepted")
	}
}

func TestZeroTimestampsAreOmitted(t *testing.T) {
	data, err := json.Marshal(validTarget())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "0001-01-01") {
		t.Fatalf("zero timestamps were serialized: %s", data)
	}
}

func TestValidateRejectsReservedTargetNames(t *testing.T) {
	for _, name := range []string{"list", "add", "edit", "remove", "connect", "help"} {
		name := name
		t.Run(name, func(t *testing.T) {
			tgt := &Target{
				Name:   name,
				Bundle: "devbox",
				SSH:    SSH{Source: SourceCustom, Host: "box"},
			}
			if err := NewFile().Put(tgt); err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("reserved name %q accepted: %v", name, err)
			}
		})
	}
	// A name that only differs in case is still reachable through the short form.
	if err := NewFile().Put(&Target{Name: "List", Bundle: "devbox", SSH: SSH{Source: SourceCustom, Host: "box"}}); err != nil {
		t.Fatalf("case-different name rejected: %v", err)
	}
	// Loading an older file that already contains a reserved name must work, so
	// the rest of the target list stays usable.
	f := NewFile()
	f.Targets["list"] = &Target{Name: "list", Bundle: "devbox", SSH: SSH{Source: SourceCustom, Host: "box"}}
	if err := f.Save(filepath.Join(t.TempDir(), "ssh.json")); err != nil {
		t.Fatalf("existing reserved name broke Save: %v", err)
	}
}

func TestExpandHomeHandlesBareTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := expandHome("~"); got != home {
		t.Fatalf("expandHome(~) = %q, want %q", got, home)
	}
	if got := expandHome("~/key"); got != filepath.Join(home, "key") {
		t.Fatalf("expandHome(~/key) = %q", got)
	}
	if got := expandHome("/abs/key"); got != "/abs/key" {
		t.Fatalf("expandHome(/abs/key) = %q", got)
	}
}
