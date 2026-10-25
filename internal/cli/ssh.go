package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/sshtarget"
)

const sshUsage = `cpt ssh — copy a bundle to a remote host and open an interactive session

Usage:
  cpt ssh                              List saved SSH targets
  cpt ssh list [--json]                List saved SSH targets
  cpt ssh <target>                     Connect to a target
  cpt ssh connect <target>             Connect to a target
  cpt ssh add <name> [flags]           Create a target
  cpt ssh edit <name> [flags]          Edit a target
  cpt ssh remove <name>                Delete a local target

Target flags:
  --bundle NAME        Existing bundle to copy and run
  --config-host ALIAS  Use an alias from ~/.ssh/config
  --host HOST          Use an explicit hostname or IP
  --user USER          SSH user override
  --port PORT          SSH port
  --identity PATH      Identity file
  --proxy-jump SPEC    ProxyJump destination
  --remote-root PATH   Remote cpt root (default ~/.cpt)
  --shell SHELL        auto, bash, zsh or sh (default auto)
  --cleanup            Remove the remote root after the session
  --no-cleanup         Keep the remote root after the session
  --force              Pass --force to the remote bundle
  --no-force           Do not pass --force

On connect the bundle is copied only when it is missing, older than 24 hours,
changed, or the target's connection settings changed. A fresh copy skips both
the upload and the bundle run and starts the shell directly. Remote startup files
are never edited; cptinit is loaded only for this session.
`

type sshTargetFlags struct {
	bundle     *string
	configHost *string
	host       *string
	user       *string
	port       *int
	identity   *string
	proxyJump  *string
	remoteRoot *string
	shell      *string
	cleanup    *bool
	noCleanup  *bool
	force      *bool
	noForce    *bool
}

func newSSHTargetFlags(name string) (*flag.FlagSet, *sshTargetFlags) {
	fs := newFlagSet(name)
	f := &sshTargetFlags{
		bundle:     fs.String("bundle", "", "bundle name"),
		configHost: fs.String("config-host", "", "ssh config alias"),
		host:       fs.String("host", "", "explicit hostname or IP"),
		user:       fs.String("user", "", "ssh user"),
		port:       fs.Int("port", 0, "ssh port"),
		identity:   fs.String("identity", "", "identity file"),
		proxyJump:  fs.String("proxy-jump", "", "ProxyJump destination"),
		remoteRoot: fs.String("remote-root", "", "remote cpt root"),
		shell:      fs.String("shell", sshtarget.ShellAuto, "remote shell"),
		cleanup:    fs.Bool("cleanup", false, "remove the remote root after the session"),
		noCleanup:  fs.Bool("no-cleanup", false, "keep the remote root after the session"),
		force:      fs.Bool("force", false, "pass --force to the remote bundle"),
		noForce:    fs.Bool("no-force", false, "do not pass --force"),
	}
	return fs, f
}

func visitedFlags(fs *flag.FlagSet) map[string]bool {
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	return seen
}

func cmdSSH(ctx context.Context, a *app.App, args []string) error {
	if len(args) == 0 {
		return cmdSSHList(ctx, a, nil)
	}
	switch args[0] {
	case "list", "ls":
		return cmdSSHList(ctx, a, args[1:])
	case "add", "create":
		return cmdSSHAdd(ctx, a, args[1:])
	case "edit", "update":
		return cmdSSHEdit(ctx, a, args[1:])
	case "remove", "rm", "delete":
		return cmdSSHRemove(ctx, a, args[1:])
	case "connect":
		return cmdSSHConnect(ctx, a, args[1:])
	case "help":
		fmt.Fprint(a.Out.Out, sshUsage)
		return nil
	default:
		// The short form is `cpt ssh <target>`. Only fall back to connect when
		// the argument really names a saved target; otherwise report the
		// unknown subcommand, as every other command group does. A load failure
		// is reported as itself rather than as an unknown name.
		if _, err := a.SSHTarget(args[0]); err == nil {
			return cmdSSHConnect(ctx, a, args)
		} else if cerr.KindOf(err) != cerr.Usage {
			return err
		}
		return cerr.New(cerr.Usage, "unknown ssh subcommand %q (or unknown SSH target; see `cpt ssh list`)", args[0])
	}
}

func cmdSSHList(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("ssh list")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if len(fs.Args()) != 0 {
		return cerr.New(cerr.Usage, "usage: cpt ssh list [--json]")
	}
	targets, err := a.SSHTargets(ctx)
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(targets)
	}
	if len(targets) == 0 {
		a.Out.Infof("no SSH targets configured; use the TUI or cpt ssh add")
		return nil
	}
	tw := tabwriter.NewWriter(a.Out.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSSH\tBUNDLE\tREMOTE\tCLEANUP\tCACHE")
	for _, t := range targets {
		cache := "missing"
		if t.BundleExists && t.BundleValid {
			cache = "stale"
		}
		if t.CacheFresh {
			cache = "fresh"
		} else if t.BundleExists && !t.BundleValid {
			cache = "invalid"
		}
		cleanup := "keep"
		if t.CleanupOnExit {
			cleanup = "remove"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Name, t.Destination, bundleLabel(t), t.RemoteRootDisplay, cleanup, cache)
	}
	_ = tw.Flush()
	return nil
}

func bundleLabel(t app.SSHTargetInfo) string {
	if t.BundleTarget == "" {
		return t.Bundle
	}
	return t.Bundle + " (" + t.BundleTarget + ")"
}

func cmdSSHAdd(ctx context.Context, a *app.App, args []string) error {
	fs, flags := newSSHTargetFlags("ssh add")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if len(fs.Args()) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt ssh add <name> --bundle NAME (--config-host ALIAS | --host HOST)")
	}
	seen := visitedFlags(fs)
	if !seen["bundle"] || strings.TrimSpace(*flags.bundle) == "" {
		return cerr.New(cerr.Usage, "cpt ssh add needs --bundle NAME")
	}
	if seen["config-host"] && seen["host"] {
		return cerr.New(cerr.Usage, "use either --config-host or --host, not both")
	}
	name := fs.Args()[0]
	t := &sshtarget.Target{
		Name:       name,
		Bundle:     strings.TrimSpace(*flags.bundle),
		RemoteRoot: strings.TrimSpace(*flags.remoteRoot),
		Shell:      strings.TrimSpace(*flags.shell),
	}
	if seen["cleanup"] && seen["no-cleanup"] {
		return cerr.New(cerr.Usage, "use either --cleanup or --no-cleanup, not both")
	}
	if seen["cleanup"] {
		t.CleanupOnExit = *flags.cleanup
	}
	if seen["no-cleanup"] {
		t.CleanupOnExit = false
	}
	if seen["force"] && seen["no-force"] {
		return cerr.New(cerr.Usage, "use either --force or --no-force, not both")
	}
	if seen["force"] {
		t.Force = *flags.force
	}
	if seen["no-force"] {
		t.Force = false
	}
	switch {
	case seen["config-host"]:
		t.SSH = sshtarget.SSH{Source: sshtarget.SourceConfig, Host: strings.TrimSpace(*flags.configHost)}
	case seen["host"]:
		t.SSH = sshtarget.SSH{Source: sshtarget.SourceCustom, Host: strings.TrimSpace(*flags.host)}
	default:
		return cerr.New(cerr.Usage, "cpt ssh add needs --config-host ALIAS or --host HOST")
	}
	t.SSH.User = strings.TrimSpace(*flags.user)
	t.SSH.Port = *flags.port
	t.SSH.IdentityFile = strings.TrimSpace(*flags.identity)
	t.SSH.ProxyJump = strings.TrimSpace(*flags.proxyJump)
	if t.Shell == "" {
		t.Shell = sshtarget.ShellAuto
	}
	if err := a.SaveSSHTarget(t); err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(t)
	}
	a.Out.Line("added SSH target %s → %s with bundle %s", t.Name, t.Destination(), t.Bundle)
	return nil
}

func cmdSSHEdit(ctx context.Context, a *app.App, args []string) error {
	fs, flags := newSSHTargetFlags("ssh edit")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if len(fs.Args()) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt ssh edit <name> [flags]")
	}
	seen := visitedFlags(fs)
	if seen["config-host"] && seen["host"] {
		return cerr.New(cerr.Usage, "use either --config-host or --host, not both")
	}
	if seen["cleanup"] && seen["no-cleanup"] {
		return cerr.New(cerr.Usage, "use either --cleanup or --no-cleanup, not both")
	}
	if seen["force"] && seen["no-force"] {
		return cerr.New(cerr.Usage, "use either --force or --no-force, not both")
	}
	// Mutate under the ssh.json lock so a concurrent edit cannot drop fields
	// that were read before this command started.
	t, err := a.UpdateSSHTarget(fs.Args()[0], func(t *sshtarget.Target) error {
		if seen["bundle"] {
			t.Bundle = strings.TrimSpace(*flags.bundle)
		}
		if seen["config-host"] {
			t.SSH.Source = sshtarget.SourceConfig
			t.SSH.Host = strings.TrimSpace(*flags.configHost)
		}
		if seen["host"] {
			t.SSH.Source = sshtarget.SourceCustom
			t.SSH.Host = strings.TrimSpace(*flags.host)
		}
		if seen["user"] {
			t.SSH.User = strings.TrimSpace(*flags.user)
		}
		if seen["port"] {
			t.SSH.Port = *flags.port
		}
		if seen["identity"] {
			t.SSH.IdentityFile = strings.TrimSpace(*flags.identity)
		}
		if seen["proxy-jump"] {
			t.SSH.ProxyJump = strings.TrimSpace(*flags.proxyJump)
		}
		if seen["remote-root"] {
			t.RemoteRoot = strings.TrimSpace(*flags.remoteRoot)
		}
		if seen["shell"] {
			t.Shell = strings.TrimSpace(*flags.shell)
		}
		if seen["cleanup"] {
			t.CleanupOnExit = *flags.cleanup
		}
		if seen["no-cleanup"] {
			t.CleanupOnExit = false
		}
		if seen["force"] {
			t.Force = *flags.force
		}
		if seen["no-force"] {
			t.Force = false
		}
		return nil
	})
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(t)
	}
	a.Out.Line("updated SSH target %s → %s with bundle %s", t.Name, t.Destination(), t.Bundle)
	return nil
}

func cmdSSHRemove(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("ssh remove")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if len(fs.Args()) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt ssh remove <name>")
	}
	removed, err := a.RemoveSSHTarget(fs.Args()[0])
	if err != nil {
		return err
	}
	if a.Out.JSON {
		return a.Out.JSONData(removed)
	}
	a.Out.Line("removed SSH target %s", removed.Name)
	return nil
}

func cmdSSHConnect(ctx context.Context, a *app.App, args []string) error {
	fs := newFlagSet("ssh connect")
	if err := parseFlags(fs, args); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	if len(fs.Args()) != 1 {
		return cerr.New(cerr.Usage, "usage: cpt ssh <target>")
	}
	if a.Out.JSON {
		return cerr.New(cerr.Usage, "cpt ssh <target> is interactive and does not support --json")
	}
	name := fs.Args()[0]
	a.Out.Infof("connecting to %s…", name)
	_, err := a.SSHConnect(ctx, name, app.SSHConnectOptions{
		Stdin:  os.Stdin,
		Stdout: a.Out.Out,
		Stderr: a.Out.Err,
	})
	return err
}
