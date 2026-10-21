package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/SuSonicTH/cpt/internal/state"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInstallURLSourceVerifiedAndTOFU(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "#!/bin/sh\necho v1\n"})
	sum := sha256.Sum256(content)
	var tamper atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/tool-1.0.0-linux-amd64.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		if tamper.Load() {
			w.Write(tarGz(t, map[string]string{"mytool": "#!/bin/sh\necho v2\n"}))
			return
		}
		w.Write(content)
	})
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		if tamper.Load() {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "%s  tool-1.0.0-linux-amd64.tar.gz\n", hex.EncodeToString(sum[:]))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	var out, errb bytes.Buffer
	a, err := Open(Options{Root: root, Quiet: true, Stdout: &out, Stderr: &errb})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()

	listDoc := fmt.Sprintf(`{
	  "schema": 1, "name": "test",
	  "packages": [{
	    "name": "mytool", "version": "1.0.0",
	    "source": {"type": "url", "url": %q, "bins": ["mytool"]}
	  }]
	}`, srv.URL+"/tool-{version}-{os}-{arch}.tar.gz")
	if err := os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(listDoc), 0o644); err != nil {
		t.Fatal(err)
	}

	var results []Result
	err = a.WithLock(func() error {
		var err error
		results, err = a.Install(context.Background(), []string{"mytool"}, InstallOptions{})
		return err
	})
	if err != nil {
		t.Fatalf("Install: %v (stderr: %s)", err, errb.String())
	}
	if len(results) != 1 || results[0].Action != "installed" {
		t.Fatalf("unexpected results: %+v", results)
	}

	bin := filepath.Join(root, "bin", "mytool")
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("installed binary: %v", err)
	}
	if !bytes.Contains(body, []byte("echo v1")) {
		t.Fatalf("unexpected binary content: %s", body)
	}

	saved, err := state.Load(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	p := saved.Packages["mytool"]
	if p == nil {
		t.Fatal("package not recorded in state")
	}
	if p.ChecksumStatus != "verified" {
		t.Fatalf("checksum status = %q", p.ChecksumStatus)
	}
	if saved.Provides["mytool"] != "mytool" {
		t.Fatalf("provides = %+v", saved.Provides)
	}

	// Same version, different bytes, no checksum file: the TOFU pin must fail.
	tamper.Store(true)
	err = a.WithLock(func() error {
		_, err := a.Install(context.Background(), []string{"mytool"}, InstallOptions{})
		return err
	})
	if err == nil {
		t.Fatal("expected an integrity failure for changed content")
	}
	if _, statErr := os.Stat(bin); statErr != nil {
		t.Fatalf("binary should still exist: %v", statErr)
	}
	if !bytes.Contains(mustRead(t, bin), []byte("echo v1")) {
		t.Fatal("binary was replaced despite verification failure")
	}
}

func TestRemoveInstalledPackage(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "x"})
	mux := http.NewServeMux()
	mux.HandleFunc("/tool-1.0.0-linux-amd64.tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(content) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	doc := fmt.Sprintf(`{"schema":1,"name":"test","packages":[{"name":"mytool","version":"1.0.0","source":{"type":"url","url":%q,"bins":["mytool"]}}]}`,
		srv.URL+"/tool-{version}-{os}-{arch}.tar.gz")
	os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(doc), 0o644)

	if err := a.WithLock(func() error {
		_, err := a.Install(context.Background(), []string{"mytool"}, InstallOptions{})
		return err
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := a.WithLock(func() error {
		_, err := a.Remove(context.Background(), []string{"mytool"}, false)
		return err
	}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "mytool")); !os.IsNotExist(err) {
		t.Fatalf("binary still present: %v", err)
	}
	saved, _ := state.Load(filepath.Join(root, "state.json"))
	if len(saved.Packages) != 0 {
		t.Fatalf("package still recorded: %+v", saved.Packages)
	}
}

func TestParseListSpec(t *testing.T) {
	cases := []struct {
		in     string
		origin string
		name   string
	}{
		{"acme/tools", "https://github.com/acme/tools/raw/HEAD/cpt.json", "tools"},
		{"github.com/acme/tools#main:ci/cpt.json", "https://github.com/acme/tools/raw/main/ci/cpt.json", "tools"},
		{"https://example.com/lists/dev.json", "https://example.com/lists/dev.json", "dev"},
		{"https://raw.githubusercontent.com/acme/tools/main/cpt.json", "https://raw.githubusercontent.com/acme/tools/main/cpt.json", "tools"},
	}
	for _, tt := range cases {
		got, err := ParseListSpec(tt.in)
		if err != nil {
			t.Errorf("ParseListSpec(%q): %v", tt.in, err)
			continue
		}
		if got.Origin != tt.origin || got.Name != tt.name {
			t.Errorf("ParseListSpec(%q) = %+v", tt.in, got)
		}
	}
	if _, err := ParseListSpec("ftp://x/y"); err == nil {
		t.Error("expected an error for ftp")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRowsConflictAndFilters(t *testing.T) {
	root := t.TempDir()
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	doc := `{"schema":1,"name":"test","packages":[{"name":"mytool","version":"1.0.0","source":{"type":"url","url":"https://x/y"}}]}`
	os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(doc), 0o644)

	rows, err := a.Rows(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Name == "mytool" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mytool not in rows: %+v", rows)
	}

	installed, err := a.Rows(context.Background(), ListOptions{Installed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 0 {
		t.Fatalf("expected no installed packages, got %+v", installed)
	}
}

func TestSaveStateIsAtomicJSON(t *testing.T) {
	root := t.TempDir()
	a, err := Open(Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.WithLock(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st state.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
}
