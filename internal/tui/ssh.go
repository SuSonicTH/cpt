package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/bundle"
	"github.com/SuSonicTH/cpt/internal/sshtarget"
)

type sshStep int

const (
	sshStepName sshStep = iota
	sshStepSource
	sshStepDestination
	sshStepBundle
	sshStepOptions
)

var sshShells = []string{sshtarget.ShellAuto, sshtarget.ShellBash, sshtarget.ShellZsh, sshtarget.ShellSh}

type sshDraft struct {
	edit         bool
	originalName string
	step         sshStep

	name      string
	nameInput textinput.Model

	configHost string
	custom     bool
	hosts      []sshtarget.ConfigHost

	sourceCursor    int
	sourceQuery     string
	sourceSearching bool
	sourceSearch    textinput.Model

	hostInput     textinput.Model
	userInput     textinput.Model
	portInput     textinput.Model
	identityInput textinput.Model
	proxyInput    textinput.Model
	destField     int

	bundles         []app.BundleInfo
	bundleCursor    int
	bundleQuery     string
	bundleSearching bool
	bundleSearch    textinput.Model

	shellCursor   int
	cleanup       bool
	force         bool
	remoteRoot    textinput.Model
	optionsCursor int
	editingRoot   bool

	// warnedName records the name that triggered the overwrite confirmation, so
	// pressing enter a second time is what overwrites an existing target.
	warnedName string
	// originalBundle keeps an existing target's bundle selectable in edit mode
	// even when it can no longer run over SSH, so saving shows a clear error
	// instead of silently switching to another bundle.
	originalBundle string

	err string
}

func newSSHInput(placeholder string, width int) textinput.Model {
	in := textinput.New()
	in.Placeholder = placeholder
	in.CharLimit = 4096
	in.Width = maxInt(10, width-10)
	in.Prompt = "> "
	return in
}

func newSSHDraft(width int, bundles []app.BundleInfo, hosts []sshtarget.ConfigHost) *sshDraft {
	d := &sshDraft{
		step:          sshStepName,
		nameInput:     newSSHInput("staging", width),
		sourceSearch:  newSSHInput("filter SSH hosts", width),
		hostInput:     newSSHInput("hostname or IP", width),
		userInput:     newSSHInput("optional user", width),
		portInput:     newSSHInput("optional port", width),
		identityInput: newSSHInput("optional identity file", width),
		proxyInput:    newSSHInput("optional ProxyJump", width),
		bundleSearch:  newSSHInput("filter bundles", width),
		remoteRoot:    newSSHInput("default ~/.cpt", width),
		bundles:       append([]app.BundleInfo(nil), bundles...),
		hosts:         append([]sshtarget.ConfigHost(nil), hosts...),
	}
	d.nameInput.CharLimit = 128
	d.nameInput.Focus()
	d.sourceSearch.Prompt = ""
	d.bundleSearch.Prompt = ""
	return d
}

func newSSHEditDraft(t *app.SSHTargetInfo, width int, bundles []app.BundleInfo, hosts []sshtarget.ConfigHost) *sshDraft {
	d := newSSHDraft(width, bundles, hosts)
	d.edit = true
	d.originalName = t.Name
	d.originalBundle = t.Bundle
	d.name = t.Name
	d.nameInput.SetValue(t.Name)
	d.nameInput.Blur()
	d.configHost = ""
	d.custom = t.SSH.Source == sshtarget.SourceCustom
	if !d.custom {
		d.configHost = t.SSH.Host
	}
	d.hostInput.SetValue(t.SSH.Host)
	d.userInput.SetValue(t.SSH.User)
	if t.SSH.Port > 0 {
		d.portInput.SetValue(strconv.Itoa(t.SSH.Port))
	}
	d.identityInput.SetValue(t.SSH.IdentityFile)
	d.proxyInput.SetValue(t.SSH.ProxyJump)
	d.remoteRoot.SetValue(t.RemoteRoot)
	d.cleanup = t.CleanupOnExit
	d.force = t.Force
	for i, shell := range sshShells {
		if shell == t.EffectiveShell() {
			d.shellCursor = i
			break
		}
	}
	if !d.custom {
		found := -1
		for i, host := range d.hosts {
			if host.Alias == t.SSH.Host {
				found = i
				break
			}
		}
		if found < 0 && strings.TrimSpace(t.SSH.Host) != "" {
			d.hosts = append(d.hosts, sshtarget.ConfigHost{Alias: t.SSH.Host})
			found = len(d.hosts) - 1
		}
		if found >= 0 {
			d.sourceCursor = found
		}
	} else {
		d.sourceCursor = len(d.hosts)
	}
	foundBundle := false
	// The cursor indexes the filtered view, not d.bundles, so an ineligible
	// bundle before the target's bundle cannot shift the selection.
	if pos := d.visibleIndex(t.Bundle); pos >= 0 {
		d.bundleCursor = pos
		foundBundle = true
	}
	if !foundBundle {
		// Keep the current reference visible even when its executable is
		// missing, so editing cannot silently switch to an unrelated bundle.
		d.bundles = append([]app.BundleInfo{{
			Name:   t.Bundle,
			Target: t.BundleTarget,
		}}, d.bundles...)
		d.bundleCursor = 0
	}
	d.step = sshStepSource
	return d
}

func (d *sshDraft) selectedShell() string {
	if d.shellCursor < 0 || d.shellCursor >= len(sshShells) {
		return sshtarget.ShellAuto
	}
	return sshShells[d.shellCursor]
}

func (d *sshDraft) sourceOptions() []sshSourceOption {
	query := strings.ToLower(strings.TrimSpace(d.sourceQuery))
	var out []sshSourceOption
	for _, host := range d.hostsForDraft() {
		if query == "" || fuzzyContains(host.Alias, query) {
			out = append(out, sshSourceOption{Alias: host.Alias})
		}
	}
	if query == "" || fuzzyContains("custom server", query) {
		out = append(out, sshSourceOption{Custom: true})
	}
	return out
}

// hostsForDraft is filled from the model before the draft is used. Keeping the
// list on the draft makes cursor rendering independent from the snapshot.
func (d *sshDraft) hostsForDraft() []sshtarget.ConfigHost {
	return d.hosts
}

type sshSourceOption struct {
	Alias  string
	Custom bool
}

func (d *sshDraft) destinationFields() ([]string, []*textinput.Model) {
	var labels []string
	var inputs []*textinput.Model
	if d.custom {
		labels = append(labels, "Host")
		inputs = append(inputs, &d.hostInput)
	}
	labels = append(labels, "User", "Port", "Identity", "ProxyJump")
	inputs = append(inputs, &d.userInput, &d.portInput, &d.identityInput, &d.proxyInput)
	return labels, inputs
}

func (d *sshDraft) blurDestination() {
	for _, in := range []*textinput.Model{&d.hostInput, &d.userInput, &d.portInput, &d.identityInput, &d.proxyInput} {
		in.Blur()
	}
}

func (d *sshDraft) focusDestination() {
	d.blurDestination()
	_, inputs := d.destinationFields()
	if len(inputs) == 0 {
		return
	}
	if d.destField < 0 {
		d.destField = 0
	}
	if d.destField >= len(inputs) {
		d.destField = len(inputs) - 1
	}
	inputs[d.destField].Focus()
}

func (d *sshDraft) visibleBundles() []int {
	query := strings.ToLower(strings.TrimSpace(d.bundleQuery))
	out := make([]int, 0, len(d.bundles))
	for i := range d.bundles {
		b := &d.bundles[i]
		// Hide bundles that can never be saved, except an existing target's own
		// bundle, which stays visible so editing cannot switch it silently.
		if !b.SSHEligible && b.Name != d.originalBundle {
			continue
		}
		if query == "" || fuzzyContains(b.Name, query) || fuzzyContains(b.Target, query) {
			out = append(out, i)
		}
	}
	return out
}

// visibleIndex returns the position of name inside the filtered bundle list.
func (d *sshDraft) visibleIndex(name string) int {
	for i, idx := range d.visibleBundles() {
		if d.bundles[idx].Name == name {
			return i
		}
	}
	return -1
}

// bundleSSHProblem explains why a bundle cannot be used by an SSH target.
func bundleSSHProblem(b app.BundleInfo) string {
	switch {
	case !b.Executable:
		return "its executable is missing or invalid; rebuild it"
	case !b.SSHEligible:
		return "it targets " + b.Target + "; SSH targets need a POSIX bundle"
	}
	return ""
}

func (m *Model) hasSSHTarget(name string) bool {
	for i := range m.sshTargets {
		if m.sshTargets[i].Name == name {
			return true
		}
	}
	return false
}

func (d *sshDraft) selectedBundle() string {
	visible := d.visibleBundles()
	if d.bundleCursor < 0 || d.bundleCursor >= len(visible) {
		return ""
	}
	return d.bundles[visible[d.bundleCursor]].Name
}

func (d *sshDraft) buildTarget() (*sshtarget.Target, error) {
	name := strings.TrimSpace(d.name)
	if !bundle.ValidName(name) {
		return nil, fmt.Errorf("use a short target name with letters, numbers, '.', '_' or '-'")
	}
	bundleName := strings.TrimSpace(d.selectedBundle())
	if !bundle.ValidName(bundleName) {
		return nil, fmt.Errorf("select a bundle for this target")
	}
	t := &sshtarget.Target{
		Name:          name,
		Bundle:        bundleName,
		RemoteRoot:    strings.TrimSpace(d.remoteRoot.Value()),
		Shell:         d.selectedShell(),
		CleanupOnExit: d.cleanup,
		Force:         d.force,
	}
	if d.custom {
		t.SSH = sshtarget.SSH{Source: sshtarget.SourceCustom, Host: strings.TrimSpace(d.hostInput.Value())}
	} else {
		if strings.TrimSpace(d.configHost) == "" {
			return nil, fmt.Errorf("select an SSH config host")
		}
		t.SSH = sshtarget.SSH{Source: sshtarget.SourceConfig, Host: strings.TrimSpace(d.configHost)}
	}
	t.SSH.User = strings.TrimSpace(d.userInput.Value())
	if v := strings.TrimSpace(d.portInput.Value()); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("port must be a number between 1 and 65535")
		}
		t.SSH.Port = port
	}
	t.SSH.IdentityFile = strings.TrimSpace(d.identityInput.Value())
	t.SSH.ProxyJump = strings.TrimSpace(d.proxyInput.Value())
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return t, nil
}

// --- model integration -----------------------------------------------------

func (m *Model) currentSSHTarget() *app.SSHTargetInfo {
	if m.sshCursor < 0 || m.sshCursor >= len(m.sshTargets) {
		return nil
	}
	return &m.sshTargets[m.sshCursor]
}

func (m *Model) resizeSSHDraft() {
	d := m.sshDraft
	if d == nil {
		return
	}
	width := maxInt(10, m.width-10)
	for _, in := range []*textinput.Model{
		&d.nameInput, &d.sourceSearch, &d.hostInput, &d.userInput, &d.portInput,
		&d.identityInput, &d.proxyInput, &d.bundleSearch, &d.remoteRoot,
	} {
		in.Width = width
	}
}

func (m *Model) sshTableHeight() int { return maxInt(3, m.bodyHeight()-1) }

func (m *Model) ensureSSHVisible(height int) {
	if height < 1 {
		height = 1
	}
	if m.sshCursor < m.sshOffset {
		m.sshOffset = m.sshCursor
	}
	if m.sshCursor >= m.sshOffset+height {
		m.sshOffset = m.sshCursor - height + 1
	}
	maxOffset := len(m.sshTargets) - height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.sshOffset > maxOffset {
		m.sshOffset = maxOffset
	}
	if m.sshOffset < 0 {
		m.sshOffset = 0
	}
}

func (m *Model) handleSSHKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.sshCursor > 0 {
			m.sshCursor--
		}
		m.ensureSSHVisible(m.sshTableHeight())
	case "down", "j":
		if m.sshCursor < len(m.sshTargets)-1 {
			m.sshCursor++
		}
		m.ensureSSHVisible(m.sshTableHeight())
	case "home", "g":
		m.sshCursor = 0
		m.ensureSSHVisible(m.sshTableHeight())
	case "end", "G":
		if len(m.sshTargets) > 0 {
			m.sshCursor = len(m.sshTargets) - 1
		}
		m.ensureSSHVisible(m.sshTableHeight())
	case "n":
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		if len(m.bundles) == 0 {
			return m, m.setStatus("create a bundle before adding an SSH target", true)
		}
		m.sshDraft = newSSHDraft(m.width, m.bundles, m.sshHosts)
		m.sshDraft.hosts = m.sshHosts
		m.clearStatus()
		return m, nil
	case "e":
		t := m.currentSSHTarget()
		if t == nil {
			return m, m.setStatus("no SSH target selected", true)
		}
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		m.sshDraft = newSSHEditDraft(t, m.width, m.bundles, m.sshHosts)
		m.sshDraft.hosts = m.sshHosts
		m.clearStatus()
		return m, nil
	case "d", "x":
		t := m.currentSSHTarget()
		if t == nil {
			return m, m.setStatus("no SSH target selected", true)
		}
		return m, m.openPrompt(promptRemoveSSH, "remove SSH target "+t.Name+"?", "", "", nil, t.Name)
	case "c", "enter":
		return m.connectSSH()
	case "r":
		if m.busy() {
			return m, m.setStatus("an operation is already running", true)
		}
		m.clearStatus()
		m.loadingRows = true
		return m, reloadTargetsCmd(m.ctx, m.app)
	}
	return m, nil
}

func (m *Model) connectSSH() (tea.Model, tea.Cmd) {
	t := m.currentSSHTarget()
	if t == nil {
		return m, m.setStatus("no SSH target selected", true)
	}
	if m.app.Offline {
		return m, m.setStatus("cannot connect in offline mode", true)
	}
	if !t.BundleExists {
		return m, m.setStatus("bundle executable is missing; rebuild it", true)
	}
	if !t.BundleValid {
		return m, m.setStatus("bundle executable is invalid; rebuild it", true)
	}
	if m.busy() {
		return m, m.setStatus("an operation is already running", true)
	}
	m.clearStatus()
	return m, sshConnectCmd(m.app, t.Name)
}

func (m *Model) handleSSHWizardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	d := m.sshDraft
	if d == nil {
		return m, nil
	}
	key := msg.String()
	if key == "esc" {
		switch {
		case d.step == sshStepSource && (d.sourceSearching || d.sourceQuery != ""):
			d.sourceSearching = false
			d.sourceSearch.Blur()
			d.sourceQuery = ""
			d.sourceSearch.SetValue("")
			d.sourceCursor = 0
			return m, nil
		case d.step == sshStepBundle && (d.bundleSearching || d.bundleQuery != ""):
			d.bundleSearching = false
			d.bundleSearch.Blur()
			d.bundleQuery = ""
			d.bundleSearch.SetValue("")
			d.bundleCursor = 0
			return m, nil
		case d.step == sshStepOptions && d.editingRoot:
			d.editingRoot = false
			d.remoteRoot.Blur()
			return m, nil
		}
		m.sshDraft = nil
		m.clearStatus()
		return m, nil
	}

	if d.step == sshStepSource && d.sourceSearching {
		if key == "enter" {
			d.sourceSearching = false
			d.sourceSearch.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		d.sourceSearch, cmd = d.sourceSearch.Update(msg)
		d.sourceQuery = d.sourceSearch.Value()
		d.sourceCursor = 0
		return m, cmd
	}
	if d.step == sshStepBundle && d.bundleSearching {
		if key == "enter" {
			d.bundleSearching = false
			d.bundleSearch.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		d.bundleSearch, cmd = d.bundleSearch.Update(msg)
		d.bundleQuery = d.bundleSearch.Value()
		d.bundleCursor = 0
		return m, cmd
	}
	if d.step == sshStepOptions && d.editingRoot {
		if key == "enter" {
			d.editingRoot = false
			d.remoteRoot.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		d.remoteRoot, cmd = d.remoteRoot.Update(msg)
		return m, cmd
	}

	switch d.step {
	case sshStepName:
		if key == "enter" {
			name := strings.TrimSpace(d.nameInput.Value())
			switch {
			case name == "":
				d.err = "a target name is required"
				return m, nil
			case !bundle.ValidName(name):
				d.err = "use a short name with letters, numbers, '.', '_' or '-'"
				return m, nil
			case d.edit && name != d.originalName:
				d.err = "target names cannot be changed; remove and re-add the target"
				return m, nil
			case sshtarget.IsReservedName(name):
				d.err = fmt.Sprintf("%q is reserved by a cpt ssh subcommand; choose another name", name)
				return m, nil
			case !d.edit && name != d.warnedName && m.hasSSHTarget(name):
				d.warnedName = name
				d.err = fmt.Sprintf("SSH target %q already exists; press enter again to overwrite it", name)
				return m, nil
			}
			d.name = name
			d.err = ""
			d.step = sshStepSource
			d.nameInput.Blur()
			return m, nil
		}
		d.err = ""
		// A warning only applies to the exact name it was shown for.
		d.warnedName = ""
		var cmd tea.Cmd
		d.nameInput, cmd = d.nameInput.Update(msg)
		return m, cmd

	case sshStepSource:
		options := d.sourceOptions()
		switch key {
		case "/":
			d.sourceSearching = true
			d.sourceSearch.Width = maxInt(10, m.width-14)
			d.sourceSearch.Focus()
			return m, nil
		case "up", "k":
			if d.sourceCursor > 0 {
				d.sourceCursor--
			}
		case "down", "j":
			if d.sourceCursor < len(options)-1 {
				d.sourceCursor++
			}
		case "home", "g":
			d.sourceCursor = 0
		case "end", "G":
			if len(options) > 0 {
				d.sourceCursor = len(options) - 1
			}
		case "shift+tab", "left", "h":
			d.step = sshStepName
			d.nameInput.Focus()
			d.err = ""
			return m, nil
		case "enter":
			if len(options) == 0 {
				return m, nil
			}
			if d.sourceCursor >= len(options) {
				d.sourceCursor = len(options) - 1
			}
			opt := options[d.sourceCursor]
			d.custom = opt.Custom
			if opt.Custom {
				d.configHost = ""
			} else {
				d.configHost = opt.Alias
				if d.hostInput.Value() == "" {
					d.hostInput.SetValue(opt.Alias)
				}
			}
			d.destField = 0
			d.step = sshStepDestination
			d.focusDestination()
			d.err = ""
			return m, nil
		}
		if d.sourceCursor >= len(options) {
			d.sourceCursor = maxInt(0, len(options)-1)
		}
		return m, nil

	case sshStepDestination:
		_, inputs := d.destinationFields()
		switch key {
		case "shift+tab":
			if d.destField > 0 {
				d.destField--
				d.focusDestination()
			} else {
				d.blurDestination()
				d.step = sshStepSource
				d.err = ""
			}
			return m, nil
		case "up":
			if d.destField > 0 {
				d.destField--
				d.focusDestination()
			}
			return m, nil
		case "down":
			if d.destField < len(inputs)-1 {
				d.destField++
				d.focusDestination()
			}
			return m, nil
		case "enter":
			if d.destField < len(inputs)-1 {
				d.destField++
				d.focusDestination()
				return m, nil
			}
			if d.custom && strings.TrimSpace(d.hostInput.Value()) == "" {
				d.err = "a hostname or IP is required"
				return m, nil
			}
			d.blurDestination()
			d.step = sshStepBundle
			d.err = ""
			return m, nil
		}
		if len(inputs) == 0 {
			return m, nil
		}
		if d.destField >= len(inputs) {
			d.destField = len(inputs) - 1
		}
		var cmd tea.Cmd
		*inputs[d.destField], cmd = inputs[d.destField].Update(msg)
		return m, cmd

	case sshStepBundle:
		visible := d.visibleBundles()
		switch key {
		case "/":
			d.bundleSearching = true
			d.bundleSearch.Width = maxInt(10, m.width-14)
			d.bundleSearch.Focus()
			return m, nil
		case "up", "k":
			if d.bundleCursor > 0 {
				d.bundleCursor--
			}
		case "down", "j":
			if d.bundleCursor < len(visible)-1 {
				d.bundleCursor++
			}
		case "home", "g":
			d.bundleCursor = 0
		case "end", "G":
			if len(visible) > 0 {
				d.bundleCursor = len(visible) - 1
			}
		case "shift+tab", "left", "h":
			d.step = sshStepDestination
			d.focusDestination()
			d.err = ""
			return m, nil
		case "enter":
			if len(visible) == 0 {
				d.err = "no bundle selected"
				return m, nil
			}
			if d.bundleCursor >= len(visible) {
				d.bundleCursor = len(visible) - 1
			}
			if b := d.bundles[visible[d.bundleCursor]]; !b.SSHEligible {
				d.err = fmt.Sprintf("bundle %s cannot run over SSH: %s", b.Name, bundleSSHProblem(b))
				return m, nil
			}
			d.step = sshStepOptions
			d.optionsCursor = 0
			d.err = ""
			return m, nil
		}
		if len(visible) > 0 && d.bundleCursor >= len(visible) {
			d.bundleCursor = len(visible) - 1
		}
		return m, nil

	case sshStepOptions:
		switch key {
		case "up", "k":
			if d.optionsCursor > 0 {
				d.optionsCursor--
			}
			return m, nil
		case "down", "j":
			if d.optionsCursor < 4 {
				d.optionsCursor++
			}
			return m, nil
		case "shift+tab":
			d.step = sshStepBundle
			d.err = ""
			return m, nil
		case "right", "l":
			if d.optionsCursor == 0 {
				d.shellCursor = (d.shellCursor + 1) % len(sshShells)
			}
			return m, nil
		case "left", "h":
			if d.optionsCursor == 0 {
				d.shellCursor = (d.shellCursor + len(sshShells) - 1) % len(sshShells)
			}
			return m, nil
		case " ":
			switch d.optionsCursor {
			case 0:
				d.shellCursor = (d.shellCursor + 1) % len(sshShells)
			case 2:
				d.cleanup = !d.cleanup
			case 3:
				d.force = !d.force
			}
			return m, nil
		case "enter":
			switch d.optionsCursor {
			case 0:
				d.shellCursor = (d.shellCursor + 1) % len(sshShells)
				return m, nil
			case 1:
				d.editingRoot = true
				d.remoteRoot.Focus()
				return m, nil
			case 2:
				d.cleanup = !d.cleanup
				return m, nil
			case 3:
				d.force = !d.force
				return m, nil
			case 4:
				t, err := d.buildTarget()
				if err != nil {
					d.err = err.Error()
					return m, nil
				}
				m.sshDraft = nil
				m.clearStatus()
				m.loadingRows = true
				return m, sshTargetSaveCmd(m.ctx, m.app, t, d)
			}
		}
		return m, nil
	}
	return m, nil
}

// --- rendering -------------------------------------------------------------

func (m *Model) viewSSH(width, height int) string {
	if width < 60 {
		return strings.Join(fitLines(m.sshListLines(width, height), width, height), "\n")
	}
	leftWidth := width * 58 / 100
	leftWidth = maxInt(50, leftWidth)
	leftWidth = minInt(leftWidth, width-24)
	rightWidth := width - leftWidth - 1
	left := strings.Join(fitLines(m.sshListLines(leftWidth, height), leftWidth, height), "\n")
	right := strings.Join(fitLines(m.sshDetailLines(rightWidth), rightWidth, height), "\n")
	return joinColumns(left, right, leftWidth, height)
}

func (m *Model) sshListLines(width, height int) []string {
	lines := []string{
		styleDim.Render(padRight(fmt.Sprintf("%-16s %-20s %-14s %s", "NAME", "SSH", "BUNDLE", "CLEANUP"), width)),
	}
	tableRows := maxInt(1, height-1)
	end := minInt(len(m.sshTargets), m.sshOffset+tableRows)
	for i := m.sshOffset; i < end; i++ {
		t := &m.sshTargets[i]
		cleanup := "keep"
		if t.CleanupOnExit {
			cleanup = "remove"
		}
		line := fmt.Sprintf("%-16s %-20s %-14s %s",
			truncate(t.Name, 16), truncate(t.Destination, 20), truncate(t.Bundle, 14), cleanup)
		line = truncate(line, width)
		if i == m.sshCursor {
			line = styleSelect.Render(padRight(line, width))
		} else if !t.BundleExists || !t.BundleValid {
			line = styleErr.Render(line)
		} else if t.CacheFresh {
			line = styleOK.Render(line)
		}
		lines = append(lines, line)
	}
	if len(m.sshTargets) == 0 {
		if m.sshHostsErr != "" {
			lines = append(lines, styleErr.Render(truncate(m.sshHostsErr, width)))
		} else {
			lines = append(lines, styleDim.Render("no SSH targets configured; press n to add one"))
		}
	}
	return lines
}

func (m *Model) sshDetailLines(width int) []string {
	width = maxInt(10, width)
	t := m.currentSSHTarget()
	if t == nil {
		return []string{styleDim.Render("nothing selected")}
	}
	lines := []string{styleTitle.Render(truncate(t.Name, width))}
	add := func(label, value string) {
		if value == "" {
			return
		}
		lines = append(lines, styleDim.Render(label+":")+" "+truncate(value, maxInt(4, width-len(label)-2)))
	}
	add("bundle", t.Bundle)
	add("target", t.BundleTarget)
	add("copy", t.BundlePath)
	if !t.BundleExists {
		lines = append(lines, styleErr.Render("bundle executable is missing; rebuild it"))
	} else if !t.BundleValid {
		lines = append(lines, styleErr.Render("bundle executable is invalid; rebuild it"))
	} else {
		add("size", formatSize(t.BundleSize))
	}
	add("ssh", t.Destination)
	add("source", t.SSH.Source)
	if t.Resolved != nil {
		add("host", t.Resolved.HostName)
		if t.Resolved.User != "" {
			add("user", t.Resolved.User)
		}
		if t.Resolved.Port > 0 {
			add("port", strconv.Itoa(t.Resolved.Port))
		}
		if t.Resolved.IdentityFile != "" {
			add("identity", t.Resolved.IdentityFile)
		}
		if t.Resolved.ProxyJump != "" {
			add("proxy", t.Resolved.ProxyJump)
		}
	} else {
		add("host", t.SSH.Host)
		add("user", t.SSH.User)
		if t.SSH.Port > 0 {
			add("port", strconv.Itoa(t.SSH.Port))
		}
		add("identity", t.SSH.IdentityFile)
		add("proxy", t.SSH.ProxyJump)
	}
	if t.ResolveError != "" {
		lines = append(lines, styleErr.Render(truncate(t.ResolveError, width)))
	}
	add("remote", t.RemoteRootDisplay)
	add("shell", t.EffectiveShell())
	add("cleanup", boolLabel(t.CleanupOnExit, "remove remote root", "keep"))
	add("force", boolLabel(t.Force, "yes", "no"))
	if t.CacheFresh {
		add("cache", "fresh (shell only)")
	} else {
		add("cache", "will copy and run")
	}
	if !t.LastCopied.IsZero() {
		add("copied", humanAge(t.LastCopied))
	}
	if !t.LastConnected.IsZero() {
		add("connected", humanAge(t.LastConnected))
	}
	if m.sshHostsErr != "" {
		lines = append(lines, "", styleErr.Render(truncate(m.sshHostsErr, width)))
	}
	return lines
}

func boolLabel(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}

func (m *Model) viewSSHWizard(width, height int) string {
	d := m.sshDraft
	if d == nil {
		return ""
	}
	switch d.step {
	case sshStepName:
		lines := []string{
			styleTitle.Render("New SSH target"),
			styleDim.Render("Step 1 of 5 · name"),
			"",
			styleAccent.Render("Target name"),
			"  " + d.nameInput.View(),
			"",
		}
		if d.err != "" {
			lines = append(lines, styleErr.Render(d.err))
		} else {
			lines = append(lines, styleDim.Render("A short identifier such as staging."))
		}
		return strings.Join(fitLines(lines, width, height), "\n")

	case sshStepSource:
		lines := []string{
			styleTitle.Render("New SSH target: " + d.name),
			styleDim.Render("Step 2 of 5 · connection source"),
			"",
			styleAccent.Render("Host from ~/.ssh/config, or a custom server"),
		}
		switch {
		case d.sourceSearching:
			lines = append(lines, "Search: "+d.sourceSearch.View())
		case d.sourceQuery != "":
			lines = append(lines, "Search: "+d.sourceQuery)
		default:
			lines = append(lines, styleDim.Render("Press / to filter SSH config hosts."))
		}
		lines = append(lines, "")
		options := d.sourceOptions()
		if len(options) == 0 {
			lines = append(lines, styleDim.Render("no matching SSH config hosts"))
		} else {
			cursor := d.sourceCursor
			if cursor >= len(options) {
				cursor = len(options) - 1
			}
			listHeight := maxInt(1, height-len(lines)-1)
			start := 0
			if cursor >= listHeight {
				start = cursor - listHeight + 1
			}
			end := minInt(len(options), start+listHeight)
			for i := start; i < end; i++ {
				opt := options[i]
				label := opt.Alias
				if opt.Custom {
					label = "Custom server (hostname / user)"
				}
				line := "  " + label
				if i == cursor {
					line = styleSelect.Render(padRight(line, width))
				}
				lines = append(lines, line)
			}
		}
		if d.err != "" {
			lines = append(lines, "", styleErr.Render(d.err))
		}
		return strings.Join(fitLines(lines, width, height), "\n")

	case sshStepDestination:
		labels, inputs := d.destinationFields()
		lines := []string{
			styleTitle.Render("New SSH target: " + d.name),
			styleDim.Render("Step 3 of 5 · destination"),
			"",
		}
		if d.custom {
			lines = append(lines, styleDim.Render("Explicit server; ssh config is not used."))
		} else {
			lines = append(lines, styleDim.Render("Using ssh config alias: ")+styleAccent.Render(d.configHost))
		}
		lines = append(lines, "")
		for i, label := range labels {
			line := "  " + padRight(label, 12) + " " + inputs[i].View()
			if i == d.destField {
				line = styleSelect.Render(padRight(line, width))
			}
			lines = append(lines, line)
		}
		if d.err != "" {
			lines = append(lines, "", styleErr.Render(d.err))
		} else {
			lines = append(lines, "", styleDim.Render("empty override fields inherit the SSH config value"))
		}
		return strings.Join(fitLines(lines, width, height), "\n")

	case sshStepBundle:
		lines := []string{
			styleTitle.Render("New SSH target: " + d.name),
			styleDim.Render("Step 4 of 5 · bundle"),
			"",
			styleAccent.Render("Bundle to copy"),
		}
		switch {
		case d.bundleSearching:
			lines = append(lines, "Search: "+d.bundleSearch.View())
		case d.bundleQuery != "":
			lines = append(lines, "Search: "+d.bundleQuery)
		default:
			lines = append(lines, styleDim.Render("Press / to filter bundles."))
		}
		lines = append(lines, "")
		visible := d.visibleBundles()
		if len(visible) == 0 {
			switch {
			case d.bundleQuery != "":
				lines = append(lines, styleDim.Render("no bundles match the filter"))
			case len(d.bundles) > 0:
				lines = append(lines, styleDim.Render("no SSH-capable bundle; build one for linux or darwin"))
			default:
				lines = append(lines, styleDim.Render("no bundles available; create one in the Bundles tab"))
			}
			if d.err != "" {
				lines = append(lines, "", styleErr.Render(d.err))
			}
			return strings.Join(fitLines(lines, width, height), "\n")
		}
		cursor := d.bundleCursor
		if cursor >= len(visible) {
			cursor = len(visible) - 1
		}
		listHeight := maxInt(1, height-len(lines)-1)
		start := 0
		if cursor >= listHeight {
			start = cursor - listHeight + 1
		}
		end := minInt(len(visible), start+listHeight)
		for i := start; i < end; i++ {
			b := d.bundles[visible[i]]
			tag := frozenLabel(b.Frozen)
			if problem := bundleSSHProblem(b); problem != "" {
				tag = "not for SSH"
			}
			line := fmt.Sprintf("  %-20s %-14s %s", truncate(b.Name, 20), b.Target, tag)
			if i == cursor {
				line = styleSelect.Render(padRight(line, width))
			}
			lines = append(lines, line)
		}
		if d.err != "" {
			lines = append(lines, "", styleErr.Render(d.err))
		}
		return strings.Join(fitLines(lines, width, height), "\n")

	case sshStepOptions:
		lines := []string{
			styleTitle.Render("New SSH target: " + d.name),
			styleDim.Render("Step 5 of 5 · session options"),
			"",
		}
		add := func(label, value string) {
			lines = append(lines, styleAccent.Render(label), "  "+value)
		}
		add("Bundle", d.selectedBundle())
		add("SSH", d.destinationSummary())
		add("Remote root", displayRoot(d.remoteRoot.Value()))
		lines = append(lines, "", styleAccent.Render("Options"), "")
		options := []struct {
			label string
			value string
		}{
			{"Shell", d.selectedShell()},
			{"Remote root", displayRoot(d.remoteRoot.Value())},
			{"Cleanup after exit", boolLabel(d.cleanup, "yes", "no")},
			{"Force bundle install", boolLabel(d.force, "yes", "no")},
			{"Save target", ""},
		}
		for i, opt := range options {
			line := "  " + padRight(opt.label, 22)
			if opt.value != "" {
				line += " " + opt.value
			}
			if i == d.optionsCursor {
				line = styleSelect.Render(padRight(line, width))
			}
			lines = append(lines, line)
			if i == 1 && d.editingRoot {
				lines = append(lines, "    "+d.remoteRoot.View())
			}
		}
		if d.err != "" {
			lines = append(lines, "", styleErr.Render(d.err))
		}
		return strings.Join(fitLines(lines, width, height), "\n")
	}
	return ""
}

func (d *sshDraft) destinationSummary() string {
	if d.custom {
		return strings.TrimSpace(d.hostInput.Value())
	}
	return d.configHost
}

func displayRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return "~/.cpt"
	}
	return root
}

func (m *Model) sshWizardHelpLine() string {
	d := m.sshDraft
	if d == nil {
		return ""
	}
	switch d.step {
	case sshStepName:
		return hintLine(keyHint("enter", "next"), keyHint("esc", "cancel"))
	case sshStepSource:
		return hintLine(keyHint("/", "search"), keyHint("↑/↓", "choose"),
			keyHint("enter", "next"), keyHint("shift+tab", "back"), keyHint("esc", "cancel"))
	case sshStepDestination:
		return hintLine(keyHint("enter", "next field"), keyHint("↑/↓", "field"),
			keyHint("shift+tab", "back"), keyHint("esc", "cancel"))
	case sshStepBundle:
		return hintLine(keyHint("/", "search"), keyHint("↑/↓", "choose"),
			keyHint("enter", "next"), keyHint("shift+tab", "back"), keyHint("esc", "cancel"))
	case sshStepOptions:
		if d.editingRoot {
			return hintLine(keyHint("enter", "done"), keyHint("esc", "stop editing"))
		}
		return hintLine(keyHint("↑/↓", "option"), keyHint("enter/space", "change"),
			keyHint("←/→", "shell"), keyHint("enter", "save on last row"),
			keyHint("shift+tab", "back"), keyHint("esc", "cancel"))
	}
	return ""
}
