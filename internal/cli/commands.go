package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/shellinit"
)

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseFlags permutes flags before positional arguments, so that
// "install fzf --force" behaves like "install --force fzf".
func parseFlags(fs *flag.FlagSet, args []string) error {
	var flags, positional []string
	sawDash := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			sawDash = true
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) > 1 && arg[0] == '-' && arg != "-" {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") {
				name := strings.TrimLeft(arg, "-")
				if f := fs.Lookup(name); f != nil {
					if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !bf.IsBoolFlag() {
						if i+1 < len(args) {
							i++
							flags = append(flags, args[i])
						}
					}
				}
			}
			continue
		}
		positional = append(positional, arg)
	}
	combined := append([]string{}, flags...)
	if sawDash {
		combined = append(combined, "--")
	}
	combined = append(combined, positional...)
	return fs.Parse(combined)
}

// autoRefresh refreshes stale lists for read-only commands. Failures are not
// fatal; the cached lists are still usable.
func autoRefresh(ctx context.Context, a *app.App) {
	if a.Offline || !a.ListsStale() {
		return
	}
	if err := a.WithLock(func() error {
		a.MaybeRefresh(ctx, false, true)
		return nil
	}); err != nil {
		a.Log.Debug("list refresh skipped", "error", err)
	}
}

func cmdList(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("list")
	installed := fs.Bool("installed", false, "show installed packages")
	available := fs.Bool("available", false, "show packages that are not installed")
	updates := fs.Bool("updates", false, "show packages with updates")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	autoRefresh(ctx, a)
	rows, err := a.Rows(ctx, app.ListOptions{Installed: *installed, Available: *available, Updates: *updates})
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(rows)
	}
	printRows(a, rows, *updates)
	return nil
}

func cmdSearch(ctx context.Context, a *app.App, args []string) error {
	if len(args) == 0 {
		return cerr.New(cerr.Usage, "search needs a query")
	}
	autoRefresh(ctx, a)
	rows, err := a.Search(ctx, strings.Join(args, " "))
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(rows)
	}
	printRows(a, rows, false)
	return nil
}

func printRows(a *app.App, rows []app.Row, withLatest bool) {
	if len(rows) == 0 {
		a.Out.Infof("no packages found")
		return
	}
	headers := []string{"NAME", "VERSION", "STATUS", "LIST", "DESCRIPTION"}
	if withLatest {
		headers = []string{"NAME", "VERSION", "LATEST", "STATUS", "LIST", "DESCRIPTION"}
	}
	body := make([][]string, 0, len(rows))
	for _, r := range rows {
		status := r.Status
		if r.Pinned {
			status += " *"
		}
		if r.Conflict {
			status = "conflict"
		}
		if r.Error != "" {
			status = "error"
		}
		row := []string{r.Name, r.Version, colorStatus(a, status), r.List, r.Description}
		if withLatest {
			row = []string{r.Name, r.Version, r.Latest, colorStatus(a, status), r.List, r.Description}
		}
		body = append(body, row)
	}
	a.Out.Table(headers, body)
	if withLatest {
		a.Out.Infof("\n* pinned")
	}
	for _, r := range rows {
		if r.Error != "" {
			a.Out.Warnf("%s: %s", r.Name, r.Error)
		}
	}
}

func colorStatus(a *app.App, status string) string {
	switch status {
	case "installed", "ok":
		return a.Out.Symbol("ok", status)
	case "update", "warn", "up-to-date":
		return a.Out.Symbol("warn", status)
	case "conflict", "failed", "error", "missing", "modified", "fail":
		return a.Out.Symbol("error", status)
	default:
		return a.Out.Symbol("dim", status)
	}
}

func cmdInfo(ctx context.Context, a *app.App, args []string) error {
	if len(args) != 1 {
		return cerr.New(cerr.Usage, "info needs exactly one package")
	}
	autoRefresh(ctx, a)
	info, err := a.Detail(ctx, args[0])
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(info)
	}
	a.Out.Line("%s — %s", info.Name, info.Description)
	if info.Homepage != "" {
		a.Out.Line("homepage:  %s", info.Homepage)
	}
	if info.License != "" {
		a.Out.Line("license:   %s", info.License)
	}
	a.Out.Line("list:      %s", info.List)
	if len(info.Tags) > 0 {
		a.Out.Line("tags:      %s", strings.Join(info.Tags, ", "))
	}
	a.Out.Line("platforms: %s", strings.Join(info.Platforms, ", "))
	if info.Installed != nil {
		a.Out.Line("installed: %s (%s)", info.Installed.Version, info.Installed.Checksum)
		if info.Installed.Pinned != "" {
			a.Out.Line("pinned:    %s", info.Installed.Pinned)
		}
		a.Out.Line("bins:      %s", strings.Join(sortedKeys(info.Installed.Bins), ", "))
	} else {
		a.Out.Line("installed: no")
	}
	if info.Shell != nil {
		switch {
		case info.Shell.LastError != "" && info.Shell.SyncPending:
			a.Out.Line("shell:     error: %s (sync pending)", shellinit.DisplayLine(info.Shell.LastError))
		case info.Shell.LastError != "":
			a.Out.Line("shell:     error: %s", shellinit.DisplayLine(info.Shell.LastError))
		case info.Shell.SyncPending:
			if a.Cfg.AllowShellPackages {
				a.Out.Line("shell:     sync pending; cpt will retry")
			} else {
				a.Out.Line("shell:     unsynced; snippets are disabled")
			}
		case info.Shell.Removal:
			live := "live snippet remains active"
			if v := shellinit.VersionLabel(info.Shell.LiveVersion); v != "" {
				live = "live " + v + " remains active"
			}
			a.Out.Line("shell:     removal pending (%s)", live)
		case info.Shell.Pending && info.Shell.Live:
			a.Out.Line("shell:     pending %s (live %s remains active, sha256:%s)",
				shellinit.VersionLabel(info.Shell.PendingVersion), shellinit.VersionLabel(info.Shell.LiveVersion),
				shellinit.ShortHash(info.Shell.PendingSHA256))
		case info.Shell.Pending:
			a.Out.Line("shell:     pending %s (no live snippet, sha256:%s)",
				shellinit.VersionLabel(info.Shell.PendingVersion), shellinit.ShortHash(info.Shell.PendingSHA256))
		case info.Shell.Rejected:
			a.Out.Line("shell:     rejected until the snippet changes")
		case info.Shell.Live:
			a.Out.Line("shell:     approved %s (sha256:%s)", shellinit.VersionLabel(info.Shell.LiveVersion), shellinit.ShortHash(info.Shell.LiveSHA256))
		}
	}
	if info.Latest != nil {
		if info.Latest.Error != "" {
			a.Out.Line("latest:    unavailable (%s)", info.Latest.Error)
		} else {
			a.Out.Line("latest:    %s", info.Latest.Version)
			a.Out.Line("asset:     %s", info.Latest.Asset)
			a.Out.Line("url:       %s", info.Latest.URL)
		}
	} else {
		a.Out.Line("latest:    unknown (offline)")
	}
	return nil
}

func cmdInstall(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("install")
	force := fs.Bool("force", false, "replace conflicting files and reinstall")
	forceSum := fs.Bool("force-checksum", false, "accept a checksum or TOFU hash mismatch")
	dryRun := fs.Bool("dry-run", false, "resolve and show what would happen")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	specs := fs.Args()
	if len(specs) == 0 {
		return cerr.New(cerr.Usage, "install needs at least one package")
	}
	var results []app.Result
	lockErr := a.WithLock(func() error {
		a.MaybeRefresh(ctx, false, false)
		var err error
		results, err = a.Install(ctx, specs, app.InstallOptions{
			Force:           *force,
			ForceChecksum:   *forceSum,
			DryRun:          *dryRun,
			SkipCurrent:     !*force,
			ApproveNewShell: true,
		})
		return err
	})
	return finishResults(a, results, lockErr)
}

func cmdUpdate(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("update")
	force := fs.Bool("force", false, "update even pinned packages")
	dryRun := fs.Bool("dry-run", false, "show what would change")
	fs.Bool("all", false, "update every package (default)")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	var results []app.Result
	lockErr := a.WithLock(func() error {
		a.MaybeRefresh(ctx, false, false)
		var err error
		results, err = a.Update(ctx, fs.Args(), *force, *dryRun)
		return err
	})
	if !a.Out.JSON && len(results) == 0 && lockErr == nil {
		a.Out.Infof("nothing to update")
	}
	return finishResults(a, results, lockErr)
}

func cmdRemove(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("remove")
	force := fs.Bool("force", false, "remove even if binaries were modified")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if len(fs.Args()) == 0 {
		return cerr.New(cerr.Usage, "remove needs at least one package")
	}
	var results []app.Result
	lockErr := a.WithLock(func() error {
		var err error
		results, err = a.Remove(ctx, fs.Args(), *force)
		return err
	})
	return finishResults(a, results, lockErr)
}

// finishResults renders results once, in either text or JSON form, and maps
// failures onto the exit code.
func finishResults(a *app.App, results []app.Result, lockErr error) error {
	resultErr := app.ResultsError(results)
	final := lockErr
	if final == nil {
		final = resultErr
	}
	if a.Out.JSON {
		code := 0
		if final != nil {
			code = int(cerr.KindOf(final))
		}
		if err := a.Out.JSONResult(results, final, code); err != nil {
			return err
		}
		if final != nil {
			return silent(final)
		}
		return nil
	}
	printResults(a, results)
	if lockErr != nil && !sameErr(lockErr, resultErr) {
		return lockErr
	}
	return silent(resultErr)
}

func sameErr(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Error() == b.Error()
}

func cmdPin(ctx context.Context, a *app.App, args []string) error {
	if len(args) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt pin <package>@<version>")
	}
	name, ver := splitAt(args[0])
	if name == "" || ver == "" {
		return cerr.New(cerr.Usage, "usage: cpt pin <package>@<version>")
	}
	if err := a.WithLock(func() error { return a.Pin(name, ver) }); err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(map[string]string{"name": name, "pinned": ver})
	}
	a.Out.Line("pinned %s to %s", name, ver)
	return nil
}

func cmdUnpin(ctx context.Context, a *app.App, args []string) error {
	if len(args) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt unpin <package>")
	}
	if err := a.WithLock(func() error { return a.Unpin(args[0]) }); err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(map[string]string{"name": args[0]})
	}
	a.Out.Line("unpinned %s", args[0])
	return nil
}

func cmdVerify(ctx context.Context, a *app.App, args []string) error {
	autoRefresh(ctx, a)
	results, err := a.Verify(args)
	if err != nil {
		return err
	}
	bad := 0
	for _, r := range results {
		if r.Status != "ok" {
			bad++
		}
	}
	var verifyErr error
	if bad > 0 {
		verifyErr = cerr.New(cerr.Integrity, "%d installed binaries failed verification", bad)
	}
	if a.Out.JSON {
		if err := a.Out.JSONResult(results, verifyErr, int(cerr.KindOf(verifyErr))); err != nil {
			return err
		}
		return silent(verifyErr)
	}
	tw := tabwriter.NewWriter(a.Out.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PACKAGE\tBIN\tSTATUS\tDETAIL")
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Package, r.Bin, colorStatus(a, r.Status), r.Detail)
	}
	_ = tw.Flush()
	return silent(verifyErr)
}

func cmdDoctor(ctx context.Context, a *app.App, args []string) error {
	autoRefresh(ctx, a)
	checks, err := a.Doctor()
	if err != nil {
		return err
	}
	fails := 0
	for _, c := range checks {
		if c.Status == "fail" {
			fails++
		}
	}
	var doctorErr error
	if fails > 0 {
		doctorErr = cerr.New(cerr.Generic, "%d checks failed", fails)
	}
	if a.Out.JSON {
		if err := a.Out.JSONResult(checks, doctorErr, int(cerr.KindOf(doctorErr))); err != nil {
			return err
		}
		return silent(doctorErr)
	}
	tw := tabwriter.NewWriter(a.Out.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tSTATUS\tDETAIL")
	for _, c := range checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, colorStatus(a, c.Status), shellinit.DisplayLine(c.Detail))
	}
	_ = tw.Flush()
	return silent(doctorErr)
}

func printResults(a *app.App, results []app.Result) {
	for _, r := range results {
		switch r.Action {
		case "failed":
			a.Out.Errorf("%v", r.Err)
		case "up-to-date":
			a.Out.Infof("%s %s is up to date", r.Name, r.Version)
		case "skipped (pinned)":
			a.Out.Infof("%s is pinned to %s (use --force to update)", r.Name, r.Version)
		case "would install", "would update":
			a.Out.Line("%s %s %s", r.Action, r.Name, r.Version)
			if r.URL != "" {
				a.Out.Infof("  %s", r.URL)
			}
		default:
			a.Out.Line("%s %s %s", r.Action, r.Name, r.Version)
		}
	}
}

func splitAt(s string) (string, string) {
	if i := strings.LastIndex(s, "@"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cmdLists(ctx context.Context, a *app.App, args []string) error {
	if len(args) == 0 {
		return listsShow(ctx, a)
	}
	switch args[0] {
	case "add":
		return cmdListsAdd(ctx, a, args[1:])
	case "remove", "rm":
		return cmdListsRemove(ctx, a, args[1:])
	case "refresh", "update":
		return cmdListsRefresh(ctx, a, args[1:])
	case "show":
		return cmdListsShow(a, args[1:])
	case "lint":
		return cmdListsLint(ctx, a, args[1:])
	default:
		return cerr.New(cerr.Usage, "unknown lists subcommand %q", args[0])
	}
}

func listsShow(ctx context.Context, a *app.App) error {
	autoRefresh(ctx, a)
	infos := a.Lists()
	if a.Out.JSON {
		return a.Out.JSONData(infos)
	}
	if len(infos) == 0 {
		a.Out.Infof("no lists configured")
		return nil
	}
	tw := tabwriter.NewWriter(a.Out.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPACKAGES\tUPDATED\tORIGIN")
	for _, li := range infos {
		updated := "never"
		if !li.FetchedAt.IsZero() {
			updated = humanAge(li.FetchedAt)
		}
		name := li.Name
		if li.Builtin {
			name += " (builtin)"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", name, li.Packages, updated, li.Origin)
	}
	_ = tw.Flush()
	return nil
}

func cmdListsAdd(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("lists add")
	name := fs.String("name", "", "override the list name")
	replace := fs.Bool("replace", false, "replace an existing list")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if len(fs.Args()) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt lists add <url|owner/repo[#ref[:path]]> [--name NAME]")
	}
	var res app.AddResult
	err := a.WithLock(func() error {
		var err error
		res, err = a.AddList(ctx, fs.Args()[0], app.AddListOptions{Name: *name, Replace: *replace})
		return err
	})
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(res)
	}
	a.Out.Line("added list %s (%d packages)", res.Name, res.Packages)
	a.Out.Infof("  %s", res.Origin)
	return nil
}

func cmdListsRemove(ctx context.Context, a *app.App, args []string) error {
	if len(args) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt lists remove <name>")
	}
	installed := 0
	err := a.WithLock(func() error {
		var err error
		installed, err = a.RemoveList(ctx, args[0])
		return err
	})
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(map[string]any{"name": args[0], "installed_packages": installed})
	}
	a.Out.Line("removed list %s", args[0])
	if installed > 0 {
		a.Out.Infof("%d installed packages came from this list and are still installed", installed)
	}
	return nil
}

func cmdListsRefresh(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("lists refresh")
	force := fs.Bool("force", false, "refresh even when not stale")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	var results []app.RefreshResult
	err := a.WithLock(func() error {
		var err error
		results, err = a.Refresh(ctx, fs.Args(), *force)
		return err
	})
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(results)
	}
	for _, r := range results {
		state := "unchanged"
		if r.Changed {
			state = "updated"
		}
		a.Out.Line("%s: %s (%d packages)", r.Name, state, r.Packages)
	}
	return nil
}

func cmdListsShow(a *app.App, args []string) error {
	if len(args) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt lists show <name>")
	}
	if !list.ValidName(args[0]) {
		return cerr.New(cerr.Usage, "invalid list name %q", args[0])
	}
	data, err := os.ReadFile(filepath.Join(a.Paths.Lists, list.FileName(args[0])))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("unknown list %q", args[0])
		}
		return err
	}
	if !a.Out.JSON {
		if _, err := a.Out.Out.Write(data); err != nil {
			return err
		}
		if len(data) > 0 && data[len(data)-1] != '\n' {
			a.Out.Line("")
		}
		return nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	return a.Out.JSONData(v)
}

func cmdListsLint(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("lists lint")
	network := fs.Bool("network", false, "check asset URLs upstream")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	results, err := a.Lint(ctx, *network)
	if err != nil {
		return err
	}
	bad := 0
	for _, r := range results {
		if r.Error != "" {
			bad++
		}
	}
	var lintErr error
	if bad > 0 {
		lintErr = cerr.New(cerr.Generic, "%d lint checks failed", bad)
	}
	if a.Out.JSON {
		if err := a.Out.JSONResult(results, lintErr, int(cerr.KindOf(lintErr))); err != nil {
			return err
		}
		return silent(lintErr)
	}
	tw := tabwriter.NewWriter(a.Out.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PACKAGE\tPLATFORM\tSTATUS\tDETAIL")
	for _, r := range results {
		status := "ok"
		detail := ""
		if r.Error != "" {
			status = "fail"
			detail = r.Error
		} else if r.Status != 0 {
			detail = fmt.Sprintf("HTTP %d", r.Status)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Package, r.Platform, colorStatus(a, status), detail)
	}
	_ = tw.Flush()
	return silent(lintErr)
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
