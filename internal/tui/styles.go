package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	styleTitle   = lipgloss.NewStyle().Bold(true)
	styleDim     = lipgloss.NewStyle().Faint(true)
	styleAccent  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleTabOn   = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color("12"))
	styleTabOff  = lipgloss.NewStyle().Faint(true)
	styleSelect  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	styleStatus  = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("237"))
	styleBadge   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleDivider = lipgloss.NewStyle().Faint(true)

	styleHintKey     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	styleHintBracket = lipgloss.NewStyle().Faint(true)
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// keyHint renders one footer shortcut. The brackets are faint so the yellow
// key itself is what draws the eye; labels stay in the terminal's default
// colour so the footer does not turn into a wall of colour.
func keyHint(key, label string) string {
	hint := styleHintBracket.Render("[") + styleHintKey.Render(key) + styleHintBracket.Render("]")
	if label == "" {
		return hint
	}
	return hint + " " + label
}

func hintLine(hints ...string) string {
	return strings.Join(hints, styleDim.Render(" · "))
}

func (m *Model) styleFor(kind string) lipgloss.Style {
	switch kind {
	case "ok":
		return styleOK
	case "warn":
		return styleWarn
	case "error":
		return styleErr
	case "accent":
		return styleAccent
	case "select":
		return styleSelect
	default:
		return styleDim
	}
}

func (m *Model) spinnerFrame() string {
	if len(spinnerFrames) == 0 {
		return ""
	}
	return spinnerFrames[m.spinner%len(spinnerFrames)]
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

func padRight(s string, width int) string {
	d := width - lipgloss.Width(s)
	if d <= 0 {
		return s
	}
	return s + strings.Repeat(" ", d)
}

func padBetween(left, right string, width int) string {
	if lipgloss.Width(right) >= width {
		return truncate(right, width)
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		left = truncate(left, maxInt(1, width-lipgloss.Width(right)-1))
		gap = width - lipgloss.Width(left) - lipgloss.Width(right)
	}
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// wrapText breaks text into lines of at most width cells, preserving explicit
// newlines. Continuation lines are indented; the first line is not, because
// callers usually render a label before it.
func wrapText(text string, width, indent int) []string {
	if width <= 1 {
		return []string{truncate(text, maxInt(1, width))}
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		prefix := strings.Repeat(" ", indent)
		first := true
		for _, word := range words {
			candidate := word
			if line != "" {
				candidate = line + " " + word
			}
			if lipgloss.Width(candidate)+indent <= width || line == "" {
				line = candidate
				continue
			}
			if first {
				out = append(out, line)
				first = false
			} else {
				out = append(out, prefix+line)
			}
			line = word
		}
		if line != "" {
			if first {
				out = append(out, line)
			} else {
				out = append(out, prefix+line)
			}
		}
	}
	return out
}

// sanitize removes terminal control characters and bidi overrides from text
// that came from a list, a log file or a remote snippet. keepNewlines preserves
// line breaks for diffs and logs.
func sanitize(s string, keepNewlines bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			if keepNewlines {
				b.WriteRune(r)
			} else {
				b.WriteByte(' ')
			}
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			continue
		case unicode.Is(unicode.Cf, r), r == '\u2028', r == '\u2029':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitizeVisible renders control characters as visible escapes instead of
// silently dropping them. It is used for shell snippets, where hiding a
// terminal escape would make the approval diff untrustworthy.
func sanitizeVisible(s string, keepNewlines bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' && keepNewlines:
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case r < 0x20, r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, "\\u%04x", r)
		case unicode.Is(unicode.Cf, r), r == '\u2028', r == '\u2029':
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// wrapDiffLines wraps long diff lines while repeating their +/- marker, so a
// wrapped addition cannot be mistaken for a context line. It operates on plain
// sanitised text, never on styled strings.
func wrapDiffLines(lines []string, width int) []string {
	if width < 4 {
		width = 4
	}
	var out []string
	for _, line := range lines {
		if line == "" {
			out = append(out, "")
			continue
		}
		// Keep standard unified-diff headers intact.
		if strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "@@ ") {
			out = append(out, line)
			continue
		}
		prefix := "  "
		body := line
		if line[0] == '+' || line[0] == '-' || line[0] == ' ' {
			prefix = string(line[0]) + " "
			body = line[1:]
		}
		chunks := splitByWidth(body, width-lipgloss.Width(prefix))
		if len(chunks) == 0 {
			chunks = []string{""}
		}
		for _, chunk := range chunks {
			out = append(out, prefix+chunk)
		}
	}
	return out
}

func splitByWidth(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > width && used > 0 {
			out = append(out, b.String())
			b.Reset()
			used = 0
		}
		b.WriteRune(r)
		used += rw
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func fitLines(lines []string, width, height int) []string {
	if height < 1 {
		height = 1
	}
	out := make([]string, height)
	for i := 0; i < height; i++ {
		if i < len(lines) {
			out[i] = truncate(lines[i], width)
		}
	}
	return out
}

func joinColumns(left, right string, leftWidth, height int) string {
	ll := strings.Split(left, "\n")
	rl := strings.Split(right, "\n")
	var b strings.Builder
	for i := 0; i < height; i++ {
		var l, r string
		if i < len(ll) {
			l = ll[i]
		}
		if i < len(rl) {
			r = rl[i]
		}
		b.WriteString(padRight(l, leftWidth))
		b.WriteString(styleDivider.Render("│"))
		b.WriteString(r)
		if i < height-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func progressBar(done, total int64, width int) string {
	if width < 4 {
		width = 4
	}
	ratio := 0.0
	if total > 0 {
		ratio = float64(done) / float64(total)
	}
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	filled := int(ratio * float64(width))
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return styleAccent.Render(bar)
}
