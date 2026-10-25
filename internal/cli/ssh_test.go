package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SuSonicTH/cpt/internal/bundle"
	"github.com/SuSonicTH/cpt/internal/cerr"
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

// writeFakeSSHBinaries puts fake scp/ssh executables first on PATH.
func writeFakeSSHBinaries(t *testing.T, sshBody, scpBody string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh/scp shell scripts are POSIX-only")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{"ssh": sshBody, "scp": scpBody} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSSHCLIUnknownSubcommandIsUsageError(t *testing.T) {
	root := sshCLIFixture(t)
	code, _, errb := runCLI(t, "--root", root, "ssh", "lst")
	if code != int(cerr.Usage) {
		t.Fatalf("unknown subcommand exit = %d, want %d (stderr=%s)", code, int(cerr.Usage), errb)
	}
	if !strings.Contains(errb, "unknown ssh subcommand") {
		t.Fatalf("unknown subcommand message = %q", errb)
	}
}

func TestSSHCLIRejectsReservedTargetName(t *testing.T) {
	root := sshCLIFixture(t)
	code, _, errb := runCLI(t, "--root", root, "ssh", "add", "list",
		"--bundle", "devbox", "--config-host", "staging-box")
	if code != int(cerr.Usage) || !strings.Contains(errb, "reserved") {
		t.Fatalf("reserved name accepted: exit=%d stderr=%s", code, errb)
	}
}

// TestSSHCLIConnectCopyFailureExitsNetwork pins the documented exit code: a
// failed scp must exit 5 even though the wrapped *exec.ExitError reports 1.
func TestSSHCLIConnectCopyFailureExitsNetwork(t *testing.T) {
	root := sshCLIFixture(t)
	if code, _, errb := runCLI(t, "--root", root, "ssh", "add", "staging",
		"--bundle", "devbox", "--config-host", "staging-box"); code != 0 {
		t.Fatalf("add failed: %d %s", code, errb)
	}
	writeFakeSSHBinaries(t, "#!/bin/sh\nexit 0\n", "#!/bin/sh\nexit 1\n")
	code, _, errb := runCLI(t, "--root", root, "ssh", "connect", "staging")
	if code != int(cerr.Network) {
		t.Fatalf("failed copy exit = %d, want %d (stderr=%s)", code, int(cerr.Network), errb)
	}
}

// TestSSHCLIConnectPropagatesSessionStatus checks that the remote shell status
// reaches the caller unchanged.
func TestSSHCLIConnectPropagatesSessionStatus(t *testing.T) {
	root := sshCLIFixture(t)
	if code, _, errb := runCLI(t, "--root", root, "ssh", "add", "staging",
		"--bundle", "devbox", "--config-host", "staging-box"); code != 0 {
		t.Fatalf("add failed: %d %s", code, errb)
	}
	writeFakeSSHBinaries(t, `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "-tt" ]; then exit 7; fi
done
exit 0
`, "#!/bin/sh\nexit 0\n")
	code, _, errb := runCLI(t, "--root", root, "ssh", "connect", "staging")
	if code != 7 {
		t.Fatalf("session exit = %d, want 7 (stderr=%s)", code, errb)
	}
}
