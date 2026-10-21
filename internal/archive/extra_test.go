package archive

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractZipRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("../evil")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("x"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "evil.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(path, "", t.TempDir()); err == nil {
		t.Fatal("zip traversal was not rejected")
	}
}

func TestSelectAutoInstallsAllExecutables(t *testing.T) {
	files := []File{
		{Rel: "pkg/tool", Mode: 0o755},
		{Rel: "pkg/helper", Mode: 0o755},
		{Rel: "pkg/README.md", Mode: 0o644},
		{Rel: "pkg/a/b/c/binary", Mode: 0o755},
	}
	bins, err := Select(files, "/tmp", SelectOptions{StripPrefix: true})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	names := map[string]bool{}
	for _, b := range bins {
		names[b.Name] = true
	}
	if !names["tool"] || !names["helper"] {
		t.Fatalf("missing executables: %+v", bins)
	}
	if names["binary"] {
		t.Fatalf("deep binary should be excluded: %+v", bins)
	}
	if names["README.md"] {
		t.Fatalf("docs should be excluded: %+v", bins)
	}
}
