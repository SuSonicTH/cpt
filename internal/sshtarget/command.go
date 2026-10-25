package sshtarget

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Connection modes.
const (
	ModeInstall = "install"
	ModeShell   = "shell"
)

// DefaultRemoteDirectory is the remote cpt root when none is configured.
const DefaultRemoteDirectory = "~/.cpt"

// Prepare-phase exit codes. They are produced only by the non-interactive
// prepare command, never by an interactive shell, so they cannot collide with
// the status a user's session returns.
const (
	ExitCacheInvalid     = 240
	ExitPlatformMismatch = 241
	ExitUnsupportedShell = 242
)

// Plan is one prepared SSH connect operation.
type Plan struct {
	Target       Target
	BundlePath   string
	BundleSHA256 string
	BundleTarget string // os/arch from the bundle manifest
	Mode         string // install or shell
}

// UploadName is the temporary home-relative name used by scp. It deliberately
// lives in the home directory so scp never has to create a remote directory.
func (p Plan) UploadName() string {
	return ".cpt-ssh-upload-" + p.Target.Name + ".bundle"
}

// UploadTarget is the scp destination token.
func (p Plan) UploadTarget() string {
	return p.Target.Destination() + ":" + p.UploadName()
}

// UploadCommand builds the scp command. Its stdin/stdout stay available for
// authentication prompts because the bundle is read from the local path.
func (p Plan) UploadCommand(ctx context.Context) *exec.Cmd {
	args := []string{"-q"}
	args = append(args, sshOptionArgs(p.Target, true)...)
	args = append(args, p.BundlePath, p.UploadTarget())
	return exec.CommandContext(ctx, "scp", args...)
}

// PrepareCommand builds the non-interactive ssh command that validates or
// installs the remote copy. It runs no interactive shell, so its exit status is
// always one of the prepare sentinels (or a bundle failure) and never a session
// status.
func (p Plan) PrepareCommand(ctx context.Context) *exec.Cmd {
	args := []string{"-T"}
	args = append(args, sshOptionArgs(p.Target, false)...)
	args = append(args, p.Target.Destination(), "sh -c "+shQuote(p.Prepare()))
	return exec.CommandContext(ctx, "ssh", args...)
}

// ShellCommand builds the interactive ssh command. Its exit status is the
// remote shell's own status and is propagated unchanged.
func (p Plan) ShellCommand(ctx context.Context) *exec.Cmd {
	args := []string{"-tt"}
	args = append(args, sshOptionArgs(p.Target, false)...)
	args = append(args, p.Target.Destination(), "sh -c "+shQuote(p.ShellScript()))
	return interactiveCommand(ctx, "ssh", args...)
}

// interactiveCommand keeps the local terminal usable when the parent context is
// cancelled. exec.CommandContext kills the child by default, which leaves the
// terminal in raw mode because ssh never gets a chance to restore it. SIGTERM
// lets ssh clean up; platforms that cannot deliver it fall back to Kill.
func interactiveCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func sshOptionArgs(t Target, scp bool) []string {
	var args []string
	if t.SSH.Port > 0 {
		if scp {
			args = append(args, "-P", fmt.Sprintf("%d", t.SSH.Port))
		} else {
			args = append(args, "-p", fmt.Sprintf("%d", t.SSH.Port))
		}
	}
	if t.SSH.IdentityFile != "" {
		args = append(args, "-i", expandHome(t.SSH.IdentityFile))
	}
	if t.SSH.ProxyJump != "" {
		args = append(args, "-o", "ProxyJump="+t.SSH.ProxyJump)
	}
	return args
}

// Prepare renders the POSIX script that validates or installs the remote copy.
func (p Plan) Prepare() string {
	wantOS, wantArch := splitPlatform(p.BundleTarget)
	mode := p.Mode
	if mode != ModeShell {
		mode = ModeInstall
	}
	return strings.NewReplacer(prepareReplacements(p, wantOS, wantArch, mode)...).Replace(prepareTemplate)
}

// ShellScript renders the POSIX script that starts the session-scoped shell.
func (p Plan) ShellScript() string {
	return strings.NewReplacer(shellReplacements(p)...).Replace(shellTemplate)
}

func prepareReplacements(p Plan, wantOS, wantArch, mode string) []string {
	return []string{
		"@@ROOT@@", rootValue(p.Target),
		"@@NAME@@", p.Target.Name,
		"@@UPLOAD@@", p.UploadName(),
		"@@MODE@@", shQuote(mode),
		"@@FORCE@@", boolDigit(p.Target.Force),
		"@@WANT_OS@@", shQuote(wantOS),
		"@@WANT_ARCH@@", shQuote(wantArch),
		"@@SHELL@@", shQuote(p.Target.EffectiveShell()),
		"@@SHA@@", shQuote(strings.ToLower(p.BundleSHA256)),
	}
}

func shellReplacements(p Plan) []string {
	return []string{
		"@@ROOT@@", rootValue(p.Target),
		"@@CLEANUP@@", boolDigit(p.Target.CleanupOnExit),
		"@@SHELL@@", shQuote(p.Target.EffectiveShell()),
	}
}

func rootValue(t Target) string {
	if t.RemoteRoot != "" {
		return shQuote(t.RemoteRoot)
	}
	return `"$HOME/.cpt"`
}

func splitPlatform(target string) (string, string) {
	parts := strings.SplitN(strings.TrimSpace(target), "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func boolDigit(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellSupport is shared by both phases so the interactive script can trust the
// check the prepare phase already made.
const shellSupport = `shell_bin="${SHELL:-/bin/sh}"
case "$shell_pref" in
  bash|zsh|sh) shell_bin="$shell_pref" ;;
esac
shell_base="${shell_bin##*/}"
case "$shell_base" in
  bash|zsh|sh|dash|ash|ksh) ;;
  *)
    echo "cpt: unsupported remote shell $shell_bin (configure bash, zsh or sh)" >&2
    exit 242
    ;;
esac
if ! command -v "$shell_bin" >/dev/null 2>&1; then
  echo "cpt: remote shell $shell_bin was not found (configure bash, zsh or sh)" >&2
  exit 242
fi
`

const prepareTemplate = `set -u
umask 077
root=@@ROOT@@
bundle="$root/incoming/cpt-ssh-@@NAME@@.bundle"
upload="$HOME/@@UPLOAD@@"
marker="$root/incoming/cpt-ssh-@@NAME@@.ok"
init="$root/cptinit.sh"
mode=@@MODE@@
force=@@FORCE@@
want_os=@@WANT_OS@@
want_arch=@@WANT_ARCH@@
shell_pref=@@SHELL@@
sha=@@SHA@@

# Never leave the home-directory upload behind, whatever this phase did.
cleanup_upload() {
  st=$?
  rm -f "$upload" 2>/dev/null || true
  exit $st
}
trap cleanup_upload EXIT
trap 'exit 130' INT
trap 'exit 129' HUP
trap 'exit 143' TERM

` + shellSupport + `
if [ -n "$want_os" ]; then
  os=$(uname -s 2>/dev/null || true)
  arch=$(uname -m 2>/dev/null || true)
  case "$os" in
    Linux|linux) os=linux ;;
    Darwin|darwin) os=darwin ;;
  esac
  case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
  esac
  if [ -n "$os" ] && [ -n "$arch" ] && { [ "$os" != "$want_os" ] || [ "$arch" != "$want_arch" ]; }; then
    echo "cpt: bundle targets $want_os/$want_arch but the remote host is $os/$arch" >&2
    exit 241
  fi
fi

if [ "$mode" = install ]; then
  if [ ! -f "$upload" ]; then
    echo "cpt: uploaded bundle was not found on the remote host" >&2
    exit 240
  fi
  chmod 700 "$upload" 2>/dev/null || true
  # Run from the home upload path. The destination root must still look like a
  # clean cpt root: bundle.validateDest rejects a non-empty root with no state.
  if [ "$force" = 1 ]; then
    "$upload" --dest "$root" --force
  else
    "$upload" --dest "$root"
  fi
  status=$?
  if [ "$status" -ne 0 ]; then
    # The prepare sentinels must never be produced by the bundle itself.
    case "$status" in
      240|241|242|255) status=1 ;;
    esac
    exit "$status"
  fi
  if [ ! -r "$init" ]; then
    echo "cpt: bundle did not create $init" >&2
    exit 1
  fi
  # Cache the bundle inside the cpt root only after the install, so the first
  # run sees a clean destination.
  mkdir -p "$root/incoming" || exit 1
  mv -f "$upload" "$bundle" || exit 1
  chmod 700 "$bundle" || exit 1
  printf '%s\n' "$sha" > "$marker" || exit 1
else
  if [ ! -r "$bundle" ] || [ ! -r "$init" ] || [ ! -r "$marker" ]; then
    exit 240
  fi
  cached=$(cat "$marker" 2>/dev/null || true)
  if [ "$cached" != "$sha" ]; then
    exit 240
  fi
fi
exit 0
`

const shellTemplate = `set -u
umask 077
root=@@ROOT@@
init="$root/cptinit.sh"
cleanup=@@CLEANUP@@
shell_pref=@@SHELL@@
do_cleanup=0
rc=""
zd=""

cleanup_run() {
  st=$?
  if [ -n "$rc" ]; then rm -f "$rc" 2>/dev/null || true; fi
  if [ -n "$zd" ]; then rm -rf "$zd" 2>/dev/null || true; fi
  if [ "$do_cleanup" = 1 ]; then
    case "$root" in
      ""|"/"|"$HOME"|"$HOME/")
        echo "cpt: refusing to clean $root" >&2
        ;;
      /*/*)
        case "$HOME" in
          "$root"/*) echo "cpt: refusing to clean $root (contains HOME)" >&2 ;;
          *) rm -rf "$root" 2>/dev/null || true ;;
        esac
        ;;
      *)
        echo "cpt: refusing to clean top-level directory $root" >&2
        ;;
    esac
  fi
  exit $st
}
trap cleanup_run EXIT
trap 'exit 130' INT
trap 'exit 129' HUP
trap 'exit 143' TERM

if [ ! -r "$init" ]; then
  # 240 belongs to the prepare phase; this phase must never emit a sentinel.
  echo "cpt: remote cpt environment is missing at $init" >&2
  exit 1
fi

CPT_SSH_INIT="$init"
export CPT_SSH_INIT
do_cleanup="$cleanup"
orig_zd="${ZDOTDIR:-$HOME}"
CPT_SSH_ORIG_ZDOTDIR="$orig_zd"
export CPT_SSH_ORIG_ZDOTDIR

` + shellSupport + `
case "$shell_base" in
  bash)
    rc="$HOME/.cpt-ssh-rc.$$"
    {
      printf '%s\n' 'if [ -f "$HOME/.bashrc" ]; then . "$HOME/.bashrc"; fi'
      printf '%s\n' '. "$CPT_SSH_INIT"'
    } > "$rc" || exit 1
    "$shell_bin" --rcfile "$rc" -i
    status=$?
    ;;
  zsh)
    zd="$HOME/.cpt-ssh-zdot.$$"
    mkdir -p "$zd" || exit 1
    {
      printf '%s\n' 'orig_zd="${CPT_SSH_ORIG_ZDOTDIR:-$HOME}"'
      printf '%s\n' 'ZDOTDIR="$orig_zd"'
      printf '%s\n' 'if [ -f "$orig_zd/.zshrc" ]; then . "$orig_zd/.zshrc"; fi'
      printf '%s\n' '. "$CPT_SSH_INIT"'
    } > "$zd/.zshrc" || exit 1
    ZDOTDIR="$zd" "$shell_bin" -i
    status=$?
    ;;
  *)
    rc="$HOME/.cpt-ssh-rc.$$"
    {
      printf '%s\n' 'if [ -f "$HOME/.profile" ]; then . "$HOME/.profile"; fi'
      printf '%s\n' '. "$CPT_SSH_INIT"'
    } > "$rc" || exit 1
    ENV="$rc" "$shell_bin" -i
    status=$?
    ;;
esac
exit "$status"
`
