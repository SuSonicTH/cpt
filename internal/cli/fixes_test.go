package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SuSonicTH/cpt/internal/platform"
)

func archiveWith(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// urlFixture creates a root with one URL-sourced package served by a test
// server. It returns the root and the server.
func urlFixture(t *testing.T) (string, *httptest.Server) {
	t.Helper()
	plat := platform.Current()
	asset := fmt.Sprintf("/tool-1.0.0-%s-%s.tar.gz", plat.OS, plat.Arch)
	content := archiveWith(t, "mytool", "#!/bin/sh\necho hi\n")

	mux := http.NewServeMux()
	mux.HandleFunc(asset, func(w http.ResponseWriter, r *http.Request) { w.Write(content) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "lists"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`{"schema":1,"name":"test","packages":[
	  {"name":"mytool","version":"1.0.0","source":{"type":"url","url":%q,"bins":["mytool"]}}]}`,
		srv.URL+"/tool-{version}-{os}-{arch}.tar.gz")
	if err := os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, srv
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestInstallJSONEnvelope(t *testing.T) {
	root, _ := urlFixture(t)
	code, out, errb := runCLI(t, "--root", root, "--json", "install", "mytool")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data []struct {
			Action string `json:"action"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if !env.OK || len(env.Data) != 1 || env.Data[0].Action != "installed" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
}

func TestInstallJSONFailureIsAnEnvelope(t *testing.T) {
	root, _ := urlFixture(t)
	code, out, _ := runCLI(t, "--root", root, "--json", "install", "nope")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if env.OK || env.Error.Code != 1 {
		t.Fatalf("unexpected envelope: %+v", env)
	}
}

func TestVerifyJSONReportsFailure(t *testing.T) {
	root, _ := urlFixture(t)
	if code, _, errb := runCLI(t, "--root", root, "install", "mytool"); code != 0 {
		t.Fatalf("install failed: %s", errb)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "mytool"), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "--root", root, "--json", "verify")
	if code != 3 {
		t.Fatalf("exit = %d, want 3", code)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data []struct {
			Status string `json:"status"`
		} `json:"data"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if env.OK || env.Error.Code != 3 || len(env.Data) != 1 || env.Data[0].Status != "modified" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
}

func TestDoctorJSONReportsFailure(t *testing.T) {
	root, _ := urlFixture(t)
	if err := os.WriteFile(filepath.Join(root, "lists", "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "--root", root, "--json", "doctor")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if env.OK {
		t.Fatalf("expected a failure envelope: %s", out)
	}
}

func TestFlagsAfterPositionalArguments(t *testing.T) {
	root, _ := urlFixture(t)
	code, out, errb := runCLI(t, "--root", root, "install", "mytool", "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
	if !strings.Contains(out, "would install") {
		t.Fatalf("unexpected output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "mytool")); !os.IsNotExist(err) {
		t.Fatal("dry run installed the package")
	}
}

func TestOfflineRefusesDownload(t *testing.T) {
	root, _ := urlFixture(t)
	code, _, errb := runCLI(t, "--root", root, "--offline", "install", "mytool")
	if code == 0 {
		t.Fatal("offline install should fail")
	}
	if !strings.Contains(errb, "offline") {
		t.Fatalf("unexpected stderr: %s", errb)
	}
}

func TestListsAddFlagsAfterPositional(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"schema":1,"name":"remote","packages":[]}`)
	}))
	defer srv.Close()
	root := t.TempDir()
	code, out, errb := runCLI(t, "--root", root, "lists", "add", srv.URL+"/cpt.json", "--name", "extra")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
	if !strings.Contains(out, "added list extra") {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, errb := runCLI(t, "--json", "version")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if !env.OK || env.Data.Version == "" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
}
