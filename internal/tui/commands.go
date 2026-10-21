package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/progress"
)

type snapshotMsg struct {
	rows    []app.Row
	lists   []app.ListInfo
	shells  []app.ShellInfo
	pending int
	err     error
	initial bool
}

type logMsg struct {
	lines []string
	err   error
}

type eventMsg progress.Event

type opDoneMsg struct {
	op      string
	results []app.Result
	err     error
}

type actionDoneMsg struct {
	status string
	err    error
}

type tickMsg time.Time

type clearStatusMsg struct {
	seq int
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func waitEventCmd(ch <-chan progress.Event, done <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-done:
			return nil
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			return eventMsg(e)
		}
	}
}

// snapshot reads the package and list views. It must only run when no other
// command is mutating the app.
func snapshot(a *app.App, ctx context.Context, probe bool) snapshotMsg {
	rows, err := a.Rows(ctx, app.ListOptions{Updates: probe})
	return snapshotMsg{
		rows:    rows,
		lists:   a.Lists(),
		shells:  a.ShellInfos(),
		pending: a.PendingShellCount(),
		err:     err,
	}
}

func startupCmd(ctx context.Context, a *app.App) tea.Cmd {
	return func() tea.Msg {
		snap := snapshot(a, ctx, false)
		snap.initial = true
		return snap
	}
}

func reloadCmd(ctx context.Context, a *app.App, refresh bool) tea.Cmd {
	return func() tea.Msg {
		if refresh && !a.Offline {
			_ = a.WithLock(func() error {
				a.MaybeRefresh(ctx, false, true)
				return nil
			})
		}
		return snapshot(a, ctx, true)
	}
}

func readLogCmd(logDir string) tea.Cmd {
	return func() tea.Msg {
		path := filepath.Join(logDir, "cpt.log")
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return logMsg{}
			}
			return logMsg{err: err}
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return logMsg{err: err}
		}
		const window = 256 << 10
		start := info.Size() - window
		if start < 0 {
			start = 0
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return logMsg{err: err}
		}
		data, err := io.ReadAll(io.LimitReader(f, window))
		if err != nil {
			return logMsg{err: err}
		}
		text := string(data)
		if start > 0 {
			if i := strings.IndexByte(text, '\n'); i >= 0 {
				text = text[i+1:]
			}
		}
		text = strings.TrimRight(text, "\r\n")
		var lines []string
		if text != "" {
			lines = strings.Split(text, "\n")
			for i := range lines {
				lines[i] = strings.TrimRight(lines[i], "\r")
			}
		}
		const maxLines = 2000
		if len(lines) > maxLines {
			lines = lines[len(lines)-maxLines:]
		}
		return logMsg{lines: lines}
	}
}

func operationCmd(ctx context.Context, a *app.App, op string, names []string, force bool) tea.Cmd {
	return func() tea.Msg {
		var results []app.Result
		var opErr error
		lockErr := a.WithLock(func() error {
			switch op {
			case "install":
				results, opErr = a.Install(ctx, names, app.InstallOptions{
					SkipCurrent:     true,
					ApproveNewShell: true,
				})
			case "update":
				results, opErr = a.Update(ctx, names, false, false)
			case "remove":
				results, opErr = a.Remove(ctx, names, force)
			default:
				opErr = fmt.Errorf("unknown operation %q", op)
			}
			return opErr
		})
		if lockErr != nil {
			opErr = lockErr
		}
		return opDoneMsg{op: op, results: results, err: opErr}
	}
}

func pinCmd(ctx context.Context, a *app.App, name, version string, unpin bool) tea.Cmd {
	return func() tea.Msg {
		err := a.WithLock(func() error {
			if unpin {
				return a.Unpin(name)
			}
			return a.Pin(name, version)
		})
		if err != nil {
			return actionDoneMsg{err: err}
		}
		if unpin {
			return actionDoneMsg{status: "unpinned " + name}
		}
		return actionDoneMsg{status: fmt.Sprintf("pinned %s to %s", name, version)}
	}
}

func refreshListsCmd(ctx context.Context, a *app.App, names []string, force bool) tea.Cmd {
	return func() tea.Msg {
		var results []app.RefreshResult
		err := a.WithLock(func() error {
			var e error
			results, e = a.Refresh(ctx, names, force)
			return e
		})
		if err != nil {
			return actionDoneMsg{err: err}
		}
		changed := 0
		for _, r := range results {
			if r.Changed {
				changed++
			}
		}
		return actionDoneMsg{status: fmt.Sprintf("refreshed %d list(s), %d changed", len(results), changed)}
	}
}

func addListCmd(ctx context.Context, a *app.App, spec string) tea.Cmd {
	return func() tea.Msg {
		var res app.AddResult
		err := a.WithLock(func() error {
			var e error
			res, e = a.AddList(ctx, spec, app.AddListOptions{})
			return e
		})
		if err != nil {
			return actionDoneMsg{err: err}
		}
		return actionDoneMsg{status: fmt.Sprintf("added list %s (%d packages)", res.Name, res.Packages)}
	}
}

func removeListCmd(ctx context.Context, a *app.App, name string) tea.Cmd {
	return func() tea.Msg {
		var installed int
		err := a.WithLock(func() error {
			var e error
			installed, e = a.RemoveList(ctx, name)
			return e
		})
		if err != nil {
			return actionDoneMsg{err: err}
		}
		status := "removed list " + name
		if installed > 0 {
			status += fmt.Sprintf(" (%d installed packages kept)", installed)
		}
		return actionDoneMsg{status: status}
	}
}

func shellDecisionCmd(ctx context.Context, a *app.App, names []string, all, approve bool) tea.Cmd {
	return func() tea.Msg {
		var selected []string
		err := a.WithLock(func() error {
			var e error
			if approve {
				selected, e = a.ApproveShell(names, all)
			} else {
				selected, e = a.RejectShell(names, all)
			}
			return e
		})
		if err != nil {
			return actionDoneMsg{err: err}
		}
		verb := "approved"
		if !approve {
			verb = "rejected"
		}
		return actionDoneMsg{status: fmt.Sprintf("%s %d shell snippet(s)", verb, len(selected))}
	}
}
