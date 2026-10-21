// Package cli implements the cpt command line interface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/out"
	"github.com/SuSonicTH/cpt/internal/version"
)

type globals struct {
	Root    string
	JSON    bool
	Yes     bool
	Verbose bool
	Quiet   bool
	NoColor bool
	Offline bool
	Help    bool
	Version bool
}

// silentError is printed by the command itself; Run only uses its exit code.
type silentError struct{ err error }

func (e *silentError) Error() string { return e.err.Error() }
func (e *silentError) Unwrap() error { return e.err }

func silent(err error) error {
	if err == nil {
		return nil
	}
	return &silentError{err: err}
}

// Run executes cpt and returns a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	g, rest, err := parseGlobals(args)
	if err != nil {
		fmt.Fprintf(stderr, "cpt: %v\n\n%s", err, usage)
		return int(cerr.Usage)
	}
	if g.Help || len(rest) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if g.Version || rest[0] == "version" {
		if g.JSON {
			p := out.New(stdout, stderr, true, false, true)
			return exitOn(p.JSONData(map[string]string{
				"version": version.Version,
				"commit":  version.Commit,
				"date":    version.Date,
			}))
		}
		fmt.Fprintln(stdout, version.Full())
		return 0
	}
	if rest[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}

	color := !g.NoColor && os.Getenv("NO_COLOR") == "" && isTTY(stdout)
	a, err := app.Open(app.Options{
		Root:    g.Root,
		Offline: g.Offline,
		Verbose: g.Verbose,
		Quiet:   g.Quiet,
		JSON:    g.JSON,
		NoColor: !color,
		Color:   color,
		Stdout:  stdout,
		Stderr:  stderr,
	})
	if err != nil {
		printError(g.JSON, stdout, stderr, err)
		return cerr.ExitCode(err)
	}
	defer a.Close()

	cmd, cmdArgs := rest[0], rest[1:]
	err = dispatch(ctx, a, cmd, cmdArgs)
	if err != nil {
		var se *silentError
		if !errors.As(err, &se) {
			printError(g.JSON, stdout, stderr, err)
		}
		return cerr.ExitCode(err)
	}
	return 0
}

func printError(jsonMode bool, stdout, stderr io.Writer, err error) {
	if jsonMode {
		p := out.New(stdout, stderr, true, false, true)
		_ = p.JSONError(int(cerr.KindOf(err)), err)
		return
	}
	fmt.Fprintf(stderr, "cpt: %v\n", err)
}

func exitOn(err error) int {
	if err != nil {
		return int(cerr.Generic)
	}
	return 0
}

func dispatch(ctx context.Context, a *app.App, cmd string, args []string) error {
	switch cmd {
	case "list", "ls":
		return cmdList(ctx, a, args)
	case "search":
		return cmdSearch(ctx, a, args)
	case "info", "show":
		return cmdInfo(ctx, a, args)
	case "install", "add":
		return cmdInstall(ctx, a, args)
	case "update", "upgrade":
		return cmdUpdate(ctx, a, args)
	case "remove", "rm", "uninstall":
		return cmdRemove(ctx, a, args)
	case "pin":
		return cmdPin(ctx, a, args)
	case "unpin":
		return cmdUnpin(ctx, a, args)
	case "verify":
		return cmdVerify(ctx, a, args)
	case "doctor":
		return cmdDoctor(ctx, a, args)
	case "lists", "list-sources":
		return cmdLists(ctx, a, args)
	default:
		return cerr.New(cerr.Usage, "unknown command %q; run cpt help", cmd)
	}
}

// parseGlobals extracts global flags from anywhere in the argument list.
func parseGlobals(args []string) (globals, []string, error) {
	var g globals
	var rest []string
	seenCommand := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if seenCommand {
				rest = append(rest, "--")
			}
			rest = append(rest, args[i+1:]...)
			break
		}
		switch {
		case arg == "--root":
			if i+1 >= len(args) {
				return g, nil, fmt.Errorf("--root needs a directory")
			}
			i++
			g.Root = args[i]
		case strings.HasPrefix(arg, "--root="):
			g.Root = strings.TrimPrefix(arg, "--root=")
		case arg == "--json":
			g.JSON = true
		case arg == "--yes" || arg == "-y":
			g.Yes = true
		case arg == "--verbose" || arg == "-v":
			g.Verbose = true
		case arg == "--quiet" || arg == "-q":
			g.Quiet = true
		case arg == "--no-color":
			g.NoColor = true
		case arg == "--offline":
			g.Offline = true
		case arg == "--help" || arg == "-h":
			g.Help = true
		case arg == "--version" || arg == "-V":
			g.Version = true
		default:
			rest = append(rest, arg)
			if !strings.HasPrefix(arg, "-") {
				seenCommand = true
			}
		}
	}
	return g, rest, nil
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
