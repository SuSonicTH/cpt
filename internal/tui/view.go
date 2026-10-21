package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/version"
)

// View renders the whole screen.
func (m *Model) View() string {
	width := m.width
	height := m.height
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 30
	}
	header := m.viewHeader(width)
	if height < 4 {
		return truncate(header, width)
	}
	bodyHeight := maxInt(1, height-3)
	var body string
	switch {
	case m.help:
		body = m.viewHelp(width, bodyHeight)
	case m.overlay == overlayShell:
		body = m.viewShell(width, bodyHeight)
	case m.overlay == overlayDetail:
		body = m.viewDetailOverlay(width, bodyHeight)
	default:
		switch m.tab {
		case tabPackages:
			body = m.viewPackages(width, bodyHeight)
		case tabLists:
			body = m.viewLists(width, bodyHeight)
		case tabBundles:
			body = m.viewBundles(width, bodyHeight)
		case tabLog:
			body = m.viewLog(width, bodyHeight)
		}
	}
	footer := m.viewFooter(width)
	return header + "\n" + body + "\n" + footer
}

func (m *Model) viewHeader(width int) string {
	var tabs []string
	for i, name := range tabNames {
		if tab(i) == m.tab {
			tabs = append(tabs, styleTabOn.Render(name))
		} else {
			tabs = append(tabs, styleTabOff.Render(name))
		}
	}
	left := styleTitle.Render("cpt") + " " + styleDim.Render(version.Version) + "   " + strings.Join(tabs, "  ")
	var badges []string
	if n := m.updateCount(); n > 0 {
		badges = append(badges, styleBadge.Render(fmt.Sprintf("%d updates", n)))
	}
	if n := m.selectionCount(); n > 0 {
		badges = append(badges, styleSelect.Render(fmt.Sprintf("%d selected", n)))
	}
	if m.pendingShells > 0 {
		badges = append(badges, styleWarn.Render(fmt.Sprintf("%d shell change(s)", m.pendingShells)))
	}
	if m.app.Offline {
		badges = append(badges, styleDim.Render("offline"))
	}
	right := strings.Join(badges, styleDim.Render(" · "))
	if right == "" {
		return truncate(left, width)
	}
	return truncate(padBetween(left, right, width), width)
}

func (m *Model) viewPackages(width, height int) string {
	if width < 50 {
		return strings.Join(fitLines(m.packageListLines(width, height), width, height), "\n")
	}
	leftWidth := width * 46 / 100
	leftWidth = maxInt(30, leftWidth)
	leftWidth = minInt(leftWidth, width-24)
	rightWidth := width - leftWidth - 1
	left := strings.Join(fitLines(m.packageListLines(leftWidth, height), leftWidth, height), "\n")
	right := strings.Join(fitLines(m.detailLines(rightWidth), rightWidth, height), "\n")
	return joinColumns(left, right, leftWidth, height)
}

func (m *Model) packageListLines(width, height int) []string {
	if m.loadingRows && len(m.rows) == 0 {
		return []string{styleDim.Render(m.spinnerFrame() + " loading packages…")}
	}
	if m.rowErr != "" && len(m.rows) == 0 {
		return []string{styleErr.Render(truncate(m.rowErr, width))}
	}
	if len(m.filtered) == 0 {
		if m.query != "" {
			return []string{styleDim.Render("no packages match " + m.query)}
		}
		return []string{styleDim.Render("no packages available; press 2 to manage lists")}
	}
	var lines []string
	end := minInt(len(m.filtered), m.offset+height)
	for i := m.offset; i < end; i++ {
		r := &m.rows[m.filtered[i]]
		line := m.packageLine(r, width, i == m.cursor)
		if i == m.cursor {
			line = styleSelect.Render(padRight(line, width))
		}
		lines = append(lines, line)
	}
	return lines
}

func (m *Model) packageLine(r *app.Row, width int, cursor bool) string {
	glyph, kind := m.packageGlyph(r)
	marker := "  "
	if cursor {
		marker = "▸ "
	}
	check := "[ ]"
	if m.selected[r.Name] {
		check = "[x]"
	}
	reserved := 9
	version := r.Version
	if a := m.activities[r.Name]; a != nil && !a.Finished {
		version = activityText(a)
	} else if r.Status == "update" && r.Latest != "" {
		if version == "" {
			version = "→" + r.Latest
		} else {
			version = version + "→" + r.Latest
		}
	}
	if version == "" {
		version = "—"
	}
	version = truncate(version, maxInt(4, width/3))
	nameWidth := width - reserved - lipgloss.Width(version) - 1
	if nameWidth < 5 {
		nameWidth = 5
	}
	name := truncate(r.Name, nameWidth)
	glyphText := glyph
	if !m.selected[r.Name] {
		// A nested colour reset would end the selection highlight early.
		glyphText = m.styleFor(kind).Render(glyph)
	}
	versionText := version
	if !m.selected[r.Name] {
		versionText = styleDim.Render(version)
	}
	return marker + check + " " + glyphText + " " + padRight(name, nameWidth) + " " + versionText
}

func (m *Model) packageGlyph(r *app.Row) (string, string) {
	if a := m.activities[r.Name]; a != nil {
		switch {
		case a.State == "failed":
			return "✗", "error"
		case !a.Finished:
			return "⧗", "warn"
		}
	}
	switch {
	case r.Conflict || r.Error != "":
		return "✗", "error"
	case r.Pinned:
		return "✓", "ok"
	case r.Status == "update":
		return "↑", "warn"
	case r.Version != "":
		return "●", "ok"
	default:
		return "○", "dim"
	}
}

func (m *Model) detailLines(width int) []string {
	width = maxInt(10, width)
	r := m.currentRow()
	if r == nil {
		if m.loadingRows {
			return []string{styleDim.Render(m.spinnerFrame() + " loading…")}
		}
		return []string{styleDim.Render("nothing selected")}
	}
	lines := []string{styleTitle.Render(truncate(r.Name, width))}
	if r.Description != "" {
		lines = append(lines, "")
		lines = append(lines, wrapText(r.Description, width, 0)...)
	}
	lines = append(lines, "")
	add := func(label, value string) {
		if value == "" {
			return
		}
		wrapped := wrapText(value, width, len(label)+2)
		if len(wrapped) == 0 {
			return
		}
		first := styleDim.Render(label+":") + " " + wrapped[0]
		lines = append(lines, first)
		lines = append(lines, wrapped[1:]...)
	}
	add("list", r.List)
	if r.Tags != "" {
		add("tags", r.Tags)
	}
	status := r.Status
	if r.Pinned {
		status += " (pinned)"
	}
	add("status", status)
	if r.Version != "" {
		installed := r.Version
		if r.Checksum != "" {
			installed += " · " + r.Checksum
		}
		add("installed", installed)
	} else {
		add("installed", "no")
	}
	if r.Latest != "" {
		add("latest", r.Latest)
	}
	add("asset", r.Asset)
	if r.ChecksumSource != "" {
		add("checksum", r.ChecksumSource)
	}
	if r.SHA256 != "" {
		add("sha256", r.SHA256)
	}
	if len(r.Bins) > 0 {
		add("bins", strings.Join(r.Bins, ", "))
	}
	if r.Homepage != "" {
		add("homepage", r.Homepage)
	}
	if r.License != "" {
		add("license", r.License)
	}
	if r.Error != "" {
		lines = append(lines, "")
		lines = append(lines, styleErr.Render("error: "+r.Error))
	}
	if a := m.activities[r.Name]; a != nil {
		lines = append(lines, "")
		line := m.spinnerFrame() + " " + a.State
		if a.Err != "" {
			line = "✗ " + a.Err
		}
		lines = append(lines, styleDim.Render(truncate(line, width)))
	}
	return lines
}

func (m *Model) viewDetailOverlay(width, height int) string {
	lines := m.detailLines(width)
	if len(lines) == 0 {
		return styleDim.Render("nothing selected")
	}
	offset := m.detailOffset
	if offset > len(lines)-1 {
		offset = maxInt(0, len(lines)-1)
	}
	if offset < 0 {
		offset = 0
	}
	return strings.Join(fitLines(lines[offset:], width, height), "\n")
}

func (m *Model) viewLists(width, height int) string {
	lines := []string{
		styleDim.Render(padRight(fmt.Sprintf("%-20s %-9s %-13s %-10s %s", "NAME", "PACKAGES", "UPDATED", "STATUS", "ORIGIN"), width)),
	}
	tableRows := maxInt(3, height-8)
	end := minInt(len(m.lists), m.listOffset+tableRows)
	for i := m.listOffset; i < end; i++ {
		li := &m.lists[i]
		name := li.Name
		if li.Builtin {
			name += "*"
		}
		updated := "never"
		if !li.FetchedAt.IsZero() {
			updated = humanAge(li.FetchedAt)
		}
		status := "ok"
		kind := "dim"
		switch {
		case li.Conflicts > 0:
			status = "conflict"
			kind = "error"
		case li.Readonly:
			status = "readonly"
			kind = "warn"
		case li.Stale:
			status = "stale"
			kind = "warn"
		}
		line := fmt.Sprintf("%-20s %-9d %-13s %-10s %s",
			truncate(name, 20), li.Packages, updated, status, li.Origin)
		line = truncate(line, width)
		if i == m.listCursor {
			line = styleSelect.Render(padRight(line, width))
		} else if kind == "warn" {
			line = styleWarn.Render(line)
		}
		lines = append(lines, line)
	}
	if len(m.lists) == 0 {
		lines = append(lines, styleDim.Render("no lists configured"))
	}
	lines = append(lines, "")
	if li := m.currentList(); li != nil {
		lines = append(lines, styleTitle.Render(li.Name))
		lines = append(lines, wrapText(li.Origin, width, 0)...)
		detail := fmt.Sprintf("%d packages · %s · %s", li.Packages, updatedLabel(li.FetchedAt), visibility(li))
		if li.Conflicts > 0 {
			detail += fmt.Sprintf(" · %d conflicting package(s)", li.Conflicts)
		}
		lines = append(lines, styleDim.Render(detail))
	}
	if m.pendingShells > 0 {
		lines = append(lines, "")
		lines = append(lines, styleWarn.Render(fmt.Sprintf("%d shell snippet change(s) pending — press s to review", m.pendingShells)))
	}
	if m.app.Offline {
		lines = append(lines, "")
		lines = append(lines, styleDim.Render("offline: refresh and add are disabled"))
	}
	return strings.Join(fitLines(lines, width, height), "\n")
}

func (m *Model) viewShell(width, height int) string {
	if width < 50 {
		return strings.Join(fitLines([]string{styleWarn.Render("terminal is too narrow for a diff")}, width, height), "\n")
	}
	leftWidth := m.shellLeftWidth()
	var left []string
	left = append(left, styleTitle.Render("Shell changes"), "")
	end := minInt(len(m.shellPending), m.shellListOffset+maxInt(1, height-2))
	for i := m.shellListOffset; i < end; i++ {
		name := m.shellPending[i]
		info := m.shellInfo(name)
		line := "  " + name
		if info != nil {
			switch {
			case info.Removal:
				line += " (remove)"
			case info.Pending:
				line += " " + info.PendingVersion
			}
		}
		line = truncate(line, leftWidth)
		if i == m.shellCursor {
			line = styleSelect.Render(padRight("▸ "+strings.TrimPrefix(line, "  "), leftWidth))
		}
		left = append(left, line)
	}
	var right []string
	name := m.currentShell()
	if name == "" {
		right = []string{styleDim.Render("nothing pending")}
	} else {
		right = append(right, styleTitle.Render(name), "")
		switch {
		case m.shellDiffErr != "":
			right = append(right, styleErr.Render(m.shellDiffErr))
		case strings.TrimSpace(m.shellDiff) == "":
			right = append(right, styleDim.Render("no textual difference"))
		default:
			diff := m.shellDiffWrapped
			offset := m.shellOffset
			if offset > len(diff)-1 {
				offset = maxInt(0, len(diff)-1)
			}
			right = append(right, diff[offset:]...)
		}
		right = append(right, "", styleDim.Render("A approve · R reject · a approve all · x reject all · pgup/pgdn scroll"))
	}
	rightWidth := m.shellRightWidth()
	leftText := strings.Join(fitLines(left, leftWidth, height), "\n")
	rightText := strings.Join(fitLines(right, rightWidth, height), "\n")
	return joinColumns(leftText, rightText, leftWidth, height)
}

func (m *Model) viewBundles(width, height int) string {
	lines := []string{
		styleDim.Render(padRight(fmt.Sprintf("%-18s %-14s %-9s %-10s %-12s %s", "NAME", "TARGET", "PACKAGES", "SIZE", "CREATED", "KIND"), width)),
	}
	tableRows := maxInt(3, height-8)
	end := minInt(len(m.bundles), m.bundleOffset+tableRows)
	for i := m.bundleOffset; i < end; i++ {
		b := &m.bundles[i]
		kind := "live"
		kindStyle := "dim"
		if b.Frozen {
			kind = "frozen"
			kindStyle = "warn"
		}
		line := fmt.Sprintf("%-18s %-14s %-9d %-10s %-12s %s",
			truncate(b.Name, 18), b.Target, b.Packages, formatSize(b.Size), humanAge(b.Created), kind)
		line = truncate(line, width)
		if i == m.bundleCursor {
			line = styleSelect.Render(padRight(line, width))
		} else if kindStyle == "warn" {
			line = styleWarn.Render(line)
		}
		lines = append(lines, line)
	}
	if len(m.bundles) == 0 {
		lines = append(lines, styleDim.Render("no bundles built yet; press n to create one"))
	}
	lines = append(lines, "")
	if b := m.currentBundle(); b != nil {
		lines = append(lines, styleTitle.Render(b.Name))
		lines = append(lines, wrapText(b.Path, width, 0)...)
		detail := fmt.Sprintf("%s · %d packages · %s · built %s", b.Target, b.Packages, frozenLabel(b.Frozen), humanAge(b.Created))
		lines = append(lines, styleDim.Render(detail))
		lines = append(lines, styleDim.Render("manifest: "+b.Manifest))
		if b.Frozen {
			lines = append(lines, styleDim.Render("frozen: cpt update explains itself until --unfreeze"))
		}
	}
	if m.bundleDraft != nil {
		lines = append(lines, "", styleAccent.Render("bundle wizard: "+m.bundleDraft.name+" → "+m.bundleDraft.target))
	}
	lines = append(lines, "", styleDim.Render("n new · r rebuild · d remove · enter path · tab switch · q quit"))
	return strings.Join(fitLines(lines, width, height), "\n")
}

func frozenLabel(frozen bool) string {
	if frozen {
		return "frozen"
	}
	return "live"
}

func (m *Model) viewLog(width, height int) string {
	lines := []string{styleDim.Render(truncate(filepath.Join(m.app.Paths.Logs, "cpt.log"), width))}
	if m.logErr != "" {
		lines = append(lines, styleErr.Render(truncate(m.logErr, width)))
	}
	view := m.log.View()
	if strings.TrimSpace(view) == "" {
		view = styleDim.Render("log is empty")
	}
	lines = append(lines, strings.Split(view, "\n")...)
	return strings.Join(fitLines(lines, width, height), "\n")
}

func (m *Model) viewFooter(width int) string {
	return m.statusLine(width) + "\n" + truncate(m.helpLine(), width)
}

func (m *Model) statusLine(width int) string {
	if m.prompt != nil {
		return styleAccent.Render(truncate(m.prompt.title, maxInt(10, width-lipgloss.Width(m.prompt.input.View())-3))) + ": " + m.prompt.input.View()
	}
	text := m.status
	isErr := m.statusErr
	if text == "" {
		switch {
		case m.running:
			text = m.spinnerFrame() + " " + m.opName + " " + m.activitySummary()
		case m.loadingRows:
			text = m.spinnerFrame() + " loading packages…"
		default:
			text = m.activitySummary()
		}
		if m.rowErr != "" && text == "" {
			text = m.rowErr
			isErr = true
		}
	}
	if text == "" {
		return ""
	}
	inner := maxInt(0, width-2)
	text = " " + truncate(text, inner) + " "
	if isErr {
		return styleStatus.Foreground(lipgloss.Color("9")).Render(text)
	}
	return styleStatus.Render(text)
}

func (m *Model) activitySummary() string {
	var active []string
	var done, total int64
	for _, name := range m.order {
		a := m.activities[name]
		if a == nil || a.Finished {
			continue
		}
		label := name + " " + activityText(a)
		active = append(active, m.spinnerFrame()+" "+label)
		if a.State == "downloading" && a.Total > 0 {
			done += a.Done
			total += a.Total
		}
	}
	if len(active) > 0 {
		line := strings.Join(active[:minInt(3, len(active))], " · ")
		if len(active) > 3 {
			line += fmt.Sprintf(" +%d", len(active)-3)
		}
		if total > 0 {
			line += " " + progressBar(done, total, 16)
		}
		return line
	}
	var recent []string
	for i := len(m.order) - 1; i >= 0 && len(recent) < 3; i-- {
		a := m.activities[m.order[i]]
		if a == nil || !a.Finished {
			continue
		}
		glyph := "✓"
		kind := "ok"
		if a.State == "failed" {
			glyph = "✗"
			kind = "error"
		}
		label := m.order[i]
		if a.Version != "" {
			label += " " + a.Version
		}
		if a.Action != "" {
			label += " " + a.Action
		} else if a.State != "" {
			label += " " + a.State
		}
		if a.Err != "" {
			label = a.Err
		}
		recent = append(recent, m.styleFor(kind).Render(glyph+" "+truncate(label, 48)))
	}
	return strings.Join(recent, " · ")
}

func (m *Model) helpLine() string {
	if m.prompt != nil {
		return "enter confirm · esc cancel"
	}
	if m.overlay == overlayShell {
		return "↑/↓ select · pgup/pgdn scroll · A approve · R reject · a approve all · x reject all · esc close"
	}
	if m.overlay == overlayDetail {
		return "↑/↓ scroll · pgup/pgdn · esc close"
	}
	if m.running {
		return "esc cancel · q quit"
	}
	switch m.tab {
	case tabPackages:
		return "/ search · space select · a all updates · i install · u update · d remove · p pin · enter detail · r refresh · q quit"
	case tabLists:
		return "n add · r refresh · R refresh all · x remove · s shell changes · tab switch · q quit"
	case tabBundles:
		return "n new · r rebuild · d remove · enter path · tab switch · q quit"
	case tabLog:
		return "↑/↓ scroll · pgup/pgdn · g/G top/bottom · tab switch · q quit"
	}
	return "q quit"
}

func (m *Model) viewHelp(width, height int) string {
	lines := []string{
		styleTitle.Render("cpt keys"),
		"",
		styleAccent.Render("Global"),
		"  tab / shift+tab   switch tabs (1, 2, 3, 4 jump directly)",
		"  ?                 toggle this help",
		"  q / ctrl+c        quit",
		"  esc               cancel an operation, clear search or selection",
		"",
		styleAccent.Render("Packages"),
		"  /                 fuzzy search name, description and tags",
		"  ↑/↓, j/k          move · pgup/pgdn · g/G top/bottom",
		"  space             select or unselect for a batch operation",
		"  a                 select every package with an update",
		"  i                 install selected (or the highlighted) package",
		"  u                 update selected, or every available update",
		"  d                 remove selected (or the highlighted) package",
		"  X                 force-remove packages whose files were modified",
		"  p                 pin the installed version / unpin when pinned",
		"  enter             full-screen detail (works on narrow terminals)",
		"  r                 re-check upstream versions",
		"",
		styleAccent.Render("Lists"),
		"  n                 add a list by URL or owner/repo",
		"  r / R             refresh the selected list / all lists",
		"  x                 remove the selected list",
		"  s                 review pending shell snippet changes",
		"",
		styleAccent.Render("Bundles"),
		"  n                 create a bundle (name, target, packages, live/frozen)",
		"  r                 rebuild the selected bundle from its exact versions",
		"  d                 delete the selected bundle and its manifest",
		"  enter             show the bundle file path",
		"",
		styleAccent.Render("Log"),
		"  ↑/↓, pgup/pgdn    scroll · g/G top/bottom",
		"",
		styleDim.Render("Shell changes: A approves, R rejects, a approves all, x rejects all."),
	}
	return strings.Join(fitLines(lines, width, height), "\n")
}

func activityText(a *activity) string {
	if a == nil {
		return ""
	}
	if a.Err != "" {
		return truncate(a.Err, 40)
	}
	if a.State == "downloading" && a.Total > 0 {
		return fmt.Sprintf("%d%%", a.Done*100/a.Total)
	}
	if a.Action != "" {
		return a.Action
	}
	if a.State == "" {
		return "working"
	}
	return a.State
}

func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func updatedLabel(t time.Time) string {
	if t.IsZero() {
		return "never updated"
	}
	return "updated " + humanAge(t)
}

func visibility(li *app.ListInfo) string {
	switch {
	case li.Builtin:
		return "builtin"
	case li.Readonly:
		return "frozen"
	default:
		return "writable"
	}
}
