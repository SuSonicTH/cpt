package sshtarget

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Connection modes.
const (
	ModeInstall = "install"
	ModeShell   = "shell"
)

// Remote bootstrap exit codes. They are intentionally high enough to be
// unlikely to collide with a normal bundle or shell exit status.
const (
	ExitCacheInvalid       = 240
	ExitPlatformMismatch   = 241
	ExitUnsupportedShell   = 242
	DefaultRemoteDirectory = "~/.cpt"
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

// SessionCommand builds the interactive ssh command.
func (p Plan) SessionCommand(ctx context.Context) *exec.Cmd {
	args := []string{"-tt"}
	args = append(args, sshOptionArgs(p.Target, false)...)
	args = append(args, p.Target.Destination(), "sh -c "+shQuote(p.Bootstrap()))
	return exec.CommandContext(ctx, "ssh", args...)
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
		args = append(args, "-i", expandLocalHome(t.SSH.IdentityFile))
	}
	if t.SSH.ProxyJump != "" {
		args = append(args, "-o", "ProxyJump="+t.SSH.ProxyJump)
	}
	return args
}

// Bootstrap renders the POSIX script executed by the remote login shell.
func (p Plan) Bootstrap() string {
	wantOS, wantArch := splitPlatform(p.BundleTarget)
	root := `"$HOME/.cpt"`
	if p.Target.RemoteRoot != "" {
		root = shQuote(p.Target.RemoteRoot)
	}
	mode := p.Mode
	if mode != ModeShell {
		mode = ModeInstall
	}
	out := bootstrapTemplate
	replacements := []string{
		"@@ROOT@@", root,
		"@@NAME@@", p.Target.Name,
		"@@UPLOAD@@", p.UploadName(),
		"@@MODE@@", shQuote(mode),
		"@@CLEANUP@@", boolDigit(p.Target.CleanupOnExit),
		"@@FORCE@@", boolDigit(p.Target.Force),
		"@@WANT_OS@@", shQuote(wantOS),
		"@@WANT_ARCH@@", shQuote(wantArch),
		"@@SHELL@@", shQuote(p.Target.EffectiveShell()),
		"@@SHA@@", shQuote(strings.ToLower(p.BundleSHA256)),
	}
	return strings.NewReplacer(replacements...).Replace(out)
}

func expandLocalHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
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

const bootstrapTemplate = `set -u
umask 077
root=@@ROOT@@
bundle="$root/incoming/cpt-ssh-@@NAME@@.bundle"
upload="$HOME/@@UPLOAD@@"
marker="$root/incoming/cpt-ssh-@@NAME@@.ok"
init="$root/cptinit.sh"
CPT_SSH_INIT="$init"
export CPT_SSH_INIT
mode=@@MODE@@
cleanup=@@CLEANUP@@
force=@@FORCE@@
want_os=@@WANT_OS@@
want_arch=@@WANT_ARCH@@
shell_pref=@@SHELL@@
sha=@@SHA@@
do_cleanup=0
rc=""
zd=""

cleanup_run() {
  rm -f "$upload" 2>/dev/null || true
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
}
trap cleanup_run EXIT
trap 'exit 130' INT
trap 'exit 129' HUP
trap 'exit 143' TERM

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

do_cleanup="$cleanup"
orig_zd="${ZDOTDIR:-$HOME}"
CPT_SSH_ORIG_ZDOTDIR="$orig_zd"
export CPT_SSH_ORIG_ZDOTDIR
shell_bin="${SHELL:-/bin/sh}"
case "$shell_pref" in
  bash|zsh|sh) shell_bin="$shell_pref" ;;
esac
shell_base="${shell_bin##*/}"
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
  sh|dash|ash|ksh)
    rc="$HOME/.cpt-ssh-rc.$$"
    {
      printf '%s\n' 'if [ -f "$HOME/.profile" ]; then . "$HOME/.profile"; fi'
      printf '%s\n' '. "$CPT_SSH_INIT"'
    } > "$rc" || exit 1
    ENV="$rc" "$shell_bin" -i
    status=$?
    ;;
  *)
    echo "cpt: unsupported remote shell $shell_bin (configure bash, zsh or sh)" >&2
    exit 242
    ;;
esac
exit "$status"
`
