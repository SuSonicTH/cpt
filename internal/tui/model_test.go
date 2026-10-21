package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/progress"
	"github.com/SuSonicTH/cpt/internal/state"
)

func testModel(t *testing.T) *Model {
	t.Helper()
	a, err := app.Open(app.Options{
		Root:   t.TempDir(),
		Quiet:  true,
		Stdout: io.Discard,
		Stderr: io.Discard,
		Events: progress.New(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m := newModel(context.Background(), a)
	m.width = 110
	m.height = 30
	m.resize()
	t.Cleanup(func() {
		m.shutdown()
		a.Close()
	})
	return m
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func send(m *Model, s string) {
	m.Update(keyMsg(s))
}

func TestBundleWizardCollectsInput(t *testing.T) {
	m := testModel(t)
	m.loadingRows = false
	m.setTab(tabBundles)
	send(m, "n")
	if m.prompt == nil || m.prompt.kind != promptBundleName {
		t.Fatalf("bundle wizard did not start: %+v", m.prompt)
	}
	m.prompt.input.SetValue("devbox")
	send(m, "enter")
	if m.prompt == nil || m.prompt.kind != promptBundleTarget {
		t.Fatalf("target prompt missing: %+v", m.prompt)
	}
	send(m, "enter")
	if m.prompt == nil || m.prompt.kind != promptBundlePackages {
		t.Fatalf("packages prompt missing: %+v", m.prompt)
	}
	send(m, "enter")
	if m.prompt == nil || m.prompt.kind != promptBundleFrozen {
		t.Fatalf("frozen prompt missing: %+v", m.prompt)
	}
	send(m, "enter")
	if m.running != true || m.bundleDraft != nil {
		t.Fatalf("bundle build did not start: running=%v draft=%+v", m.running, m.bundleDraft)
	}
}

func loadRows(t *testing.T, m *Model) {
	t.Helper()
	_, cmd := m.Update(snapshotMsg{
		rows: []app.Row{
			{Name: "fzf", List: "core", Status: "update", Version: "0.1.0", Latest: "0.2.0", Description: "fuzzy finder", Tags: "shell"},
			{Name: "jq", List: "core", Status: "installed", Version: "1.0.0", Description: "JSON processor", Tags: "json"},
			{Name: "rg", List: "core", Status: "available", Description: "fast grep", Tags: "search"},
		},
		lists:  []app.ListInfo{{Name: "core", Packages: 3, Builtin: true}},
		shells: nil,
	})
	if cmd != nil {
		t.Fatalf("snapshot should not start commands")
	}
	if m.loadingRows {
		t.Fatal("snapshot should clear loadingRows")
	}
	if len(m.filtered) != 3 {
		t.Fatalf("filtered = %d, want 3", len(m.filtered))
	}
}

func TestModelSearchAndClear(t *testing.T) {
	m := testModel(t)
	loadRows(t, m)

	send(m, "/")
	if !m.searching {
		t.Fatal("search did not start")
	}
	send(m, "json")
	if len(m.filtered) != 1 || m.currentRow().Name != "jq" {
		t.Fatalf("unexpected filter: %+v", m.filtered)
	}
	send(m, "esc")
	if m.searching || m.query != "" || len(m.filtered) != 3 {
		t.Fatalf("search was not cleared: searching=%v query=%q filtered=%d", m.searching, m.query, len(m.filtered))
	}
}

func TestModelSelectionAndUpdateTargets(t *testing.T) {
	m := testModel(t)
	loadRows(t, m)

	send(m, "a")
	if !m.selected["fzf"] || m.selectionCount() != 1 {
		t.Fatalf("select all updates failed: %+v", m.selected)
	}
	send(m, "a")
	if m.selectionCount() != 0 {
		t.Fatalf("second select all should clear: %+v", m.selected)
	}

	m.cursor = 2
	send(m, " ")
	if !m.selected["rg"] {
		t.Fatalf("space did not select current row: %+v", m.selected)
	}
	send(m, " ")
	if m.selected["rg"] {
		t.Fatalf("space did not unselect current row: %+v", m.selected)
	}
}

func TestModelTabsAndPrompt(t *testing.T) {
	m := testModel(t)
	loadRows(t, m)

	send(m, "2")
	if m.tab != tabLists {
		t.Fatalf("tab = %v, want lists", m.tab)
	}
	view := m.View()
	for _, want := range []string{"Packages", "Lists", "Bundles", "Log", "NAME"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}

	send(m, "n")
	if m.prompt == nil || m.prompt.kind != promptAddList {
		t.Fatalf("add-list prompt did not open: %+v", m.prompt)
	}
	send(m, "esc")
	if m.prompt != nil {
		t.Fatal("esc did not close the prompt")
	}

	send(m, "3")
	if m.tab != tabBundles {
		t.Fatalf("tab = %v, want bundles", m.tab)
	}
	send(m, "4")
	if m.tab != tabLog {
		t.Fatalf("tab = %v, want log", m.tab)
	}
	send(m, "?")
	if !m.help {
		t.Fatal("help did not open")
	}
	if !strings.Contains(m.View(), "Global") {
		t.Fatalf("help view missing keys:\n%s", m.View())
	}
	send(m, "?")
	if m.help {
		t.Fatal("help did not close")
	}
}

func TestModelProgressEvents(t *testing.T) {
	m := testModel(t)
	loadRows(t, m)

	m.Update(eventMsg(progress.Event{Kind: progress.KindDownloading, Package: "fzf", Done: 5, Total: 10}))
	a := m.activities["fzf"]
	if a == nil || a.State != "downloading" || a.Total != 10 {
		t.Fatalf("unexpected activity: %+v", a)
	}
	m.Update(eventMsg(progress.Event{Kind: progress.KindDone, Package: "fzf", Version: "0.2.0", Action: "updated"}))
	if !a.Finished || a.Action != "updated" {
		t.Fatalf("done event did not finish activity: %+v", a)
	}

	m.Update(eventMsg(progress.Event{Kind: progress.KindInfo, Message: "warning: something"}))
	if len(m.liveLog) != 1 || m.liveLog[0] != "warning: something" {
		t.Fatalf("info event was not captured: %+v", m.liveLog)
	}
}

func TestModelQuitStopsEventPump(t *testing.T) {
	m := testModel(t)
	_, cmd := m.Update(keyMsg("q"))
	if cmd == nil {
		t.Fatal("q did not return a quit command")
	}
	select {
	case <-m.done:
	default:
		t.Fatal("done channel was not closed")
	}
}

func TestModelSearchAfterScrollKeepsResultVisible(t *testing.T) {
	m := testModel(t)
	var rows []app.Row
	for i := 0; i < 80; i++ {
		rows = append(rows, app.Row{Name: fmt.Sprintf("pkg%02d", i), List: "core", Status: "available"})
	}
	m.Update(snapshotMsg{rows: rows})
	m.cursor = 70
	m.ensureCursorVisible(m.bodyHeight())
	if m.offset == 0 {
		t.Fatal("expected a scrolled list")
	}
	m.query = "pkg79"
	m.refilter()
	if len(m.filtered) != 1 || m.offset != 0 {
		t.Fatalf("filtered=%d offset=%d, want 1/0", len(m.filtered), m.offset)
	}
	view := m.View()
	if !strings.Contains(view, "pkg79") {
		t.Fatalf("filtered row is not rendered:\n%s", view)
	}
}

func TestModelFuzzySearch(t *testing.T) {
	m := testModel(t)
	loadRows(t, m)
	m.query = "fz"
	m.refilter()
	if len(m.filtered) != 1 || m.currentRow().Name != "fzf" {
		t.Fatalf("fuzzy query did not match fzf: %+v", m.filtered)
	}
}

func TestModelLogTabSchedulesRead(t *testing.T) {
	m := testModel(t)
	m.logReading = false
	cmd := m.setTab(tabLog)
	if cmd == nil {
		t.Fatal("switching to the log tab should start a read")
	}
	if !m.logReading {
		t.Fatal("log read should be marked in flight")
	}
	m.Update(logMsg{})
	if m.logReading {
		t.Fatal("log read flag was not cleared")
	}
}

func TestModelDetailOverlay(t *testing.T) {
	m := testModel(t)
	loadRows(t, m)
	send(m, "enter")
	if m.overlay != overlayDetail {
		t.Fatalf("enter did not open detail: %v", m.overlay)
	}
	view := m.View()
	if !strings.Contains(view, "fuzzy finder") {
		t.Fatalf("detail view missing description:\n%s", view)
	}
	send(m, "esc")
	if m.overlay != overlayNone {
		t.Fatal("esc did not close detail")
	}
}

func TestSanitizeStripsControlSequences(t *testing.T) {
	got := sanitize("a\x1b[31mb\u202ec\x07", false)
	if strings.ContainsAny(got, "\x1b\x07\u202e") {
		t.Fatalf("control characters survived sanitisation: %q", got)
	}
	if got != "a[31mbc" {
		t.Fatalf("unexpected sanitised text %q", got)
	}
	if got := sanitize("line1\nline2", true); got != "line1\nline2" {
		t.Fatalf("newlines should survive in diffs: %q", got)
	}
}

func TestModelShellOverlayOpensWithoutReadingAppStateInView(t *testing.T) {
	m := testModel(t)
	loadRows(t, m)
	m.shells = []app.ShellInfo{{Package: "fzf", Pending: true, PendingVersion: "0.2.0"}}
	m.pendingShells = 1
	send(m, "2")
	send(m, "s")
	if m.overlay != overlayShell {
		t.Fatalf("s did not open the shell overlay: %v", m.overlay)
	}
	view := m.View()
	if !strings.Contains(view, "Shell changes") || !strings.Contains(view, "fzf") {
		t.Fatalf("shell overlay is missing content:\n%s", view)
	}
	send(m, "esc")
	if m.overlay != overlayNone {
		t.Fatal("esc did not close the shell overlay")
	}
}

func TestModelForceRemovePrompt(t *testing.T) {
	m := testModel(t)
	loadRows(t, m)
	m.cursor = 0 // fzf is installed (update)
	send(m, "X")
	if m.prompt == nil || m.prompt.kind != promptForceRemove {
		t.Fatalf("X did not open a force-remove prompt: %+v", m.prompt)
	}
}

func TestShellDiffRefreshesWhenPendingSetShrinks(t *testing.T) {
	m := testModel(t)
	loadRows(t, m)
	m.app.State.Shell["a"] = &state.Shell{Pending: &state.ShellSnippet{Bash: "echo a"}, PendingSHA256: "ha"}
	m.app.State.Shell["b"] = &state.Shell{Pending: &state.ShellSnippet{Bash: "echo b"}, PendingSHA256: "hb"}
	m.Update(snapshotMsg{
		rows:    m.rows,
		shells:  []app.ShellInfo{{Package: "a", Pending: true}, {Package: "b", Pending: true}},
		pending: 2,
	})
	send(m, "2")
	send(m, "s")
	send(m, "down")
	if !strings.Contains(m.shellDiff, "echo b") {
		t.Fatalf("expected b's diff, got %q", m.shellDiff)
	}

	m.Update(snapshotMsg{
		rows:    m.rows,
		shells:  []app.ShellInfo{{Package: "a", Pending: true}},
		pending: 1,
	})
	if m.shellCursor != 0 {
		t.Fatalf("cursor was not clamped: %d", m.shellCursor)
	}
	if !strings.Contains(m.shellDiff, "echo a") || strings.Contains(m.shellDiff, "echo b") {
		t.Fatalf("stale diff after clamp: %q", m.shellDiff)
	}
}

func TestModelCancelledRefreshKeepsPreviousRows(t *testing.T) {
	m := testModel(t)
	loadRows(t, m)
	before := len(m.rows)
	m.Update(snapshotMsg{err: context.Canceled})
	if len(m.rows) != before {
		t.Fatalf("cancelled refresh replaced rows: %d -> %d", before, len(m.rows))
	}
	if m.status != "refresh cancelled" || m.statusErr {
		t.Fatalf("unexpected cancellation status %q (err=%v)", m.status, m.statusErr)
	}
}
