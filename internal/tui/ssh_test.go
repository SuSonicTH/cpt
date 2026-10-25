package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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
	m.bundles = []app.BundleInfo{{Name: "devbox", Target: "linux/amd64", Executable: true, SSHEligible: true}}
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
	m.bundles = []app.BundleInfo{{Name: "aaa", Target: "linux/amd64", Executable: true, SSHEligible: true}, {Name: "devbox", Target: "linux/amd64", Executable: true, SSHEligible: true}}
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

// wizardToDestination drives the wizard to step 3 with a custom server.
func wizardToDestination(t *testing.T) *Model {
	t.Helper()
	m := testModel(t)
	m.loadingRows = false
	installTUIBundle(t, m, "devbox")
	m.bundles = []app.BundleInfo{{Name: "devbox", Target: "linux/amd64", Executable: true, SSHEligible: true}}
	m.sshHosts = []sshtarget.ConfigHost{{Alias: "staging-box"}}
	m.setTab(tabSSH)
	send(m, "n")
	m.sshDraft.nameInput.SetValue("staging")
	send(m, "enter") // step 2
	send(m, "down")  // Custom server
	send(m, "enter") // step 3
	if m.sshDraft == nil || m.sshDraft.step != sshStepDestination {
		t.Fatalf("did not reach destination step: %+v", m.sshDraft)
	}
	if !m.sshDraft.custom {
		t.Fatal("expected the custom server source")
	}
	return m
}

// TestSSHWizardTypesJAndKInDestinationFields is the regression for the wizard
// stealing j/k from text entry: a hostname such as jump.example.com must land
// in the host field rather than moving focus.
func TestSSHWizardTypesJAndKInDestinationFields(t *testing.T) {
	m := wizardToDestination(t)
	for _, r := range "jump.example.com" {
		send(m, string(r))
	}
	if got := m.sshDraft.hostInput.Value(); got != "jump.example.com" {
		t.Fatalf("host input = %q, want jump.example.com", got)
	}
	if m.sshDraft.destField != 0 {
		t.Fatalf("typing j/k moved the field to %d", m.sshDraft.destField)
	}
	send(m, "down") // arrows still move between fields
	if m.sshDraft.destField != 1 {
		t.Fatalf("down did not move to the user field: %d", m.sshDraft.destField)
	}
	for _, r := range "jsmith" {
		send(m, string(r))
	}
	if got := m.sshDraft.userInput.Value(); got != "jsmith" {
		t.Fatalf("user input = %q, want jsmith", got)
	}
	if m.sshDraft.destField != 1 {
		t.Fatalf("typing a user moved the field to %d", m.sshDraft.destField)
	}
}

func TestSSHWizardRendersEveryStep(t *testing.T) {
	m := wizardToDestination(t)
	view := plainView(m)
	if !strings.Contains(view, "Step 3 of 5") {
		t.Fatalf("destination step did not render:\n%s", view)
	}
	m.sshDraft.hostInput.SetValue("10.0.0.7")
	for i := 0; i < 5; i++ {
		send(m, "enter")
	}
	if m.sshDraft.step != sshStepBundle {
		t.Fatalf("bundle step missing: %v", m.sshDraft.step)
	}
	if view := plainView(m); !strings.Contains(view, "Step 4 of 5") {
		t.Fatalf("bundle step did not render:\n%s", view)
	}
	send(m, "enter")
	if view := plainView(m); !strings.Contains(view, "Step 5 of 5") {
		t.Fatalf("options step did not render:\n%s", view)
	}
}

// TestSSHWizardRejectsNonPOSIXBundleBeforeSaving covers the picker offering a
// bundle that SaveSSHTarget would reject at the very end of the wizard.
func TestSSHWizardRejectsNonPOSIXBundleBeforeSaving(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	m.bundles = []app.BundleInfo{
		{Name: "windows", Target: "windows/amd64", Executable: true},
		{Name: "devbox", Target: "linux/amd64", Executable: true, SSHEligible: true},
	}
	m.sshHosts = []sshtarget.ConfigHost{{Alias: "staging-box"}}
	m.setTab(tabSSH)
	send(m, "n")
	m.sshDraft.nameInput.SetValue("staging")
	send(m, "enter")
	send(m, "enter")
	for i := 0; i < 4; i++ {
		send(m, "enter")
	}
	if m.sshDraft.step != sshStepBundle {
		t.Fatalf("bundle step missing: %v", m.sshDraft.step)
	}
	// The windows bundle is hidden entirely.
	if got := m.sshDraft.visibleBundles(); len(got) != 1 || m.sshDraft.bundles[got[0]].Name != "devbox" {
		t.Fatalf("visible bundles = %v, want only devbox", got)
	}
}

func TestSSHWizardWarnsBeforeOverwrite(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	installTUIBundle(t, m, "devbox")
	m.bundles = []app.BundleInfo{{Name: "devbox", Target: "linux/amd64", Executable: true, SSHEligible: true}}
	m.sshTargets = []app.SSHTargetInfo{{Target: sshtarget.Target{Name: "staging", Bundle: "devbox"}}}
	m.setTab(tabSSH)
	send(m, "n")
	m.sshDraft.nameInput.SetValue("staging")
	send(m, "enter")
	if m.sshDraft.step != sshStepName || !strings.Contains(m.sshDraft.err, "already exists") {
		t.Fatalf("overwrite was not confirmed: step=%v err=%q", m.sshDraft.step, m.sshDraft.err)
	}
	send(m, "enter")
	if m.sshDraft.step != sshStepSource {
		t.Fatalf("second enter did not continue: %v", m.sshDraft.step)
	}
}

func TestSSHWizardBackNavigationClearsStaleError(t *testing.T) {
	m := wizardToDestination(t)
	m.sshDraft.hostInput.SetValue("")
	// Enter walks the fields, then validates on the last one.
	for i := 0; i < 5; i++ {
		send(m, "enter")
	}
	if m.sshDraft.step != sshStepDestination || m.sshDraft.err == "" {
		t.Fatalf("expected a host validation error: step=%v err=%q", m.sshDraft.step, m.sshDraft.err)
	}
	// Shift+tab walks back through the fields and then to the previous step.
	for i := 0; i < 5; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	}
	if m.sshDraft.step != sshStepSource {
		t.Fatalf("shift+tab did not go back: %v", m.sshDraft.step)
	}
	if m.sshDraft.err != "" {
		t.Fatalf("stale error survived back-navigation: %q", m.sshDraft.err)
	}
	if view := plainView(m); strings.Contains(view, "hostname or IP is required") {
		t.Fatalf("stale error is still rendered:\n%s", view)
	}
}

// TestSSHWizardKeepsDraftWhenSaveFails drives the real save command against a
// bundle deleted after selection; the wizard must reopen with the input intact.
func TestSSHWizardKeepsDraftWhenSaveFails(t *testing.T) {
	m := wizardToDestination(t)
	m.sshDraft.hostInput.SetValue("10.0.0.7")
	for i := 0; i < 6; i++ {
		send(m, "enter")
	}
	if m.sshDraft.step != sshStepOptions {
		t.Fatalf("options step missing: %v", m.sshDraft.step)
	}
	for i := 0; i < 4; i++ {
		send(m, "down")
	}
	// Invalidate the executable so SaveSSHTarget rejects the draft.
	if err := os.Remove(filepath.Join(m.app.Paths.Bundles, "devbox-linux-amd64")); err != nil {
		t.Fatal(err)
	}
	_, cmd := m.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("save did not return a command")
	}
	msg := cmd()
	failure, ok := msg.(sshSaveFailedMsg)
	if !ok || failure.err == nil {
		t.Fatalf("save msg = %#v, want sshSaveFailedMsg", msg)
	}
	m.Update(failure)
	if m.sshDraft == nil {
		t.Fatal("draft was discarded after a failed save")
	}
	if m.sshDraft.step != sshStepOptions || m.sshDraft.name != "staging" {
		t.Fatalf("restored draft lost state: %+v", m.sshDraft)
	}
	if m.sshDraft.err == "" {
		t.Fatal("save error was not shown in the wizard")
	}
	if m.loadingRows {
		t.Fatal("loadingRows stayed set after a failed save")
	}
}

func TestSSHHostsAreSanitized(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	m.Update(snapshotMsg{sshHosts: []sshtarget.ConfigHost{{Alias: "evil\u202Ealias"}}})
	for _, h := range m.sshHosts {
		if strings.ContainsRune(h.Alias, '\u202E') {
			t.Fatalf("host alias was not sanitized: %q", h.Alias)
		}
	}
}

func TestSSHDeletePromptRespectsBusy(t *testing.T) {
	m := testModel(t)
	m.loadingRows = true
	m.sshTargets = []app.SSHTargetInfo{{Target: sshtarget.Target{Name: "staging", Bundle: "devbox"}}}
	m.setTab(tabSSH)
	send(m, "d")
	if m.prompt != nil {
		t.Fatal("delete prompt opened while an operation was running")
	}
	if !m.statusErr {
		t.Fatalf("expected a busy status, got %q", m.status)
	}
}

// TestSSHWizardEditSelectsCorrectFilteredBundle guards the interaction between
// the eligibility filter and the cursor: the wizard cursor indexes the filtered
// list, so an ineligible bundle listed before the target's own bundle must not
// shift the selection. (The original defect was the raw-index cursor; this
// covers the filter/index pair introduced by the fix.)
func TestSSHWizardEditSelectsCorrectFilteredBundle(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	installTUIBundle(t, m, "devbox")
	if err := m.app.SaveSSHTarget(&sshtarget.Target{
		Name:   "staging",
		Bundle: "devbox",
		SSH:    sshtarget.SSH{Source: sshtarget.SourceCustom, Host: "10.0.0.7"},
	}); err != nil {
		t.Fatal(err)
	}
	targets, err := m.app.SSHTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.sshTargets = sanitizeSSHTargets(targets)
	m.bundles = []app.BundleInfo{
		{Name: "winbox", Target: "windows/amd64", Executable: true},
		{Name: "devbox", Target: "linux/amd64", Executable: true, SSHEligible: true},
		{Name: "extra", Target: "linux/amd64", Executable: true, SSHEligible: true},
	}
	m.setTab(tabSSH)
	m.sshCursor = 0
	send(m, "e")
	d := m.sshDraft
	if d == nil {
		t.Fatal("edit did not open the wizard")
	}
	send(m, "enter") // source -> destination
	for i := 0; i < 5; i++ {
		send(m, "enter") // walk fields, then to the bundle step
	}
	if d.step != sshStepBundle {
		t.Fatalf("did not reach the bundle step: %v", d.step)
	}
	if got := d.selectedBundle(); got != "devbox" {
		t.Fatalf("edit selected bundle %q, want devbox", got)
	}
}

func TestSSHWizardOverwriteWarningTracksEditedName(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	installTUIBundle(t, m, "devbox")
	m.bundles = []app.BundleInfo{{Name: "devbox", Target: "linux/amd64", Executable: true, SSHEligible: true}}
	m.sshTargets = []app.SSHTargetInfo{{Target: sshtarget.Target{Name: "staging", Bundle: "devbox"}}}
	m.setTab(tabSSH)
	send(m, "n")
	m.sshDraft.nameInput.SetValue("staging")
	send(m, "enter")
	if !strings.Contains(m.sshDraft.err, "already exists") || m.sshDraft.warnedName != "staging" {
		t.Fatalf("warning state = %q/%q", m.sshDraft.err, m.sshDraft.warnedName)
	}
	// Editing the name invalidates the confirmation.
	send(m, "x")
	if m.sshDraft.warnedName != "" {
		t.Fatalf("warnedName survived an edit: %q", m.sshDraft.warnedName)
	}
	m.sshDraft.nameInput.SetValue("staging")
	send(m, "enter")
	if !strings.Contains(m.sshDraft.err, "already exists") {
		t.Fatalf("warning was not re-shown: %q", m.sshDraft.err)
	}
}

func TestSSHWizardRejectsReservedName(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	m.bundles = []app.BundleInfo{{Name: "devbox", Target: "linux/amd64", Executable: true, SSHEligible: true}}
	m.setTab(tabSSH)
	send(m, "n")
	m.sshDraft.nameInput.SetValue("list")
	send(m, "enter")
	if m.sshDraft.step != sshStepName || !strings.Contains(m.sshDraft.err, "reserved") {
		t.Fatalf("reserved name accepted: step=%v err=%q", m.sshDraft.step, m.sshDraft.err)
	}
}

// TestSSHViewDoesNotMutateDraft guards render purity: View() must not fix up
// wizard cursor state, because callers may render without dispatching a key.
func TestSSHViewDoesNotMutateDraft(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	m.bundles = []app.BundleInfo{{Name: "devbox", Target: "linux/amd64", Executable: true, SSHEligible: true}}
	m.setTab(tabSSH)
	send(m, "n")
	m.sshDraft.step = sshStepBundle
	m.sshDraft.bundleCursor = 99
	m.sshDraft.sourceCursor = 42
	_ = plainView(m)
	if m.sshDraft.bundleCursor != 99 || m.sshDraft.sourceCursor != 42 {
		t.Fatalf("render mutated draft state: bundle=%d source=%d", m.sshDraft.bundleCursor, m.sshDraft.sourceCursor)
	}
}
