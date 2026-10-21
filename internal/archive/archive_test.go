package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func writeTarGz(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typeflag := byte(tar.TypeReg)
		if e.link != "" {
			typeflag = tar.TypeSymlink
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Size:     int64(len(e.body)),
			Typeflag: typeflag,
			Linkname: e.link,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "asset.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type tarEntry struct {
	name string
	body string
	mode int64
	link string
}

func TestExtractAndSelectTarGz(t *testing.T) {
	path := writeTarGz(t, []tarEntry{
		{name: "tool-1.0/tool", body: "#!/bin/sh\necho hi\n", mode: 0o755},
		{name: "tool-1.0/README.md", body: "docs", mode: 0o644},
		{name: "tool-1.0/tool.1", body: "man", mode: 0o644},
	})
	dest := t.TempDir()
	files, err := Extract(path, "", dest)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}
	bins, err := Select(files, dest, SelectOptions{Bins: []string{"tool"}})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(bins) != 1 || bins[0].Name != "tool" {
		t.Fatalf("unexpected bins: %+v", bins)
	}
	if _, err := os.Stat(bins[0].Path); err != nil {
		t.Fatalf("selected binary missing: %v", err)
	}
}

func TestSelectFallsBackToPrefixedBinary(t *testing.T) {
	files := []File{
		{Rel: "yq_linux_amd64", Mode: 0o755},
		{Rel: "yq.1", Mode: 0o644},
	}
	bins, err := Select(files, "/tmp", SelectOptions{Bins: []string{"yq"}})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(bins) != 1 || bins[0].Name != "yq" || bins[0].Path != filepath.Join("/tmp", "yq_linux_amd64") {
		t.Fatalf("unexpected bins: %+v", bins)
	}
}

func TestSelectRawSingleFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "jq-linux-amd64")
	if err := os.WriteFile(src, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	files, err := Extract(src, "raw", dest)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	bins, err := Select(files, dest, SelectOptions{Bins: []string{"jq"}, Windows: true})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(bins) != 1 || bins[0].Name != "jq.exe" {
		t.Fatalf("unexpected bins: %+v", bins)
	}
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	path := writeTarGz(t, []tarEntry{{name: "../evil", body: "x", mode: 0o644}})
	_, err := Extract(path, "", t.TempDir())
	if err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
}

func TestExtractRejectsAbsolutePath(t *testing.T) {
	path := writeTarGz(t, []tarEntry{{name: "/etc/passwd", body: "x", mode: 0o644}})
	_, err := Extract(path, "", t.TempDir())
	if err == nil {
		t.Fatal("expected absolute path to be rejected")
	}
}

func TestExtractSkipsSymlinks(t *testing.T) {
	path := writeTarGz(t, []tarEntry{
		{name: "safe", body: "x", mode: 0o644},
		{name: "link", mode: 0o777, link: "/etc/passwd"},
	})
	files, err := Extract(path, "", t.TempDir())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, f := range files {
		if f.Rel == "link" {
			t.Fatal("symlink was extracted")
		}
	}
}

func TestExtractZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("folder/tool.exe")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("binary"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tool.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	files, err := Extract(path, "", dest)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	bins, err := Select(files, dest, SelectOptions{Bins: []string{"tool"}, Windows: true})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if bins[0].Name != "tool.exe" {
		t.Fatalf("unexpected bin name %q", bins[0].Name)
	}
}

func TestDetectFormat(t *testing.T) {
	cases := map[string]string{
		"a.tar.gz":  "tar.gz",
		"a.tgz":     "tar.gz",
		"a.tar.xz":  "tar.xz",
		"a.tar.zst": "tar.zst",
		"a.zip":     "zip",
		"a-linux":   "raw",
	}
	for name, want := range cases {
		if got := DetectFormat(name); got != want {
			t.Errorf("DetectFormat(%q) = %q, want %q", name, got, want)
		}
	}
}
