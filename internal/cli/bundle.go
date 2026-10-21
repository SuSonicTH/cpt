package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/bundle"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/install"
	"github.com/SuSonicTH/cpt/internal/out"
)

const bundleUsage = `cpt bundle — build and inspect self-extracting bundles

Usage:
  cpt bundle                             List saved bundles
  cpt bundle list                        List saved bundles
  cpt bundle create <name> [flags]       Build a bundle for a target platform
  cpt bundle rebuild <name>              Rebuild from the saved exact versions
  cpt bundle remove <name>               Delete a saved bundle
  cpt bundle run <file> [flags]          Unpack a bundle without executing it

Create flags:
  --packages a,b,c   Packages to include (default: every installed package)
  --target os/arch   Target platform (default: this host)
  --frozen           Mark the bundled lists read-only on the target
  --base PATH        Base cpt executable (default: this binary, or the release)
  --output PATH      Write the bundle somewhere other than ~/.cpt/bundles
  --force-checksum   Accept a rebuild hash that differs from the manifest

Run flags:
  --dest DIR         Install into DIR instead of ~/.cpt
  --list             Print the bundle contents without installing
  --dry-run          Resolve and report without writing anything
  --force            Replace conflicting or unmanaged files
  --no-init          Do not generate cptinit scripts
  --install-init     Add the cpt marker block to the shell startup file
`

func cmdBundle(ctx context.Context, a *app.App, args []string) error {
	if len(args) == 0 {
		return bundleList(a)
	}
	switch args[0] {
	case "list", "ls", "show":
		return bundleList(a)
	case "create", "build":
		return cmdBundleCreate(ctx, a, args[1:])
	case "rebuild":
		return cmdBundleRebuild(ctx, a, args[1:])
	case "remove", "rm", "delete":
		return cmdBundleRemove(a, args[1:])
	case "run", "install", "unpack":
		return cmdBundleRun(ctx, a, args[1:])
	case "help":
		fmt.Fprint(a.Out.Out, bundleUsage)
		return nil
	default:
		return cerr.New(cerr.Usage, "unknown bundle subcommand %q", args[0])
	}
}

func cmdBundleCreate(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("bundle create")
	packages := fs.String("packages", "", "comma-separated package list")
	target := fs.String("target", "", "target platform os/arch")
	frozen := fs.Bool("frozen", false, "make the bundled lists read-only")
	base := fs.String("base", "", "base cpt executable")
	output := fs.String("output", "", "output path")
	forceSum := fs.Bool("force-checksum", false, "accept a changed rebuild hash")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if len(fs.Args()) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt bundle create <name> [--packages a,b] [--target os/arch]")
	}
	opt := app.BundleOptions{
		Name:          fs.Args()[0],
		Target:        *target,
		Frozen:        *frozen,
		BasePath:      *base,
		Output:        *output,
		ForceChecksum: *forceSum,
		Packages:      splitComma(*packages),
	}
	var res app.BundleBuildResult
	err := a.WithBuildLock(func() error {
		var e error
		res, e = a.BuildBundle(ctx, opt)
		return e
	})
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(res)
	}
	a.Out.Line("built %s for %s", res.Name, res.Target)
	a.Out.Line("  %s", res.Path)
	a.Out.Line("  %d bytes · sha256:%s", res.Size, res.SHA256)
	a.Out.Infof("  packages: %s", strings.Join(res.Packages, ", "))
	for _, w := range res.Warnings {
		a.Out.Warnf("%s", w)
	}
	return nil
}

func cmdBundleRebuild(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("bundle rebuild")
	forceSum := fs.Bool("force-checksum", false, "accept a rebuild hash that differs from the manifest")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if len(fs.Args()) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt bundle rebuild <name> [--force-checksum]")
	}
	var res app.BundleBuildResult
	err := a.WithBuildLock(func() error {
		var e error
		res, e = a.RebuildBundleForced(ctx, fs.Args()[0], *forceSum)
		return e
	})
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(res)
	}
	a.Out.Line("rebuilt %s for %s", res.Name, res.Target)
	a.Out.Line("  %s", res.Path)
	a.Out.Line("  %d bytes · sha256:%s", res.Size, res.SHA256)
	for _, w := range res.Warnings {
		a.Out.Warnf("%s", w)
	}
	return nil
}

func cmdBundleRemove(a *app.App, args []string) error {
	fs := newFlagSet("bundle remove")
	force := fs.Bool("force", false, "remove a manifest-backed file even if its footer is damaged")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if len(fs.Args()) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt bundle remove <name> [--force]")
	}
	name := fs.Args()[0]
	var path string
	err := a.WithLock(func() error {
		var e error
		path, e = a.RemoveBundleForced(name, *force)
		return e
	})
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(map[string]string{"name": name, "removed": path})
	}
	a.Out.Line("removed bundle %s", name)
	return nil
}

func bundleList(a *app.App) error {
	infos, err := a.Bundles()
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(infos)
	}
	if len(infos) == 0 {
		a.Out.Infof("no bundles built yet")
		return nil
	}
	tw := tabwriter.NewWriter(a.Out.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTARGET\tPACKAGES\tSIZE\tCREATED\tKIND")
	for _, b := range infos {
		kind := "live"
		if b.Frozen {
			kind = "frozen"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n",
			b.Name, b.Target, b.Packages, humanSize(b.Size), humanAge(b.Created), kind)
	}
	_ = tw.Flush()
	return nil
}

func cmdBundleRun(ctx context.Context, a *app.App, args []string) error {
	opt, jsonMode, positional, err := parseRunFlags(args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt bundle run <file> [--dest DIR]")
	}
	jsonMode = jsonMode || a.Out.JSON
	res, err := bundle.Run(ctx, positional[0], opt)
	if err != nil {
		if jsonMode {
			if encErr := a.Out.JSONResult(res, err, int(cerr.KindOf(err))); encErr != nil {
				return encErr
			}
			return silent(err)
		}
		if res.Dest != "" {
			printRunBody(a.Out, res)
		}
		return err
	}
	printRunResult(a.Out, res, jsonMode)
	return nil
}

func parseRunFlags(args []string) (bundle.RunOptions, bool, []string, error) {
	fs := flag.NewFlagSet("bundle run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opt bundle.RunOptions
	fs.StringVar(&opt.Dest, "dest", "", "destination root")
	fs.BoolVar(&opt.List, "list", false, "list contents")
	fs.BoolVar(&opt.DryRun, "dry-run", false, "do not write")
	fs.BoolVar(&opt.Force, "force", false, "replace conflicting files")
	fs.BoolVar(&opt.NoInit, "no-init", false, "skip cptinit")
	fs.BoolVar(&opt.InstallInit, "install-init", false, "edit the shell startup file")
	jsonMode := fs.Bool("json", false, "JSON output")
	if err := parseFlags(fs, args); err != nil {
		return opt, false, nil, cerr.New(cerr.Usage, "%v", err)
	}
	return opt, *jsonMode, fs.Args(), nil
}

func printRunResult(p *out.Printer, res bundle.RunResult, jsonMode bool) {
	if jsonMode {
		_ = p.JSONData(res)
		return
	}
	if res.DryRun {
		p.Line("bundle %s targets %s (%s)", res.Name, res.Target, frozenLabel(res.Frozen))
		for _, r := range res.Packages {
			p.Line("  %s %s %s", r.Action, r.Name, r.Version)
		}
		return
	}
	if res.Dest == "" {
		// --list mode.
		p.Line("bundle %s targets %s (%s)", res.Name, res.Target, frozenLabel(res.Frozen))
		for _, r := range res.Packages {
			p.Line("  %s %s", r.Name, r.Version)
		}
		return
	}
	p.Line("installed bundle %s into %s (%s)", res.Name, res.Dest, frozenLabel(res.Frozen))
	printRunBody(p, res)
	if res.Init != "" {
		p.Infof("cptinit written to %s", res.Init)
		p.Infof("run: source %q", res.Init)
	}
	if res.Cpt != "" {
		p.Infof("cpt installed at %s", res.Cpt)
	}
}

func printRunBody(p *out.Printer, res bundle.RunResult) {
	for _, r := range res.Packages {
		p.Line("  %s %s %s", r.Action, r.Name, r.Version)
	}
	for _, w := range res.Warnings {
		p.Warnf("%s", w)
	}
}

func frozenLabel(frozen bool) string {
	if frozen {
		return "frozen"
	}
	return "live"
}

// runBundleMode handles a self-extracting bundle executable. It runs before
// normal command dispatch because a bundle's arguments describe the install,
// not a cpt subcommand. Global flags have already been consumed by parseGlobals.
func runBundleMode(ctx context.Context, self string, args []string, g globals, stdout, stderr io.Writer) int {
	if g.Help || hasHelp(args) {
		fmt.Fprint(stdout, bundleUsage)
		return 0
	}
	opt, jsonMode, positional, err := parseRunFlags(args)
	jsonMode = jsonMode || g.JSON
	if err != nil {
		return bundleModeError(stdout, stderr, jsonMode, err)
	}
	if len(positional) != 0 {
		return bundleModeError(stdout, stderr, jsonMode,
			cerr.New(cerr.Usage, "unexpected argument %q", positional[0]))
	}
	if opt.Dest == "" && g.Root != "" {
		opt.Dest = g.Root
	}
	res, err := bundle.Run(ctx, self, opt)
	if err != nil {
		if jsonMode {
			p := out.New(stdout, stderr, true, false, true)
			_ = p.JSONResult(res, err, int(cerr.KindOf(err)))
		} else {
			if res.Dest != "" {
				printRunBody(out.New(stdout, stderr, false, false, false), res)
			}
			fmt.Fprintf(stderr, "cpt: %v\n", err)
		}
		return int(cerr.KindOf(err))
	}
	p := out.New(stdout, stderr, jsonMode, false, false)
	printRunResult(p, res, jsonMode)
	return 0
}

func bundleModeError(stdout, stderr io.Writer, jsonMode bool, err error) int {
	if jsonMode {
		p := out.New(stdout, stderr, true, false, true)
		_ = p.JSONError(int(cerr.KindOf(err)), err)
	} else {
		fmt.Fprintf(stderr, "cpt: %v\n", err)
	}
	return int(cerr.KindOf(err))
}

// runStagedSelf runs a clean cpt.new.exe staged by a Windows bundle install
// that could not replace its own running image. The child gets the original
// arguments and its exit code is propagated, so every normal command works even
// while the installer image is still on disk.
func runStagedSelf(self string, args []string, stdout, stderr io.Writer) (int, bool) {
	pending := install.PendingPath(self)
	if _, err := os.Stat(pending); err != nil {
		legacy := self + ".new"
		if _, legacyErr := os.Stat(legacy); legacyErr != nil {
			return 0, false
		}
		pending = legacy
	}
	if bundle.IsBundle(pending) {
		return 0, false
	}
	cmd := exec.Command(pending, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), true
		}
		fmt.Fprintf(stderr, "cpt: %v\n", err)
		return int(cerr.Generic), true
	}
	return 0, true
}

// looksLikeBundleInvocation catches arguments that only make sense when the
// executable is a self-extracting bundle. If those appear on a binary whose
// footer is missing or damaged, report corruption instead of "unknown command".
func looksLikeBundleInvocation(args []string) bool {
	if len(args) == 0 || !strings.HasPrefix(args[0], "-") {
		// A real cpt subcommand ("install --dry-run") is not a bundle call.
		return false
	}
	for _, a := range args {
		switch {
		case a == "--dest", a == "--list", a == "--dry-run", a == "--force",
			a == "--no-init", a == "--install-init", a == "--json":
			return true
		case strings.HasPrefix(a, "--dest="):
			return true
		}
	}
	return false
}

func hasHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

func humanSize(n int64) string {
	switch {
	case n <= 0:
		return "—"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	}
}

func splitComma(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
