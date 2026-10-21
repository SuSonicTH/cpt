package tui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/progress"
)

func tgz(t *testing.T, files map[string]string) []byte {
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

func TestOperationCmdInstalls(t *testing.T) {
	content := tgz(t, map[string]string{"mytool": "#!/bin/sh\necho hi\n"})
	mux := http.NewServeMux()
	mux.HandleFunc("/tool-1.0.0.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	a, err := app.Open(app.Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard, Events: progress.New()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	doc := fmt.Sprintf(`{"schema":1,"name":"test","packages":[{"name":"mytool","version":"1.0.0","source":{"type":"url","url":%q,"bins":["mytool"]}}]}`,
		srv.URL+"/tool-{version}.tar.gz")
	if err := os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	msg := operationCmd(context.Background(), a, "install", []string{"mytool"}, false)()
	done, ok := msg.(opDoneMsg)
	if !ok {
		t.Fatalf("got %T, want opDoneMsg", msg)
	}
	if done.err != nil {
		t.Fatalf("install failed: %v", done.err)
	}
	if len(done.results) != 1 || done.results[0].Action != "installed" {
		t.Fatalf("unexpected results: %+v", done.results)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", binaryName("mytool"))); err != nil {
		t.Fatalf("binary not installed: %v", err)
	}
}

func binaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
