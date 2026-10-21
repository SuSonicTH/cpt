package shellinit

import (
	"fmt"
	"strings"
	"unicode"
)

type editKind byte

const (
	editEqual  editKind = ' '
	editDelete editKind = '-'
	editInsert editKind = '+'
)

type edit struct {
	kind    editKind
	text    string
	oldLine int
	newLine int
}

// maxDiffLines bounds the Myers trace for unusually large snippets. Above it a
// whole-block replacement is shown instead of a line diff.
const maxDiffLines = 600

// UnifiedDiff renders a line diff suitable for showing pending shell snippets.
// It returns an empty string when both inputs are identical.
func UnifiedDiff(oldText, newText, oldLabel, newLabel string) string {
	if oldText == newText {
		return ""
	}
	a, b := splitTextLines(oldText), splitTextLines(newText)
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	midA := a[prefix : len(a)-suffix]
	midB := b[prefix : len(b)-suffix]

	var ops []edit
	for _, line := range a[:prefix] {
		ops = append(ops, edit{kind: editEqual, text: line})
	}
	if len(midA) > maxDiffLines || len(midB) > maxDiffLines {
		for _, line := range midA {
			ops = append(ops, edit{kind: editDelete, text: line})
		}
		for _, line := range midB {
			ops = append(ops, edit{kind: editInsert, text: line})
		}
	} else {
		ops = append(ops, diffLines(midA, midB)...)
	}
	for _, line := range a[len(a)-suffix:] {
		ops = append(ops, edit{kind: editEqual, text: line})
	}
	if len(ops) == 0 {
		return ""
	}
	assignLineNumbers(ops)
	const context = 3
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n", oldLabel)
	fmt.Fprintf(&out, "+++ %s\n", newLabel)

	changes := make([]int, 0)
	for i, e := range ops {
		if e.kind != editEqual {
			changes = append(changes, i)
		}
	}
	for i := 0; i < len(changes); {
		start := max(0, changes[i]-context)
		end := min(len(ops), changes[i]+context+1)
		i++
		for i < len(changes) && changes[i]-context <= end {
			end = min(len(ops), changes[i]+context+1)
			i++
		}
		writeHunk(&out, ops, start, end)
	}
	return out.String()
}

func writeHunk(out *strings.Builder, ops []edit, start, end int) {
	oldCount, newCount := 0, 0
	for _, e := range ops[start:end] {
		switch e.kind {
		case editEqual:
			oldCount++
			newCount++
		case editDelete:
			oldCount++
		case editInsert:
			newCount++
		}
	}
	oldStart := ops[start].oldLine
	newStart := ops[start].newLine
	if oldCount == 0 {
		oldStart--
	}
	if newCount == 0 {
		newStart--
	}
	fmt.Fprintf(out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
	for _, e := range ops[start:end] {
		out.WriteByte(byte(e.kind))
		out.WriteString(displaySafe(e.text))
		if !strings.HasSuffix(e.text, "\n") {
			out.WriteString("\n\\ No newline at end of file\n")
		}
	}
}

// DisplaySafe makes control and format characters visible so remote text
// cannot hide or spoof what the user is approving. Newlines and tabs are kept
// because this is used for multi-line diffs.
func DisplaySafe(s string) string { return sanitize(s, true) }

// DisplayLine is DisplaySafe for a single-line status field; layout characters
// are escaped so they cannot break table formatting.
func DisplayLine(s string) string { return sanitize(s, false) }

func displaySafe(s string) string { return DisplaySafe(s) }

func sanitize(s string, allowLayout bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case allowLayout && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\u2028' || r == '\u2029' || unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func splitTextLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines is Myers' algorithm over lines. It produces an edit script from
// the beginning of both inputs.
func diffLines(a, b []string) []edit {
	n, m := len(a), len(b)
	maxD := n + m
	if maxD == 0 {
		return nil
	}
	offset := maxD
	v := make([]int, 2*maxD+1)
	trace := make([][]int, 0, maxD+1)
	for d := 0; d <= maxD; d++ {
		vc := make([]int, len(v))
		copy(vc, v)
		trace = append(trace, vc)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return backtrack(trace, a, b, offset)
			}
		}
	}
	return nil
}

func backtrack(trace [][]int, a, b []string, offset int) []edit {
	x, y := len(a), len(b)
	var rev []edit
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[offset+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			rev = append(rev, edit{kind: editEqual, text: a[x-1]})
			x--
			y--
		}
		if d == 0 {
			break
		}
		if x == prevX {
			rev = append(rev, edit{kind: editInsert, text: b[prevY]})
		} else {
			rev = append(rev, edit{kind: editDelete, text: a[prevX]})
		}
		x, y = prevX, prevY
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	assignLineNumbers(rev)
	return rev
}

func assignLineNumbers(ops []edit) {
	oldLine, newLine := 1, 1
	for i := range ops {
		ops[i].oldLine = oldLine
		ops[i].newLine = newLine
		if ops[i].kind != editInsert {
			oldLine++
		}
		if ops[i].kind != editDelete {
			newLine++
		}
	}
}
