package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SuSonicTH/cpt/internal/bundle"
)

func sshCLIFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bundles"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := bundle.Manifest{
		Schema:     bundle.ManifestSchema,
		Name:       "devbox",
		Target:     "linux/amd64",
		CptVersion: "test",
		Packages: []bundle.ManifestPackage{{
			Name:    "fzf",
			Tag:     "v1",
			Version: "1.0.0",
			Asset:   "fzf.tar.gz",
			Bins:    []string{"fzf"},
		}},
		Lists: map[string]bundle.ManifestList{},
	}
	if err := m.Save(filepath.Join(root, "bundles", "devbox.json")); err != nil {
		t.Fatal(err)
	}
	base := []byte("base")
	payload := []byte("payload")
	footer := bundle.Footer{
		PayloadOffset: uint64(len(base)),
		PayloadLen:    uint64(len(payload)),
		Version:       bundle.FormatVersion,
	}
	data := append(append(append([]byte{}, base...), payload...), footer.Marshal()...)
	if err := os.WriteFile(filepath.Join(root, "bundles", "devbox-linux-amd64"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSSHCLIAddListEditRemove(t *testing.T) {
	root := sshCLIFixture(t)
	code, out, errb := runCLI(t, "--root", root, "--json", "ssh", "add", "staging",
		"--bundle", "devbox", "--config-host", "staging-box", "--cleanup")
	if code != 0 {
		t.Fatalf("add exit = %d, stderr = %s", code, errb)
	}
	if !strings.Contains(out, `"name": "staging"`) || !strings.Contains(out, `"cleanup_on_exit": true`) {
		t.Fatalf("unexpected add output:\n%s", out)
	}

	code, out, errb = runCLI(t, "--root", root, "--json", "ssh", "list")
	if code != 0 {
		t.Fatalf("list exit = %d, stderr = %s", code, errb)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data []struct {
			Name        string `json:"name"`
			Bundle      string `json:"bundle"`
			BundlePath  string `json:"bundle_path"`
			BundleValid bool   `json:"bundle_valid"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("invalid list JSON: %v\n%s", err, out)
	}
	if !env.OK || len(env.Data) != 1 || env.Data[0].Name != "staging" || env.Data[0].Bundle != "devbox" || !env.Data[0].BundleValid {
		t.Fatalf("unexpected list: %+v", env)
	}

	code, _, errb = runCLI(t, "--root", root, "ssh", "edit", "staging", "--shell", "zsh", "--no-cleanup")
	if code != 0 {
		t.Fatalf("edit exit = %d, stderr = %s", code, errb)
	}
	code, out, _ = runCLI(t, "--root", root, "--json", "ssh", "list")
	if code != 0 {
		t.Fatalf("list after edit exit = %d", code)
	}
	if !strings.Contains(out, `"shell": "zsh"`) || strings.Contains(out, `"cleanup_on_exit": true`) {
		t.Fatalf("edit was not applied:\n%s", out)
	}

	code, _, errb = runCLI(t, "--root", root, "ssh", "remove", "staging")
	if code != 0 {
		t.Fatalf("remove exit = %d, stderr = %s", code, errb)
	}
	code, out, _ = runCLI(t, "--root", root, "--json", "ssh", "list")
	if code != 0 || !strings.Contains(out, `"data"`) {
		t.Fatalf("list after remove failed: code=%d out=%s", code, out)
	}
}

func TestSSHCLIRejectsMissingBundleAndUnknownTarget(t *testing.T) {
	root := sshCLIFixture(t)
	code, _, _ := runCLI(t, "--root", root, "ssh", "add", "bad", "--bundle", "nope", "--host", "box")
	if code != 2 {
		t.Fatalf("missing bundle exit = %d, want 2", code)
	}
	code, _, _ = runCLI(t, "--root", root, "ssh", "missing")
	if code != 2 {
		t.Fatalf("unknown target exit = %d, want 2", code)
	}
	code, _, _ = runCLI(t, "--root", root, "--json", "ssh", "missing")
	if code != 2 {
		t.Fatalf("json connect exit = %d, want 2", code)
	}
}

func TestSSHCLIRejectsInvalidBundleExecutable(t *testing.T) {
	root := sshCLIFixture(t)
	if err := os.WriteFile(filepath.Join(root, "bundles", "devbox-linux-amd64"), []byte("broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runCLI(t, "--root", root, "ssh", "add", "bad", "--bundle", "devbox", "--host", "box")
	if code != 2 || !strings.Contains(errb, "valid executable") {
		t.Fatalf("invalid bundle exit=%d stderr=%s", code, errb)
	}
}
