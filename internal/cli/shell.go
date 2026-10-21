package cli

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"unicode/utf16"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/fsutil"
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
		changed, err = editRCFile(rc, *install, block)
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

// editRCFile edits a shell startup file atomically, preserving its mode and
// following a symlinked dotfile instead of replacing the link.
func editRCFile(path string, install bool, block string) (bool, error) {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	current, encoding, err := decodeRC(data)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	next := shellinit.RemoveBlock(current)
	if install {
		next = shellinit.AddBlock(current, block)
	}
	if next == current {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := fsutil.WriteFileAtomic(path, encodeRC(next, encoding), mode); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

type rcEncoding int

const (
	rcUTF8 rcEncoding = iota
	rcUTF8BOM
	rcUTF16LE
	rcUTF16BE
)

// decodeRC understands the UTF-16 files Windows PowerShell frequently uses for
// profiles, so editing one does not corrupt it.
func decodeRC(data []byte) (string, rcEncoding, error) {
	switch {
	case len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF:
		return string(data[3:]), rcUTF8BOM, nil
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE:
		s, err := decodeUTF16(data[2:], binary.LittleEndian)
		return s, rcUTF16LE, err
	case len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF:
		s, err := decodeUTF16(data[2:], binary.BigEndian)
		return s, rcUTF16BE, err
	default:
		return string(data), rcUTF8, nil
	}
}

func decodeUTF16(data []byte, order binary.ByteOrder) (string, error) {
	if len(data)%2 != 0 {
		return "", fmt.Errorf("truncated UTF-16 data")
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = order.Uint16(data[i*2:])
	}
	return string(utf16.Decode(units)), nil
}

func encodeRC(s string, encoding rcEncoding) []byte {
	switch encoding {
	case rcUTF8BOM:
		return append([]byte{0xEF, 0xBB, 0xBF}, []byte(s)...)
	case rcUTF16LE:
		return append([]byte{0xFF, 0xFE}, encodeUTF16(s, binary.LittleEndian)...)
	case rcUTF16BE:
		return append([]byte{0xFE, 0xFF}, encodeUTF16(s, binary.BigEndian)...)
	default:
		return []byte(s)
	}
}

func encodeUTF16(s string, order binary.ByteOrder) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, len(units)*2)
	for i, u := range units {
		order.PutUint16(out[i*2:], u)
	}
	return out
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
