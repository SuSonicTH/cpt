package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/shellinit"
)

func cmdInit(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("init")
	install := fs.Bool("install", false, "add a marker block to the shell startup file")
	uninstall := fs.Bool("uninstall", false, "remove the marker block from the shell startup file")
	file := fs.String("file", "", "startup file to edit instead of the shell default")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if *install && *uninstall {
		return cerr.New(cerr.Usage, "--install and --uninstall are mutually exclusive")
	}
	if len(fs.Args()) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt init <bash|zsh|pwsh|powershell> [--install|--uninstall] [--file PATH]")
	}
	shell, err := shellinit.ParsedShell(fs.Args()[0])
	if err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if *file != "" && !*install && !*uninstall {
		return cerr.New(cerr.Usage, "--file requires --install or --uninstall")
	}

	if !*install && !*uninstall {
		warnShellPending(a)
		script := a.InitScript(shell.Flavor())
		if a.Out.JSON {
			return a.Out.JSONData(map[string]string{"shell": shell.String(), "script": script})
		}
		fmt.Fprint(a.Out.Out, script)
		return nil
	}

	home, homeErr := shellinit.HomeDir()
	rc := *file
	if rc == "" {
		if homeErr != nil {
			return cerr.New(cerr.Usage, "%v; pass --file to choose a startup file", homeErr)
		}
		if filepath.Clean(a.Paths.Root) != filepath.Join(filepath.Clean(home), ".cpt") {
			return cerr.New(cerr.Usage,
				"--root %s does not use the home directory layout; pass --file to choose the startup file", a.Paths.Root)
		}
		var err error
		rc, err = shellinit.ProfilePath(ctx, shell, home)
		if err != nil {
			return err
		}
	} else {
		if home == "" && (rc == "~" || strings.HasPrefix(rc, "~/") || strings.HasPrefix(rc, `~\`)) {
			return cerr.New(cerr.Usage, "cannot expand ~ without a home directory; pass an absolute path")
		}
		rc = shellinit.ExpandPath(rc, home)
	}
	block := shellinit.MarkerBlock(shell, a.InitScriptPath(shell.Flavor()), a.ShellInitOptions())
	var changed bool
	if err := a.WithLock(func() error {
		if *install {
			if err := a.RegenerateInit(); err != nil {
				return err
			}
		}
		var err error
		changed, err = shellinit.EditRCFile(rc, *install, block)
		return err
	}); err != nil {
		return err
	}

	result := map[string]any{
		"shell":     shell.String(),
		"file":      rc,
		"installed": *install,
		"changed":   changed,
	}
	if a.Out.JSON {
		return a.Out.JSONData(result)
	}
	switch {
	case *install && changed:
		a.Out.Infof("installed cpt init block in %s", rc)
		a.Out.Infof("open a new shell to load it")
	case *install:
		a.Out.Infof("cpt init block is already installed in %s", rc)
	case changed:
		a.Out.Infof("removed cpt init block from %s", rc)
	default:
		a.Out.Infof("no cpt init block found in %s", rc)
	}
	warnShellPending(a)
	return nil
}

func warnShellPending(a *app.App) {
	if n := a.PendingShellCount(); n > 0 {
		a.Out.Warnf("%d shell snippet change(s) await approval; run cpt shell", n)
		return
	}
	if n := a.SyncPendingShellCount(); n > 0 {
		a.Out.Warnf("%d shell snippet sync(s) pending; cpt will retry or run cpt lists refresh", n)
	}
}

func cmdShell(ctx context.Context, a *app.App, args []string) error {
	if len(args) == 0 {
		return shellList(a)
	}
	switch args[0] {
	case "list", "status", "ls":
		return shellList(a)
	case "diff":
		return shellDiff(a, args[1:])
	case "approve":
		return shellApprove(a, args[1:], true)
	case "reject":
		return shellApprove(a, args[1:], false)
	case "reset", "forget":
		return shellReset(ctx, a, args[1:])
	case "help":
		fmt.Fprint(a.Out.Out, shellUsage)
		return nil
	default:
		return cerr.New(cerr.Usage, "unknown shell subcommand %q", args[0])
	}
}

func shellList(a *app.App) error {
	infos := a.ShellInfos()
	if a.Out.JSON {
		return a.Out.JSONData(map[string]any{
			"enabled":  a.Cfg.AllowShellPackages,
			"snippets": infos,
		})
	}
	if !a.Cfg.AllowShellPackages {
		a.Out.Infof("shell packages are disabled by allow_shell_packages=false")
	}
	if len(infos) == 0 {
		a.Out.Infof("no shell snippets installed")
		return nil
	}
	tw := tabwriter.NewWriter(a.Out.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PACKAGE\tSTATUS\tLIVE\tPENDING\tDETAIL")
	for _, info := range infos {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", info.Package, shellStatus(info),
			shellRef(info.LiveVersion, info.LiveSHA256), shellRef(info.PendingVersion, info.PendingSHA256), shellDetail(a, info))
	}
	_ = tw.Flush()
	if n := a.PendingShellCount(); n > 0 {
		a.Out.Infof("%d snippet change(s) await approval: cpt shell diff <package>, then cpt shell approve <package>", n)
	}
	return nil
}

func shellRef(version, hash string) string {
	short := shellinit.ShortHash(hash)
	switch {
	case version == "":
		return short
	case short == "":
		return shellinit.VersionLabel(version)
	default:
		return shellinit.VersionLabel(version) + " " + short
	}
}

func shellStatus(info app.ShellInfo) string {
	switch {
	case info.LastError != "":
		return "error"
	case info.SyncPending:
		return "unsynced"
	case info.Removal:
		return "removal"
	case info.Pending:
		return "pending"
	case info.Rejected:
		return "rejected"
	case info.Live:
		return "approved"
	default:
		return "pending"
	}
}

func shellDiff(a *app.App, args []string) error {
	if len(args) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt shell diff <package>")
	}
	diff, err := a.ShellDiff(args[0])
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(map[string]string{"package": args[0], "diff": diff})
	}
	if diff == "" {
		a.Out.Infof("no pending shell snippet for %s", args[0])
		return nil
	}
	fmt.Fprint(a.Out.Out, diff)
	return nil
}

func shellReset(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("shell reset")
	all := fs.Bool("all", false, "reset every rejected snippet")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if *all && len(fs.Args()) > 0 {
		return cerr.New(cerr.Usage, "--all does not take package names")
	}
	var names []string
	if err := a.WithLock(func() error {
		var err error
		names, err = a.ResetShellRejection(ctx, fs.Args(), *all)
		return err
	}); err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(map[string]any{"action": "reset", "packages": names})
	}
	if len(names) == 0 {
		a.Out.Infof("no rejected shell snippets")
	}
	for _, name := range names {
		a.Out.Line("reset shell rejection for %s", name)
	}
	if !a.Cfg.AllowShellPackages {
		a.Out.Infof("shell packages are disabled; changes will sync when re-enabled")
	} else if n := a.PendingShellCount(); n > 0 {
		a.Out.Infof("%d snippet change(s) queued; run cpt shell diff <package>", n)
	}
	return nil
}

func shellApprove(a *app.App, args []string, approve bool) error {
	name := "shell reject"
	if approve {
		name = "shell approve"
	}
	fs := newFlagSet(name)
	all := fs.Bool("all", false, "apply to every pending snippet")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if *all && len(fs.Args()) > 0 {
		return cerr.New(cerr.Usage, "--all does not take package names")
	}
	var names []string
	err := a.WithLock(func() error {
		var err error
		if approve {
			names, err = a.ApproveShell(fs.Args(), *all)
		} else {
			names, err = a.RejectShell(fs.Args(), *all)
		}
		return err
	})
	if err != nil {
		return err
	}
	action := "rejected"
	if approve {
		action = "approved"
	}
	if a.Out.JSON {
		return a.Out.JSONData(map[string]any{"action": action, "packages": names})
	}
	if len(names) == 0 {
		a.Out.Infof("no pending shell snippets")
		return nil
	}
	for _, name := range names {
		a.Out.Line("%s shell snippet for %s", action, name)
	}
	if approve {
		a.Out.Infof("cptinit updated; open a new shell or add it with cpt init <shell> --install (custom init files are left untouched)")
	} else {
		a.Out.Infof("live snippets are unchanged; cpt will not offer this change again")
	}
	return nil
}

func shellDetail(a *app.App, info app.ShellInfo) string {
	switch {
	case info.LastError != "" && info.SyncPending:
		return shellinit.DisplayLine(info.LastError) + " (sync pending)"
	case info.LastError != "":
		return shellinit.DisplayLine(info.LastError)
	case info.SyncPending && a.Cfg.AllowShellPackages:
		return "sync pending; cpt will retry"
	case info.SyncPending:
		return "will sync when shell packages are re-enabled"
	case info.Removal:
		return "list no longer declares a snippet"
	case info.Rejected:
		return "rejected until the snippet changes"
	case info.Pending:
		return "run: cpt shell diff " + info.Package
	case !info.Declared && info.Live:
		return "not declared by current lists; run cpt lists refresh"
	default:
		return ""
	}
}
