package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/bundle"
	"github.com/SuSonicTH/cpt/internal/platform"
	"github.com/SuSonicTH/cpt/internal/progress"
)

// Update handles a Bubble Tea message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case snapshotMsg:
		if m.probeCancel != nil {
			m.probeCancel()
			m.probeCancel = nil
		}
		m.loadingRows = false
		m.rowErr = ""
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) {
				return m, m.setStatus("refresh cancelled", false)
			}
			m.rowErr = sanitize(msg.err.Error(), false)
		} else {
			sanitizeRows(msg.rows)
			m.setRows(msg.rows)
		}
		m.lists = sanitizeLists(msg.lists)
		m.bundles = sanitizeBundles(msg.bundles)
		m.sshTargets = sanitizeSSHTargets(msg.sshTargets)
		m.sshHosts = msg.sshHosts
		m.sshHostsErr = ""
		if msg.sshHostErr != nil {
			m.sshHostsErr = sanitize(msg.sshHostErr.Error(), false)
		}
		m.shells = sanitizeShells(msg.shells)
		m.pendingShells = msg.pending
		if m.listCursor >= len(m.lists) {
			m.listCursor = len(m.lists) - 1
		}
		if m.listCursor < 0 {
			m.listCursor = 0
		}
		m.ensureListVisible(m.listTableHeight())
		if m.bundleCursor >= len(m.bundles) {
			m.bundleCursor = len(m.bundles) - 1
		}
		if m.bundleCursor < 0 {
			m.bundleCursor = 0
		}
		m.ensureBundleVisible(m.bundleTableHeight())
		if m.sshCursor >= len(m.sshTargets) {
			m.sshCursor = len(m.sshTargets) - 1
		}
		if m.sshCursor < 0 {
			m.sshCursor = 0
		}
		m.ensureSSHVisible(m.sshTableHeight())
		for name := range m.selected {
			found := false
			for i := range m.rows {
				if m.rows[i].Name == name {
					found = true
					break
				}
			}
			if !found {
				delete(m.selected, name)
			}
		}
		if m.overlay == overlayShell {
			m.shellPending = m.pendingShellNames()
			if len(m.shellPending) == 0 {
				m.overlay = overlayNone
			} else {
				if m.shellCursor >= len(m.shellPending) {
					m.shellCursor = len(m.shellPending) - 1
				}
				m.ensureShellVisible(m.shellListHeight())
				// Always refresh: the cursor may have been clamped onto a
				// different package, and approving the wrong diff is unsafe.
				m.refreshShellDiff()
			}
		}
		if msg.initial && !m.app.Offline {
			// The cached catalog is already visible; now refresh stale lists
			// (if any) and probe upstream versions.
			return m, m.startReload(m.app.ListsStale())
		}
		return m, nil
	case logMsg:
		m.logReading = false
		if msg.err != nil {
			m.logErr = sanitize(msg.err.Error(), false)
		} else {
			m.logErr = ""
			m.logLines = make([]string, len(msg.lines))
			for i, line := range msg.lines {
				m.logLines[i] = sanitize(line, false)
			}
			m.refreshLog()
		}
		return m, nil
	case eventMsg:
		cmd := m.applyEvent(progress.Event(msg))
		return m, tea.Batch(cmd, waitEventCmd(m.events, m.done))
	case tickMsg:
		m.spinner++
		var cmds []tea.Cmd
		cmds = append(cmds, tickCmd())
		if m.tab == tabLog && !m.logReading {
			m.logReading = true
			cmds = append(cmds, readLogCmd(m.app.Paths.Logs))
		}
		return m, tea.Batch(cmds...)
	case clearStatusMsg:
		if msg.seq == m.statusSeq {
			m.status = ""
			m.statusErr = false
		}
		return m, nil
	case opDoneMsg:
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		m.running = false
		m.opName = ""
		text, isErr := summarizeResults(msg.op, msg.results, msg.err)
		logged := false
		for _, r := range msg.results {
			if r.Err != nil {
				m.appendLog("✗ " + r.Name + ": " + r.Err.Error())
				logged = true
			}
		}
		if msg.err != nil && !logged {
			m.appendLog("✗ " + msg.err.Error())
		}
		return m, tea.Batch(m.setStatus(text, isErr), m.startReload(false))
	case actionDoneMsg:
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		m.running = false
		m.opName = ""
		text := msg.status
		isErr := msg.err != nil
		for _, w := range msg.warnings {
			m.appendLog("warning: " + w)
		}
		if isErr {
			text = msg.err.Error()
			m.appendLog("✗ " + msg.err.Error())
		}
		return m, tea.Batch(m.setStatus(text, isErr), m.startReload(false))
	}
	return m, nil
}

func (m *Model) listTableHeight() int { return maxInt(3, m.bodyHeight()-8) }

func (m *Model) bundleTableHeight() int { return maxInt(3, m.bodyHeight()-1) }

func (m *Model) shellListHeight() int { return maxInt(2, m.bodyHeight()-2) }

func (m *Model) ensureBundleVisible(height int) {
	if height < 1 {
		height = 1
	}
	if m.bundleCursor < m.bundleOffset {
		m.bundleOffset = m.bundleCursor
	}
	if m.bundleCursor >= m.bundleOffset+height {
		m.bundleOffset = m.bundleCursor - height + 1
	}
	maxOffset := len(m.bundles) - height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.bundleOffset > maxOffset {
		m.bundleOffset = maxOffset
	}
	if m.bundleOffset < 0 {
		m.bundleOffset = 0
	}
}

func (m *Model) ensureShellVisible(height int) {
	if height < 1 {
		height = 1
	}
	if m.shellCursor < m.shellListOffset {
		m.shellListOffset = m.shellCursor
	}
	if m.shellCursor >= m.shellListOffset+height {
		m.shellListOffset = m.shellCursor - height + 1
	}
	maxOffset := len(m.shellPending) - height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.shellListOffset > maxOffset {
		m.shellListOffset = maxOffset
	}
	if m.shellListOffset < 0 {
		m.shellListOffset = 0
	}
}

func (m *Model) ensureListVisible(height int) {
	if height < 1 {
		height = 1
	}
	if m.listCursor < m.listOffset {
		m.listOffset = m.listCursor
	}
	if m.listCursor >= m.listOffset+height {
		m.listOffset = m.listCursor - height + 1
	}
	maxOffset := len(m.lists) - height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.listOffset > maxOffset {
		m.listOffset = maxOffset
	}
	if m.listOffset < 0 {
		m.listOffset = 0
	}
}

func (m *Model) resize() {
	body := m.bodyHeight()
	m.log.Width = maxInt(20, m.width)
	m.log.Height = maxInt(3, body-1)
	m.search.Width = maxInt(10, m.width-6)
	if m.prompt != nil {
		m.prompt.input.Width = maxInt(10, m.width-20)
	}
	if m.bundleDraft != nil {
		m.bundleDraft.nameInput.Width = maxInt(10, m.width-10)
		m.bundleDraft.search.Width = maxInt(10, m.width-14)
	}
	if m.sshDraft != nil {
		m.resizeSSHDraft()
	}
	m.ensureCursorVisible(body)
	m.ensureListVisible(m.listTableHeight())
	m.ensureBundleVisible(m.bundleTableHeight())
	m.ensureSSHVisible(m.sshTableHeight())
	if m.overlay == overlayShell {
		m.ensureShellVisible(m.shellListHeight())
		m.wrapShellDiff()
	}
	m.refreshLog()
}

func (m *Model) bodyHeight() int {
	return maxInt(1, m.height-3)
}

func (m *Model) ensureCursorVisible(height int) {
	if height < 1 {
		height = 1
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+height {
		m.offset = m.cursor - height + 1
	}
	maxOffset := len(m.filtered) - height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.offset > maxOffset {
		m.offset = maxOffset
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *Model) refreshLog() {
	lines := make([]string, 0, len(m.logLines)+len(m.liveLog))
	lines = append(lines, m.logLines...)
	lines = append(lines, m.liveLog...)
	atBottom := m.log.AtBottom()
	m.log.SetContent(strings.Join(lines, "\n"))
	if atBottom || len(m.logLines) == 0 {
		m.log.GotoBottom()
	}
}

func (m *Model) busy() bool { return m.running || m.loadingRows }

func (m *Model) clearStatus() {
	m.status = ""
	m.statusErr = false
}

// startReload probes upstream versions (and optionally refreshes stale lists)
// on a cancellable context. Only one reload runs at a time; mutations wait for
// it because both touch App state.
func (m *Model) startReload(refresh bool) tea.Cmd {
	if m.probeCancel != nil {
		m.probeCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.probeCancel = cancel
	m.loadingRows = true
	return reloadCmd(ctx, m.app, refresh)
}

func (m *Model) appendLog(line string) {
	line = sanitize(line, false)
	if line == "" {
		return
	}
	m.liveLog = append(m.liveLog, line)
	if len(m.liveLog) > 500 {
		m.liveLog = m.liveLog[len(m.liveLog)-500:]
	}
	if m.tab == tabLog {
		m.refreshLog()
	}
}

// refreshShellDiff caches the pending diff. It must only be called on the event
// loop goroutine while no command is mutating App state.
func (m *Model) refreshShellDiff() {
	m.shellDiff = ""
	m.shellDiffErr = ""
	m.shellOffset = 0
	name := m.currentShell()
	if name == "" {
		return
	}
	diff, err := m.app.ShellDiff(name)
	if err != nil {
		m.shellDiffErr = sanitize(err.Error(), false)
		return
	}
	m.shellDiff = sanitizeVisible(diff, true)
	m.wrapShellDiff()
}

func (m *Model) shellLeftWidth() int  { return minInt(34, m.width/3) }
func (m *Model) shellRightWidth() int { return maxInt(10, m.width-m.shellLeftWidth()-1) }

// wrapShellDiff precomputes display lines so scrolling and bounds match what is
// actually rendered. It only touches cached text, never App state.
func (m *Model) wrapShellDiff() {
	m.shellDiffWrapped = nil
	m.shellDiffTotal = 0
	if strings.TrimSpace(m.shellDiff) == "" {
		return
	}
	raw := strings.Split(strings.TrimRight(m.shellDiff, "\n"), "\n")
	m.shellDiffWrapped = wrapDiffLines(raw, m.shellRightWidth())
	m.shellDiffTotal = len(m.shellDiffWrapped)
	if m.shellOffset >= m.shellDiffTotal {
		m.shellOffset = maxInt(0, m.shellDiffTotal-1)
	}
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		m.shutdown()
		return m, tea.Quit
	}
	if m.bundleDraft != nil {
		return m.handleBundleWizardKey(msg)
	}
	if m.sshDraft != nil {
		return m.handleSSHWizardKey(msg)
	}
	if m.prompt != nil {
		return m.handlePromptKey(msg)
	}
	if m.help {
		if key == "?" || key == "esc" || key == "q" {
			m.help = false
		}
		return m, nil
	}
	if m.overlay == overlayShell {
		return m.handleShellKey(msg)
	}
	if m.overlay == overlayDetail {
		return m.handleDetailKey(msg)
	}
	if m.searching {
		switch key {
		case "esc":
			m.searching = false
			m.search.Blur()
			m.query = ""
			m.search.SetValue("")
			m.refilter()
			return m, nil
		case "enter":
			m.searching = false
			m.search.Blur()
			return m, nil
		default:
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			m.query = m.search.Value()
			m.refilter()
			return m, cmd
		}
	}

	switch key {
	case "q":
		m.shutdown()
		return m, tea.Quit
	case "?":
		m.help = true
		return m, nil
	case "tab":
		return m, m.setTab((m.tab + 1) % tab(len(tabNames)))
	case "shift+tab":
		return m, m.setTab((m.tab + tab(len(tabNames)) - 1) % tab(len(tabNames)))
	case "1":
		return m, m.setTab(tabPackages)
	case "2":
		return m, m.setTab(tabLists)
	case "3":
		return m, m.setTab(tabBundles)
	case "4":
		return m, m.setTab(tabSSH)
	case "5":
		return m, m.setTab(tabLog)
	case "esc":
		if m.running {
			if m.cancel != nil {
				m.cancel()
			}
			return m, m.setStatus("cancelling…", false)
		}
		if m.loadingRows && m.probeCancel != nil {
			m.probeCancel()
			return m, m.setStatus("cancelling refresh…", false)
		}
		if m.query != "" {
			m.query = ""
			m.search.SetValue("")
			m.refilter()
			return m, nil
		}
		if m.selectionCount() > 0 {
			m.selected = map[string]bool{}
			return m, nil
		}
		return m, nil
	}

	switch m.tab {
	case tabPackages:
		return m.handlePackagesKey(msg)
	case tabLists:
		return m.handleListsKey(msg)
	case tabBundles:
		return m.handleBundlesKey(msg)
	case tabSSH:
		return m.handleSSHKey(msg)
	case tabLog:
		return m.handleLogKey(msg)
	}
	return m, nil
}

func (m *Model) setTab(t tab) tea.Cmd {
	m.tab = t
	if t == tabLog {
		m.refreshLog()
		if !m.logReading {
			m.logReading = true
			return readLogCmd(m.app.Paths.Logs)
		}
	}
	return nil
}

func (m *Model) move(delta int) {
	if len(m.filtered) == 0 {
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	m.ensureCursorVisible(m.bodyHeight())
}

func (m *Model) handlePackagesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "/":
		m.searching = true
		m.search.Focus()
		return m, nil
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup", "ctrl+b":
		m.move(-m.bodyHeight())
	case "pgdown", "ctrl+f":
		m.move(m.bodyHeight())
	case "home", "g":
		m.cursor = 0
		m.ensureCursorVisible(m.bodyHeight())
	case "end", "G":
		if len(m.filtered) > 0 {
			m.cursor = len(m.filtered) - 1
		}
		m.ensureCursorVisible(m.bodyHeight())
	case "enter":
		if m.currentRow() != nil {
			m.overlay = overlayDetail
			m.detailOffset = 0
			m.detailTotal = len(m.detailLines(maxInt(20, m.width)))
		} else {
			return m, m.setStatus("nothing selected", true)
		}
	case " ":
		if r := m.currentRow(); r != nil {
			if m.selected[r.Name] {
				delete(m.selected, r.Name)
			} else {
				m.selected[r.Name] = true
			}
			m.move(1)
		}
	case "a":
		updates := 0
		for i := range m.rows {
			if m.rows[i].Status == "update" && m.selected[m.rows[i].Name] {
				updates++
			}
		}
		all := m.updateCount()
		if all > 0 && updates == all {
			for i := range m.rows {
				if m.rows[i].Status == "update" {
					delete(m.selected, m.rows[i].Name)
				}
			}
		} else {
			for i := range m.rows {
				if m.rows[i].Status == "update" {
					m.selected[m.rows[i].Name] = true
				}
			}
		}
	case "i":
		names := m.selectedNames()
		if len(names) == 0 {
			if r := m.currentRow(); r != nil {
				names = []string{r.Name}
			}
		}
		return m, m.beginOperation("install", names)
	case "u":
		names := m.selectedNames()
		if len(names) == 0 {
			for i := range m.rows {
				if m.rows[i].Status == "update" {
					names = append(names, m.rows[i].Name)
				}
			}
		}
		if len(names) == 0 {
			return m, m.setStatus("everything is up to date", false)
		}
		return m, m.beginOperation("update", names)
	case "d":
		names := m.removeTargets()
		if len(names) == 0 {
			return m, m.setStatus("no installed package selected", true)
		}
		return m, m.openPrompt(promptRemovePackages, "remove "+strings.Join(names, " ")+"?", "", "", names, "")
	case "X":
		names := m.removeTargets()
		if len(names) == 0 {
			return m, m.setStatus("no installed package selected", true)
		}
		return m, m.openPrompt(promptForceRemove, "force remove "+strings.Join(names, " ")+" (modified files will be deleted)?", "", "", names, "")
	case "p":
		r := m.currentRow()
		if r == nil || r.Version == "" {
			return m, m.setStatus("the selected package is not installed", true)
		}
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		m.clearStatus()
		m.loadingRows = true
		if r.Pinned {
			return m, pinCmd(m.ctx, m.app, r.Name, "", true)
		}
		return m, pinCmd(m.ctx, m.app, r.Name, r.Version, false)
	case "r":
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		m.clearStatus()
		return m, m.startReload(false)
	case "x":
		m.selected = map[string]bool{}
	}
	return m, nil
}

func (m *Model) removeTargets() []string {
	var names []string
	for i := range m.rows {
		r := &m.rows[i]
		if r.Version != "" && m.selected[r.Name] {
			names = append(names, r.Name)
		}
	}
	if len(names) > 0 {
		return names
	}
	if r := m.currentRow(); r != nil && r.Version != "" {
		return []string{r.Name}
	}
	return nil
}

func (m *Model) handleListsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.listCursor > 0 {
			m.listCursor--
		}
		m.ensureListVisible(m.listTableHeight())
	case "down", "j":
		if m.listCursor < len(m.lists)-1 {
			m.listCursor++
		}
		m.ensureListVisible(m.listTableHeight())
	case "home", "g":
		m.listCursor = 0
		m.ensureListVisible(m.listTableHeight())
	case "end", "G":
		if len(m.lists) > 0 {
			m.listCursor = len(m.lists) - 1
		}
		m.ensureListVisible(m.listTableHeight())
	case "n":
		if m.app.Offline {
			return m, m.setStatus("cannot add a list in offline mode", true)
		}
		return m, m.openPrompt(promptAddList, "add list", "URL or owner/repo[#ref[:path]]", "", nil, "")
	case "r":
		if li := m.currentList(); li != nil {
			if li.Readonly {
				return m, m.setStatus("list "+li.Name+" is read-only", true)
			}
			if m.busy() {
				return m, m.setStatus("an operation is already running", true)
			}
			m.clearStatus()
			m.loadingRows = true
			return m, refreshListsCmd(m.ctx, m.app, []string{li.Name}, true)
		}
	case "R":
		if m.app.Offline {
			return m, m.setStatus("cannot refresh lists in offline mode", true)
		}
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		m.clearStatus()
		m.loadingRows = true
		return m, refreshListsCmd(m.ctx, m.app, nil, true)
	case "x":
		li := m.currentList()
		if li == nil {
			return m, nil
		}
		if li.Builtin {
			return m, m.setStatus("the builtin list cannot be removed", true)
		}
		if li.Readonly {
			return m, m.setStatus("list "+li.Name+" is read-only", true)
		}
		return m, m.openPrompt(promptRemoveList, "remove list "+li.Name+"?", "", "", nil, li.Name)
	case "s":
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		if m.pendingShells == 0 {
			return m, m.setStatus("no shell snippet changes are pending", false)
		}
		m.shellPending = m.pendingShellNames()
		m.shellCursor = 0
		m.shellListOffset = 0
		m.overlay = overlayShell
		m.refreshShellDiff()
	}
	return m, nil
}

func (m *Model) handleBundlesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.bundleCursor > 0 {
			m.bundleCursor--
		}
		m.ensureBundleVisible(m.bundleTableHeight())
	case "down", "j":
		if m.bundleCursor < len(m.bundles)-1 {
			m.bundleCursor++
		}
		m.ensureBundleVisible(m.bundleTableHeight())
	case "home", "g":
		m.bundleCursor = 0
		m.ensureBundleVisible(m.bundleTableHeight())
	case "end", "G":
		if len(m.bundles) > 0 {
			m.bundleCursor = len(m.bundles) - 1
		}
		m.ensureBundleVisible(m.bundleTableHeight())
	case "n":
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		if m.app.Offline {
			return m, m.setStatus("cannot build a bundle in offline mode", true)
		}
		m.bundleDraft = newBundleDraft(m.width)
		m.clearStatus()
		return m, nil
	case "r":
		b := m.currentBundle()
		if b == nil {
			return m, m.setStatus("no bundle selected", true)
		}
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		if m.app.Offline {
			return m, m.setStatus("cannot rebuild a bundle in offline mode", true)
		}
		m.clearStatus()
		ctx, cancel := context.WithCancel(m.ctx)
		m.cancel = cancel
		m.running = true
		m.opName = "rebuild " + b.Name
		return m, bundleRebuildCmd(ctx, m.app, b.Name)
	case "e":
		b := m.currentBundle()
		if b == nil {
			return m, m.setStatus("no bundle selected", true)
		}
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		if m.app.Offline {
			return m, m.setStatus("cannot edit a bundle in offline mode", true)
		}
		m.bundleDraft = m.newBundleEditDraft(b)
		m.clearStatus()
		return m, nil
	case "d", "x":
		b := m.currentBundle()
		if b == nil {
			return m, m.setStatus("no bundle selected", true)
		}
		return m, m.openPrompt(promptRemoveBundle, "remove bundle "+b.Name+"?", "", "", nil, b.Name)
	case "enter":
		if b := m.currentBundle(); b != nil {
			return m, m.setStatus(b.Path, false)
		}
	}
	return m, nil
}

func (m *Model) handleBundleWizardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	d := m.bundleDraft
	if d == nil {
		return m, nil
	}
	key := msg.String()
	if key == "esc" {
		if d.step == bundleStepPackages && (d.searching || d.query != "") {
			d.searching = false
			d.search.Blur()
			d.query = ""
			d.search.SetValue("")
			d.packageCursor = 0
			d.packageOffset = 0
			return m, nil
		}
		m.bundleDraft = nil
		m.clearStatus()
		return m, nil
	}

	if d.step == bundleStepPackages && d.searching {
		switch key {
		case "enter":
			d.searching = false
			d.search.Blur()
			return m, nil
		default:
			var cmd tea.Cmd
			d.search, cmd = d.search.Update(msg)
			d.query = d.search.Value()
			d.packageCursor = 0
			d.packageOffset = 0
			m.ensureBundlePackageVisible(d, m.bundlePackageListHeight())
			return m, cmd
		}
	}

	switch d.step {
	case bundleStepName:
		if key == "enter" {
			name := strings.TrimSpace(d.nameInput.Value())
			switch {
			case name == "":
				d.err = "a bundle name is required"
				return m, nil
			case !bundle.ValidName(name):
				d.err = "use a short name with letters, numbers, '.', '_' or '-'"
				return m, nil
			}
			d.name = name
			d.err = ""
			d.step = bundleStepTarget
			d.nameInput.Blur()
			return m, nil
		}
		d.err = ""
		var cmd tea.Cmd
		d.nameInput, cmd = d.nameInput.Update(msg)
		return m, cmd

	case bundleStepTarget:
		targets := bundleTargets()
		switch key {
		case "up", "k":
			if d.targetCursor > 0 {
				d.targetCursor--
			}
		case "down", "j":
			if d.targetCursor < len(targets)-1 {
				d.targetCursor++
			}
		case "home", "g":
			d.targetCursor = 0
		case "end", "G":
			d.targetCursor = len(targets) - 1
		case "shift+tab", "left", "h":
			d.step = bundleStepName
			d.nameInput.Focus()
			return m, nil
		case "enter":
			d.target = targets[d.targetCursor].String()
			m.prepareBundlePackages(d)
			d.step = bundleStepPackages
			d.err = ""
			return m, nil
		}
		if d.targetCursor >= 0 && d.targetCursor < len(targets) {
			d.target = targets[d.targetCursor].String()
		}
		return m, nil

	case bundleStepPackages:
		switch key {
		case "/":
			d.searching = true
			d.search.Width = maxInt(10, m.width-14)
			d.search.Focus()
			return m, nil
		case "up", "k":
			m.moveBundlePackage(d, -1)
		case "down", "j":
			m.moveBundlePackage(d, 1)
		case "pgup", "ctrl+b":
			m.moveBundlePackage(d, -m.bundlePackageListHeight())
		case "pgdown", "ctrl+f":
			m.moveBundlePackage(d, m.bundlePackageListHeight())
		case "home", "g":
			d.packageCursor = 0
			m.ensureBundlePackageVisible(d, m.bundlePackageListHeight())
		case "end", "G":
			if visible := m.bundleVisibleCandidates(d); len(visible) > 0 {
				d.packageCursor = len(visible) - 1
			}
			m.ensureBundlePackageVisible(d, m.bundlePackageListHeight())
		case " ":
			visible := m.bundleVisibleCandidates(d)
			if len(visible) > 0 {
				if d.packageCursor >= len(visible) {
					d.packageCursor = len(visible) - 1
				}
				name := d.candidates[visible[d.packageCursor]].Name
				d.selected[name] = !d.selected[name]
				m.moveBundlePackage(d, 1)
			}
		case "a":
			visible := m.bundleVisibleCandidates(d)
			all := len(visible) > 0
			for _, idx := range visible {
				if !d.selected[d.candidates[idx].Name] {
					all = false
					break
				}
			}
			for _, idx := range visible {
				name := d.candidates[idx].Name
				if all {
					delete(d.selected, name)
				} else {
					d.selected[name] = true
				}
			}
		case "shift+tab", "left", "h":
			d.step = bundleStepTarget
			d.err = ""
			return m, nil
		case "enter":
			if len(d.selectedPackages()) == 0 {
				d.err = "select at least one package for the bundle"
				return m, nil
			}
			d.step = bundleStepMode
			d.err = ""
			if d.frozen {
				d.modeCursor = 1
			} else {
				d.modeCursor = 0
			}
			return m, nil
		}
		return m, nil

	case bundleStepMode:
		switch key {
		case "up", "k", "left", "h":
			d.modeCursor = 0
		case "down", "j", "right", "l":
			d.modeCursor = 1
		case " ":
			if d.modeCursor == 0 {
				d.modeCursor = 1
			} else {
				d.modeCursor = 0
			}
		case "shift+tab":
			d.step = bundleStepPackages
			d.err = ""
			return m, nil
		case "enter":
			d.frozen = d.modeCursor == 1
			if m.app.Offline {
				d.err = "cannot build a bundle in offline mode"
				return m, nil
			}
			m.clearStatus()
			return m, m.startBundleBuild()
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) bundlePackageListHeight() int {
	return maxInt(1, m.bodyHeight()-9)
}

func (m *Model) bundleVisibleCandidates(d *bundleDraft) []int {
	query := strings.ToLower(strings.TrimSpace(d.query))
	out := make([]int, 0, len(d.candidates))
	for i := range d.candidates {
		c := &d.candidates[i]
		if query == "" || fuzzyContains(c.Name, query) || fuzzyContains(c.Description, query) {
			out = append(out, i)
		}
	}
	return out
}

func (m *Model) ensureBundlePackageVisible(d *bundleDraft, height int) {
	if height < 1 {
		height = 1
	}
	visible := m.bundleVisibleCandidates(d)
	if len(visible) == 0 {
		d.packageCursor = 0
		d.packageOffset = 0
		return
	}
	if d.packageCursor < 0 {
		d.packageCursor = 0
	}
	if d.packageCursor >= len(visible) {
		d.packageCursor = len(visible) - 1
	}
	if d.packageCursor < d.packageOffset {
		d.packageOffset = d.packageCursor
	}
	if d.packageCursor >= d.packageOffset+height {
		d.packageOffset = d.packageCursor - height + 1
	}
	maxOffset := len(visible) - height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if d.packageOffset > maxOffset {
		d.packageOffset = maxOffset
	}
	if d.packageOffset < 0 {
		d.packageOffset = 0
	}
}

func (m *Model) moveBundlePackage(d *bundleDraft, delta int) {
	visible := m.bundleVisibleCandidates(d)
	if len(visible) == 0 {
		d.packageCursor = 0
		d.packageOffset = 0
		return
	}
	d.packageCursor += delta
	if d.packageCursor < 0 {
		d.packageCursor = 0
	}
	if d.packageCursor >= len(visible) {
		d.packageCursor = len(visible) - 1
	}
	m.ensureBundlePackageVisible(d, m.bundlePackageListHeight())
}

func (m *Model) prepareBundlePackages(d *bundleDraft) {
	if d.candidatesTarget == d.target && len(d.candidates) > 0 {
		return
	}
	target, err := platform.Parse(d.target)
	if err != nil {
		return
	}
	d.candidatesTarget = d.target
	d.candidates = m.bundleCandidates(target)
	d.selected = map[string]bool{}
	for _, name := range m.installedForTarget(target) {
		d.selected[name] = true
	}
	d.packageCursor = 0
	d.packageOffset = 0
	d.query = ""
	d.searching = false
	d.search.Blur()
	d.search.SetValue("")
}

// bundleCandidates lists every package that can be built for target. Installed
// packages start selected so the common case (bundle what is already here) is
// one keystroke away.
func (m *Model) bundleCandidates(target platform.Platform) []bundleCandidate {
	rowByName := make(map[string]app.Row, len(m.rows))
	for _, row := range m.rows {
		rowByName[row.Name] = row
	}
	out := []bundleCandidate{}
	cat, err := m.app.Catalog()
	if err != nil {
		for _, row := range m.rows {
			out = append(out, bundleCandidate{
				Name:        row.Name,
				Description: row.Description,
				Installed:   row.Version != "",
				Conflict:    row.Conflict,
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	conflicts := cat.Conflicts()
	seen := map[string]bool{}
	for _, entry := range cat.Entries() {
		name := entry.Pkg.Name
		if seen[name] {
			continue
		}
		seen[name] = true
		if entry.Pkg.Source.Type != "url" && entry.Pkg.Source.Asset[target.String()] == "" {
			continue
		}
		row, ok := rowByName[name]
		description := entry.Pkg.Description
		if description == "" && ok {
			description = row.Description
		}
		out = append(out, bundleCandidate{
			Name:        name,
			Description: description,
			Installed:   ok && row.Version != "",
			Conflict:    len(conflicts[name]) > 1,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// newBundleEditDraft opens the same wizard as a new bundle, but with the
// existing bundle's name, target, package set and mode already filled in.
func (m *Model) newBundleEditDraft(b *app.BundleInfo) *bundleDraft {
	d := newBundleDraft(m.width)
	d.name = b.Name
	d.nameInput.SetValue(b.Name)
	d.target = b.Target
	for i, p := range bundleTargets() {
		if p.String() == d.target {
			d.targetCursor = i
			break
		}
	}
	d.frozen = b.Frozen
	if b.Frozen {
		d.modeCursor = 1
	}
	m.prepareBundlePackages(d)
	if len(b.PackageNames) > 0 {
		available := make(map[string]bool, len(d.candidates))
		for _, c := range d.candidates {
			available[c.Name] = true
		}
		d.selected = map[string]bool{}
		for _, name := range b.PackageNames {
			if available[name] {
				d.selected[name] = true
			}
		}
	}
	return d
}

// installedForTarget lists installed packages that declare support for target,
// so the wizard starts from a set that can actually be resolved.
func (m *Model) installedForTarget(target platform.Platform) []string {
	cat, err := m.app.Catalog()
	if err != nil {
		return nil
	}
	installed := map[string]bool{}
	for i := range m.rows {
		if m.rows[i].Version != "" {
			installed[m.rows[i].Name] = true
		}
	}
	var names []string
	for _, entry := range cat.Entries() {
		if !installed[entry.Pkg.Name] {
			continue
		}
		if entry.Pkg.Source.Type == "url" || entry.Pkg.Source.Asset[target.String()] != "" {
			names = append(names, entry.Pkg.Name)
		}
	}
	sort.Strings(names)
	return names
}

func (m *Model) startBundleBuild() tea.Cmd {
	draft := m.bundleDraft
	if draft == nil {
		return m.setStatus("bundle wizard was cancelled", true)
	}
	packages := draft.selectedPackages()
	if len(packages) == 0 {
		return m.setStatus("select at least one package for the bundle", true)
	}
	m.bundleDraft = nil
	opt := app.BundleOptions{
		Name:     draft.name,
		Target:   draft.target,
		Packages: packages,
		Frozen:   draft.frozen,
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	m.running = true
	m.opName = "bundle " + draft.name
	return bundleCreateCmd(ctx, m.app, opt)
}

func (m *Model) handleShellKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.busy() && key != "esc" && key != "q" {
		return m, m.setStatus("an operation is already running", true)
	}
	switch key {
	case "esc", "q":
		m.overlay = overlayNone
	case "up", "k":
		if m.shellCursor > 0 {
			m.shellCursor--
			m.ensureShellVisible(m.shellListHeight())
			if !m.busy() {
				m.refreshShellDiff()
			}
		}
	case "down", "j":
		if m.shellCursor < len(m.shellPending)-1 {
			m.shellCursor++
			m.ensureShellVisible(m.shellListHeight())
			if !m.busy() {
				m.refreshShellDiff()
			}
		}
	case "pgup":
		m.shellOffset -= 10
		if m.shellOffset < 0 {
			m.shellOffset = 0
		}
	case "pgdown":
		m.shellOffset = minInt(m.shellOffset+10, maxInt(0, m.shellDiffTotal-1))
	case "A":
		name := m.currentShell()
		if name == "" {
			return m, nil
		}
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		m.clearStatus()
		m.loadingRows = true
		return m, shellDecisionCmd(m.ctx, m.app, []string{name}, false, true)
	case "R":
		name := m.currentShell()
		if name == "" {
			return m, nil
		}
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		m.clearStatus()
		m.loadingRows = true
		return m, shellDecisionCmd(m.ctx, m.app, []string{name}, false, false)
	case "a":
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		m.clearStatus()
		m.loadingRows = true
		return m, shellDecisionCmd(m.ctx, m.app, nil, true, true)
	case "x":
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		m.clearStatus()
		m.loadingRows = true
		return m, shellDecisionCmd(m.ctx, m.app, nil, true, false)
	}
	return m, nil
}

func (m *Model) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.detailTotal = len(m.detailLines(maxInt(20, m.width)))
	last := maxInt(0, m.detailTotal-1)
	switch msg.String() {
	case "esc", "enter", "q":
		m.overlay = overlayNone
	case "up", "k":
		if m.detailOffset > 0 {
			m.detailOffset--
		}
	case "down", "j":
		m.detailOffset = minInt(m.detailOffset+1, last)
	case "pgup":
		m.detailOffset -= 10
		if m.detailOffset < 0 {
			m.detailOffset = 0
		}
	case "pgdown":
		m.detailOffset = minInt(m.detailOffset+10, last)
	case "home", "g":
		m.detailOffset = 0
	case "end", "G":
		m.detailOffset = last
	}
	return m, nil
}

func (m *Model) handleLogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "g":
		m.log.GotoTop()
		return m, nil
	case "G":
		m.log.GotoBottom()
		return m, nil
	case "home":
		m.log.GotoTop()
		return m, nil
	case "end":
		m.log.GotoBottom()
		return m, nil
	}
	var cmd tea.Cmd
	m.log, cmd = m.log.Update(msg)
	return m, cmd
}

func (m *Model) openPrompt(kind promptKind, title, placeholder, value string, names []string, list string) tea.Cmd {
	if m.busy() {
		return m.setStatus("an operation is already running", true)
	}
	input := textinput.New()
	input.Placeholder = placeholder
	input.CharLimit = 4096
	input.Width = maxInt(10, m.width-20)
	input.Prompt = "> "
	input.SetValue(value)
	input.Focus()
	m.prompt = &promptState{kind: kind, title: title, input: input, names: names, list: list}
	return nil
}

func (m *Model) handlePromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		kind := m.prompt.kind
		m.prompt = nil
		if kind >= promptBundleName && kind <= promptBundleFrozen {
			m.bundleDraft = nil
		}
		return m, nil
	case "enter":
		p := m.prompt
		value := strings.TrimSpace(p.input.Value())
		m.prompt = nil
		switch p.kind {
		case promptAddList:
			if value == "" {
				return m, m.setStatus("a list URL or repository is required", true)
			}
			m.clearStatus()
			m.loadingRows = true
			return m, addListCmd(m.ctx, m.app, value)
		case promptRemoveList:
			m.clearStatus()
			m.loadingRows = true
			return m, removeListCmd(m.ctx, m.app, p.list)
		case promptRemovePackages:
			return m, m.beginOperation("remove", p.names)
		case promptForceRemove:
			return m, m.beginOperationForce("remove", p.names, true)
		case promptRemoveBundle:
			m.clearStatus()
			m.loadingRows = true
			return m, bundleRemoveCmd(m.ctx, m.app, p.list)
		case promptRemoveSSH:
			m.clearStatus()
			m.loadingRows = true
			return m, sshDeleteCmd(m.ctx, m.app, p.list)
		}
		return m, nil
	default:
		var cmd tea.Cmd
		m.prompt.input, cmd = m.prompt.input.Update(msg)
		return m, cmd
	}
}

// beginOperation starts an install, update or remove run. Interactive installs
// approve a package's first shell snippet, matching the explicit CLI command.
func (m *Model) beginOperation(op string, names []string) tea.Cmd {
	return m.beginOperationForce(op, names, false)
}

func (m *Model) beginOperationForce(op string, names []string, force bool) tea.Cmd {
	if m.busy() {
		return m.setStatus("an operation is already running", true)
	}
	if len(names) == 0 {
		return m.setStatus("nothing selected", true)
	}
	m.clearStatus()
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	m.running = true
	m.opName = op
	return operationCmd(ctx, m.app, op, names, force)
}

func splitPackageList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func summarizeResults(op string, results []app.Result, err error) (string, bool) {
	if err != nil && len(results) == 0 {
		return err.Error(), true
	}
	counts := map[string]int{}
	firstErr := ""
	for _, r := range results {
		action := r.Action
		if action == "" {
			action = "done"
		}
		counts[action]++
		if r.Err != nil && firstErr == "" {
			firstErr = r.Name + ": " + r.Err.Error()
		}
	}
	var parts []string
	for _, key := range []string{"installed", "updated", "reinstalled", "removed", "up-to-date", "skipped (pinned)", "skipped (frozen)", "failed"} {
		if counts[key] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[key], key))
		}
	}
	if len(parts) == 0 {
		if err != nil {
			return err.Error(), true
		}
		return op + ": no changes", false
	}
	text := op + ": " + strings.Join(parts, ", ")
	if firstErr != "" {
		return text + " — " + firstErr, true
	}
	if err != nil {
		return text + " — " + err.Error(), true
	}
	return text, false
}
