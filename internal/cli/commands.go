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
)

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func cmdList(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("list")
	installed := fs.Bool("installed", false, "show installed packages")
	available := fs.Bool("available", false, "show packages that are not installed")
	updates := fs.Bool("updates", false, "show packages with updates")
	if err := fs.Parse(args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
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
}

func colorStatus(a *app.App, status string) string {
	switch status {
	case "installed":
		return a.Out.Symbol("ok", status)
	case "update":
		return a.Out.Symbol("warn", status)
	case "conflict", "failed", "missing", "modified":
		return a.Out.Symbol("error", status)
	default:
		return a.Out.Symbol("dim", status)
	}
}

func cmdInfo(ctx context.Context, a *app.App, args []string) error {
	if len(args) != 1 {
		return cerr.New(cerr.Usage, "info needs exactly one package")
	}
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
	force := fs.Bool("force", false, "replace conflicting files")
	forceSum := fs.Bool("force-checksum", false, "accept a checksum or TOFU hash mismatch")
	dryRun := fs.Bool("dry-run", false, "resolve and show what would happen")
	if err := fs.Parse(args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	specs := fs.Args()
	if len(specs) == 0 {
		return cerr.New(cerr.Usage, "install needs at least one package")
	}
	var results []app.Result
	err := a.WithLock(func() error {
		a.MaybeRefresh(ctx, false)
		var err error
		results, err = a.Install(ctx, specs, app.InstallOptions{Force: *force, ForceChecksum: *forceSum, DryRun: *dryRun})
		return err
	})
	printResults(a, results)
	if err != nil {
		if len(results) == 0 {
			return err
		}
		return silent(err)
	}
	if a.Out.JSON {
		if err := a.Out.JSONData(results); err != nil {
			return err
		}
	}
	return silent(app.ResultsError(results))
}

func cmdUpdate(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("update")
	force := fs.Bool("force", false, "update even pinned packages")
	dryRun := fs.Bool("dry-run", false, "show what would change")
	fs.Bool("all", false, "update every package (default)")
	if err := fs.Parse(args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	var results []app.Result
	err := a.WithLock(func() error {
		a.MaybeRefresh(ctx, false)
		var err error
		results, err = a.Update(ctx, fs.Args(), *force, *dryRun)
		return err
	})
	printResults(a, results)
	if len(results) == 0 && !a.Out.JSON {
		a.Out.Infof("nothing to update")
	}
	if err != nil {
		if len(results) == 0 {
			return err
		}
		return silent(err)
	}
	if a.Out.JSON {
		if err := a.Out.JSONData(results); err != nil {
			return err
		}
	}
	return silent(app.ResultsError(results))
}

func cmdRemove(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("remove")
	force := fs.Bool("force", false, "remove even if binaries were modified")
	if err := fs.Parse(args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if len(fs.Args()) == 0 {
		return cerr.New(cerr.Usage, "remove needs at least one package")
	}
	var results []app.Result
	err := a.WithLock(func() error {
		var err error
		results, err = a.Remove(ctx, fs.Args(), *force)
		return err
	})
	printResults(a, results)
	if err != nil {
		if len(results) == 0 {
			return err
		}
		return silent(err)
	}
	if a.Out.JSON {
		if err := a.Out.JSONData(results); err != nil {
			return err
		}
	}
	return silent(app.ResultsError(results))
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
	results, err := a.Verify(args)
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(results)
	}
	bad := 0
	tw := tabwriter.NewWriter(a.Out.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PACKAGE\tBIN\tSTATUS\tDETAIL")
	for _, r := range results {
		if r.Status != "ok" {
			bad++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Package, r.Bin, colorStatus(a, r.Status), r.Detail)
	}
	_ = tw.Flush()
	if bad > 0 {
		return silent(cerr.New(cerr.Integrity, "%d installed binaries failed verification", bad))
	}
	return nil
}

func cmdDoctor(ctx context.Context, a *app.App, args []string) error {
	checks, err := a.Doctor()
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(checks)
	}
	fails := 0
	tw := tabwriter.NewWriter(a.Out.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tSTATUS\tDETAIL")
	for _, c := range checks {
		if c.Status == "fail" {
			fails++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, colorStatus(a, c.Status), c.Detail)
	}
	_ = tw.Flush()
	if fails > 0 {
		return silent(cerr.New(cerr.Generic, "%d checks failed", fails))
	}
	return nil
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
		return listsShow(a)
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

func listsShow(a *app.App) error {
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
	if err := fs.Parse(args); err != nil {
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
		installed, err = a.RemoveList(args[0])
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
	if err := fs.Parse(args); err != nil {
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
	data, err := os.ReadFile(filepath.Join(a.Paths.Lists, args[0]+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("unknown list %q", args[0])
		}
		return err
	}
	if !a.Out.JSON {
		a.Out.Out.Write(data)
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
	if err := fs.Parse(args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	results, err := a.Lint(ctx, *network)
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(results)
	}
	bad := 0
	tw := tabwriter.NewWriter(a.Out.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PACKAGE\tPLATFORM\tSTATUS\tDETAIL")
	for _, r := range results {
		status := "ok"
		detail := ""
		if r.Error != "" {
			status = "fail"
			detail = r.Error
			bad++
		} else if r.Status != 0 {
			detail = fmt.Sprintf("HTTP %d", r.Status)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Package, r.Platform, colorStatus(a, status), detail)
	}
	_ = tw.Flush()
	if bad > 0 {
		return silent(cerr.New(cerr.Generic, "%d lint checks failed", bad))
	}
	return nil
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
