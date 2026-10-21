package tui

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/progress"
)

type tab int

const (
	tabPackages tab = iota
	tabLists
	tabBundles
	tabLog
)

var tabNames = []string{"Packages", "Lists", "Bundles", "Log"}

func (t tab) String() string {
	if t >= 0 && int(t) < len(tabNames) {
		return tabNames[t]
	}
	return "?"
}

type promptKind int

const (
	promptNone promptKind = iota
	promptAddList
	promptRemoveList
	promptRemovePackages
	promptForceRemove
	promptBundleName
	promptBundleTarget
	promptBundlePackages
	promptBundleFrozen
	promptRemoveBundle
)

type promptState struct {
	kind  promptKind
	title string
	input textinput.Model
	names []string
	list  string
}

type overlay int

const (
	overlayNone overlay = iota
	overlayShell
	overlayDetail
)

type bundleDraft struct {
	name     string
	target   string
	packages []string
	frozen   bool
}

type activity struct {
	State    string
	Version  string
	Action   string
	Err      string
	Done     int64
	Total    int64
	Updated  time.Time
	Finished bool
}

// Model is the Bubble Tea model for the interactive interface.
type Model struct {
	ctx       context.Context
	ctxCancel context.CancelFunc
	app       *app.App

	events      <-chan progress.Event
	unsubscribe func()
	done        chan struct{}

	width  int
	height int
	tab    tab
	help   bool

	rows        []app.Row
	filtered    []int
	cursor      int
	offset      int
	selected    map[string]bool
	query       string
	searching   bool
	search      textinput.Model
	loadingRows bool
	rowErr      string

	lists         []app.ListInfo
	shells        []app.ShellInfo
	pendingShells int
	listCursor    int
	listOffset    int

	bundles      []app.BundleInfo
	bundleCursor int
	bundleOffset int
	bundleDraft  *bundleDraft

	logLines   []string
	liveLog    []string
	log        viewport.Model
	logErr     string
	logReading bool

	prompt           *promptState
	overlay          overlay
	shellCursor      int
	shellPending     []string
	shellListOffset  int
	shellDiff        string
	shellDiffWrapped []string
	shellDiffErr     string
	shellDiffTotal   int
	shellOffset      int
	detailOffset     int
	detailTotal      int

	activities  map[string]*activity
	order       []string
	running     bool
	opName      string
	cancel      context.CancelFunc
	probeCancel context.CancelFunc

	spinner   int
	status    string
	statusErr bool
	statusSeq int
	quitting  bool
}

func newModel(ctx context.Context, a *app.App) *Model {
	var events <-chan progress.Event
	unsubscribe := func() {}
	if a.Events != nil {
		events, unsubscribe = a.Events.Subscribe()
	}
	search := textinput.New()
	search.Placeholder = "name, description or tag"
	search.CharLimit = 200
	search.Prompt = "/ "

	runCtx, runCancel := context.WithCancel(ctx)
	m := &Model{
		ctx:         runCtx,
		ctxCancel:   runCancel,
		app:         a,
		events:      events,
		unsubscribe: unsubscribe,
		done:        make(chan struct{}),
		tab:         tabPackages,
		selected:    map[string]bool{},
		search:      search,
		activities:  map[string]*activity{},
		width:       100,
		height:      30,
		loadingRows: true,
	}
	m.log = viewport.New(80, 10)
	return m
}

// Init starts data loading, the log tail and the event pump.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		tickCmd(),
		startupCmd(m.ctx, m.app),
		readLogCmd(m.app.Paths.Logs),
		waitEventCmd(m.events, m.done),
	}
	return tea.Batch(cmds...)
}

func (m *Model) refilter() {
	query := strings.ToLower(strings.TrimSpace(m.query))
	m.filtered = m.filtered[:0]
	for i := range m.rows {
		r := &m.rows[i]
		if query == "" || fuzzyRow(r, query) {
			m.filtered = append(m.filtered, i)
		}
	}
	switch {
	case len(m.filtered) == 0:
		m.cursor = 0
		m.offset = 0
	case m.cursor >= len(m.filtered):
		m.cursor = len(m.filtered) - 1
	}
	m.ensureCursorVisible(m.bodyHeight())
}

// fuzzyRow reports whether query is a case-insensitive subsequence of the
// package name, description or a tag. This is intentionally permissive: typing
// "fz" finds fzf, and "json" finds jq.
func fuzzyRow(r *app.Row, query string) bool {
	tags := strings.Split(r.Tags, ",")
	if app.MatchesQuery(r.Name, r.Description, tags, query) {
		return true
	}
	if fuzzyContains(r.Name, query) || fuzzyContains(r.Description, query) {
		return true
	}
	for _, tag := range tags {
		if fuzzyContains(tag, query) {
			return true
		}
	}
	return false
}

func fuzzyContains(text, query string) bool {
	qr := []rune(strings.ToLower(query))
	if len(qr) == 0 {
		return true
	}
	i := 0
	for _, r := range strings.ToLower(text) {
		if r == qr[i] {
			i++
			if i == len(qr) {
				return true
			}
		}
	}
	return false
}

func (m *Model) currentRow() *app.Row {
	if m.cursor < 0 || m.cursor >= len(m.filtered) {
		return nil
	}
	return &m.rows[m.filtered[m.cursor]]
}

func (m *Model) setRows(rows []app.Row) {
	m.rows = rows
	m.refilter()
	m.ensureCursorVisible(m.bodyHeight())
}

func (m *Model) selectedNames() []string {
	var names []string
	for i := range m.rows {
		if m.selected[m.rows[i].Name] {
			names = append(names, m.rows[i].Name)
		}
	}
	return names
}

func (m *Model) selectionCount() int {
	n := 0
	for _, r := range m.rows {
		if m.selected[r.Name] {
			n++
		}
	}
	return n
}

func (m *Model) updateCount() int {
	n := 0
	for _, r := range m.rows {
		if r.Status == "update" {
			n++
		}
	}
	return n
}

func (m *Model) currentList() *app.ListInfo {
	if m.listCursor < 0 || m.listCursor >= len(m.lists) {
		return nil
	}
	return &m.lists[m.listCursor]
}

func (m *Model) currentBundle() *app.BundleInfo {
	if m.bundleCursor < 0 || m.bundleCursor >= len(m.bundles) {
		return nil
	}
	return &m.bundles[m.bundleCursor]
}

func (m *Model) pendingShellNames() []string {
	names := make([]string, 0, len(m.shells))
	for _, info := range m.shells {
		if info.Pending {
			names = append(names, info.Package)
		}
	}
	sort.Strings(names)
	return names
}

func (m *Model) shellInfo(name string) *app.ShellInfo {
	for i := range m.shells {
		if m.shells[i].Package == name {
			return &m.shells[i]
		}
	}
	return nil
}

func (m *Model) currentShell() string {
	if m.shellCursor < 0 || m.shellCursor >= len(m.shellPending) {
		return ""
	}
	return m.shellPending[m.shellCursor]
}

func (m *Model) setStatus(text string, isErr bool) tea.Cmd {
	m.status = sanitize(text, false)
	m.statusErr = isErr
	m.statusSeq++
	if isErr {
		// Errors stay visible until the next action; transient statuses clear.
		return nil
	}
	seq := m.statusSeq
	return tea.Tick(6*time.Second, func(time.Time) tea.Msg { return clearStatusMsg{seq: seq} })
}

func (m *Model) activity(name string) *activity {
	if a, ok := m.activities[name]; ok {
		return a
	}
	a := &activity{State: string(progress.KindQueued), Updated: time.Now()}
	m.activities[name] = a
	m.order = append(m.order, name)
	m.pruneActivities()
	return a
}

func (m *Model) pruneActivities() {
	if len(m.order) <= 80 {
		return
	}
	keep := make([]string, 0, len(m.order))
	for _, name := range m.order {
		a := m.activities[name]
		if a != nil && a.Finished && len(keep) < 20 {
			delete(m.activities, name)
			continue
		}
		keep = append(keep, name)
	}
	m.order = keep
}

func (m *Model) applyEvent(e progress.Event) tea.Cmd {
	e.Package = sanitize(e.Package, false)
	e.Version = sanitize(e.Version, false)
	e.Action = sanitize(e.Action, false)
	e.Message = sanitize(e.Message, false)
	if e.Kind == progress.KindInfo {
		m.appendLog(e.Message)
		return nil
	}
	if e.Package == "" {
		return nil
	}
	a := m.activity(e.Package)
	a.Updated = time.Now()
	switch e.Kind {
	case progress.KindDone:
		a.State = e.Action
		if a.State == "" {
			a.State = "done"
		}
		a.Version = e.Version
		a.Action = e.Action
		a.Err = ""
		a.Finished = true
	case progress.KindFailed:
		a.State = "failed"
		a.Version = e.Version
		a.Err = e.Message
		a.Finished = true
	default:
		a.State = string(e.Kind)
		if e.Version != "" {
			a.Version = e.Version
		}
		if e.Kind == progress.KindDownloading {
			a.Done = e.Done
			a.Total = e.Total
		}
		a.Finished = false
	}
	return nil
}
