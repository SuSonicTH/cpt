// Package logx builds the file+stderr logger used by cpt.
package logx

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// Options controls logger construction.
type Options struct {
	Verbose bool
	Quiet   bool
	Level   string
}

// New opens the log file under dir, falling back to stderr when it cannot be
// created. The returned closer is always non-nil.
func New(dir string, opts Options) (*slog.Logger, io.Closer) {
	level := slog.LevelInfo
	switch opts.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	if opts.Verbose {
		level = slog.LevelDebug
	}

	var writers []io.Writer
	var closer io.Closer = io.NopCloser(nil)
	if !opts.Quiet {
		writers = append(writers, os.Stderr)
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		path := filepath.Join(dir, "cpt.log")
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			writers = append(writers, f)
			closer = f
		}
	}
	if len(writers) == 0 {
		writers = append(writers, io.Discard)
	}
	handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: level})
	return slog.New(handler), closer
}
