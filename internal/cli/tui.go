package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/progress"
	"github.com/SuSonicTH/cpt/internal/tui"
)

const nonTTYMessage = `cpt: interactive mode needs a terminal; showing command help instead.
Use a subcommand such as "cpt list" or "cpt install <package>", or run "cpt" in a terminal for the TUI.`

const noTUIMessage = `cpt: interactive mode disabled by --no-tui; showing command help instead.
Remove the flag or run a subcommand such as "cpt list".`

// uiWriter turns console output into progress bus notices so that a package
// operation can never scribble over the alternate screen.
type uiWriter struct {
	bus *progress.Bus

	mu  sync.Mutex
	buf []byte
}

func (w *uiWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		if line != "" {
			w.bus.Publish(progress.Event{Kind: progress.KindInfo, Message: line})
		}
	}
	// Never let an unterminated line grow without bound.
	if len(w.buf) > 8192 {
		w.bus.Publish(progress.Event{Kind: progress.KindInfo, Message: string(w.buf)})
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// runTUI opens the app in interactive mode and hands the terminal to Bubble Tea.
func runTUI(ctx context.Context, g globals, stdout, stderr io.Writer) int {
	bus := progress.New()
	defer bus.Close()
	console := &uiWriter{bus: bus}
	a, err := app.Open(app.Options{
		Context:     ctx,
		Root:        g.Root,
		Offline:     g.Offline,
		Verbose:     g.Verbose,
		Interactive: true,
		NoColor:     true,
		Stdout:      console,
		Stderr:      console,
		Events:      bus,
	})
	if err != nil {
		printError(false, stdout, stderr, err)
		return cerr.ExitCode(err)
	}
	defer a.Close()

	if err := tui.Run(ctx, a); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "cpt: interactive mode: %v\n", err)
		return int(cerr.Generic)
	}
	return 0
}
