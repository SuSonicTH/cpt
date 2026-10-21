package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st := New()
	st.Packages["fzf"] = &Package{
		Version:        "0.74.5",
		Bins:           map[string]string{"fzf": "abc"},
		DownloadSHA256: "abc",
	}
	st.Provides["fzf"] = "fzf"
	if err := st.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Packages["fzf"].Version != "0.74.5" {
		t.Fatalf("unexpected state: %+v", got.Packages)
	}
	if owner, ok := got.Owner("fzf"); !ok || owner != "fzf" {
		t.Fatalf("owner = %q, %v", owner, ok)
	}
}

func TestLoadMissingFile(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Packages == nil || st.Lists == nil {
		t.Fatal("maps should be initialised")
	}
}

func TestRemoveClearsOwnership(t *testing.T) {
	st := New()
	st.Packages["a"] = &Package{Bins: map[string]string{"tool": "h"}}
	st.Provides["tool"] = "a"
	st.Remove("a")
	if _, ok := st.Provides["tool"]; ok {
		t.Fatal("provides entry not cleared")
	}
	if _, ok := st.Packages["a"]; ok {
		t.Fatal("package not removed")
	}
}

func TestRemoveClearsShellState(t *testing.T) {
	st := New()
	st.Packages["a"] = &Package{Bins: map[string]string{}}
	st.Shell["a"] = &Shell{Live: &ShellSnippet{Bash: "export A=1\n"}, Approved: true}
	st.Remove("a")
	if _, ok := st.Shell["a"]; ok {
		t.Fatal("shell state survived package removal")
	}
}

func TestLoadKeepsRejectedSnippetState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	doc := `{"schema":1,"shell":{"a":{"approved":false,"rejected_sha256":"abc"}}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Shell["a"] == nil || st.Shell["a"].RejectedSHA256 != "abc" {
		t.Fatalf("rejection marker was dropped: %+v", st.Shell)
	}
}

func TestLockContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	l1, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if _, err := Acquire(path, 150*time.Millisecond); err == nil {
		t.Fatal("expected second Acquire to time out")
	}
	if err := l1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

func TestStaleLockFileDoesNotBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	// Metadata left behind by a crashed process must not matter: the kernel
	// released its lock when the process exited.
	if err := os.WriteFile(path, []byte("999999 deadbeef 2020-01-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path, 0)
	if err != nil {
		t.Fatalf("Acquire with a stale lock file: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	lock, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}
