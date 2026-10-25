package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/bundle"
	"github.com/SuSonicTH/cpt/internal/sshtarget"
)

func installTUIBundle(t *testing.T, m *Model, name string) {
	t.Helper()
	manifest := bundle.Manifest{
		Schema:     bundle.ManifestSchema,
		Name:       name,
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
	if err := manifest.Save(filepath.Join(m.app.Paths.Bundles, bundle.FileName(name))); err != nil {
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
	if err := os.WriteFile(manifest.OutputPath(m.app.Paths.Bundles), data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSSHTabShowsTargetsAndDetails(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	m.sshTargets = []app.SSHTargetInfo{{
		Target: sshtarget.Target{
			Name:   "staging",
			Bundle: "devbox",
			SSH:    sshtarget.SSH{Source: sshtarget.SourceConfig, Host: "staging-box"},
			Shell:  sshtarget.ShellBash,
		},
		BundleTarget:      "linux/amd64",
		BundleExists:      true,
		BundleValid:       true,
		BundleSize:        2048,
		RemoteRootDisplay: "~/.cpt",
		Destination:       "staging-box",
		Resolved: &sshtarget.ResolvedHost{
			Alias:    "staging-box",
			HostName: "10.0.0.5",
			User:     "mike",
			Port:     2222,
		},
		CacheFresh: true,
	}}
	m.setTab(tabSSH)
	view := plainView(m)
	for _, want := range []string{"staging", "staging-box", "devbox", "10.0.0.5", "fresh (shell only)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("SSH view missing %q:\n%s", want, view)
		}
	}
	for _, want := range []string{"[c] connect", "[n] new"} {
		if !strings.Contains(view, want) {
			t.Fatalf("SSH footer missing %q:\n%s", want, view)
		}
	}
}

func TestSSHWizardCreatesTarget(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	installTUIBundle(t, m, "devbox")
	m.bundles = []app.BundleInfo{{Name: "devbox", Target: "linux/amd64"}}
	m.sshHosts = []sshtarget.ConfigHost{{Alias: "staging-box"}}
	m.setTab(tabSSH)

	send(m, "n")
	if m.sshDraft == nil || m.sshDraft.step != sshStepName {
		t.Fatalf("wizard did not start: %+v", m.sshDraft)
	}
	m.sshDraft.nameInput.SetValue("staging")
	send(m, "enter")
	if m.sshDraft.step != sshStepSource {
		t.Fatalf("source step missing: %v", m.sshDraft.step)
	}
	send(m, "enter")
	if m.sshDraft.step != sshStepDestination || m.sshDraft.configHost != "staging-box" {
		t.Fatalf("destination step missing: %+v", m.sshDraft)
	}
	for i := 0; i < 4; i++ {
		send(m, "enter")
	}
	if m.sshDraft.step != sshStepBundle {
		t.Fatalf("bundle step missing: %v", m.sshDraft.step)
	}
	send(m, "enter")
	if m.sshDraft.step != sshStepOptions {
		t.Fatalf("options step missing: %v", m.sshDraft.step)
	}
	for i := 0; i < 4; i++ {
		send(m, "down")
	}
	_, cmd := m.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("save did not return a command")
	}
	msg, ok := cmd().(actionDoneMsg)
	if !ok || msg.err != nil {
		t.Fatalf("save failed: %+v", msg)
	}
	if m.sshDraft != nil {
		t.Fatal("wizard stayed open after save")
	}
	saved, err := m.app.SSHTarget("staging")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Bundle != "devbox" || saved.SSH.Source != sshtarget.SourceConfig || saved.SSH.Host != "staging-box" {
		t.Fatalf("unexpected saved target: %+v", saved)
	}
}

func TestSSHWizardEditPrefillsTarget(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	installTUIBundle(t, m, "devbox")
	if err := m.app.SaveSSHTarget(&sshtarget.Target{
		Name:          "staging",
		Bundle:        "devbox",
		SSH:           sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "10.0.0.7", User: "mike", Port: 2222},
		RemoteRoot:    "/srv/cpt",
		Shell:         sshtarget.ShellZsh,
		CleanupOnExit: true,
	}); err != nil {
		t.Fatal(err)
	}
	targets, err := m.app.SSHTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.sshTargets = sanitizeSSHTargets(targets)
	m.bundles = []app.BundleInfo{{Name: "aaa", Target: "linux/amd64"}, {Name: "devbox", Target: "linux/amd64"}}
	m.setTab(tabSSH)
	m.sshCursor = 0
	send(m, "e")
	d := m.sshDraft
	if d == nil {
		t.Fatal("edit did not open the wizard")
	}
	if d.name != "staging" || !d.custom || d.hostInput.Value() != "10.0.0.7" {
		t.Fatalf("name/source not pre-filled: %+v", d)
	}
	if d.userInput.Value() != "mike" || d.portInput.Value() != "2222" || d.remoteRoot.Value() != "/srv/cpt" {
		t.Fatalf("destination/options not pre-filled: %+v", d)
	}
	if d.selectedShell() != sshtarget.ShellZsh || !d.cleanup {
		t.Fatalf("shell/cleanup not pre-filled: %+v", d)
	}
	send(m, "enter")
	for i := 0; i < 5; i++ {
		send(m, "enter")
	}
	if d.step != sshStepBundle {
		t.Fatalf("did not reach bundle step: %v", d.step)
	}
	if got := d.selectedBundle(); got != "devbox" {
		t.Fatalf("edit lost bundle selection: %q", got)
	}
}

func TestSSHConnectProcessUsesCurrentRoot(t *testing.T) {
	m := testModel(t)
	cmd, err := sshConnectProcess(m.app, "staging")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--root", m.app.Paths.Root, "ssh staging"} {
		if !strings.Contains(args, want) {
			t.Fatalf("connect command missing %q: %s", want, args)
		}
	}
}

func TestSSHDeletePromptAndOfflineConnect(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	m.sshTargets = []app.SSHTargetInfo{{Target: sshtarget.Target{Name: "staging", Bundle: "devbox"}}}
	m.setTab(tabSSH)
	send(m, "d")
	if m.prompt == nil || m.prompt.kind != promptRemoveSSH || m.prompt.list != "staging" {
		t.Fatalf("delete prompt = %+v", m.prompt)
	}
	send(m, "esc")
	m.app.Offline = true
	send(m, "c")
	if !m.statusErr || !strings.Contains(m.status, "offline") {
		t.Fatalf("offline connect status = %q (err=%v)", m.status, m.statusErr)
	}
	m.app.Offline = false
	m.sshTargets[0].BundleExists = true
	m.sshTargets[0].BundleValid = false
	send(m, "c")
	if !m.statusErr || !strings.Contains(m.status, "invalid") {
		t.Fatalf("invalid-bundle connect status = %q (err=%v)", m.status, m.statusErr)
	}
}
