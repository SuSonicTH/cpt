package sshtarget

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/SuSonicTH/cpt/internal/platform"
)

func TestHostsParsesIncludesAndSkipsPatterns(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "config")
	body := `# a comment
Host alpha beta
    HostName example.test
Host *.wild
Host !negated
Include conf.d/*
`
	if err := os.WriteFile(main, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conf.d", "extra"), []byte("Host gamma # inline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hosts, err := Hosts(main)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range hosts {
		names = append(names, h.Alias)
	}
	got := strings.Join(names, ",")
	if got != "alpha,beta,gamma" {
		t.Fatalf("hosts = %q, want alpha,beta,gamma", got)
	}
}

func TestPrepareScriptIsPOSIXAndInstallsBeforeIncoming(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell is not available")
	}
	p := Plan{
		Target: Target{
			Name:          "staging",
			Bundle:        "devbox",
			SSH:           SSH{Source: SourceConfig, Host: "box"},
			Shell:         ShellAuto,
			CleanupOnExit: true,
		},
		BundleSHA256: strings.Repeat("a", 64),
		BundleTarget: "linux/amd64",
		Mode:         ModeInstall,
	}
	script := p.Prepare()
	for _, forbidden := range []string{"--install-init", ">>", "sed -i", "-tt", "ZDOTDIR"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("prepare script contains %q", forbidden)
		}
	}
	for _, want := range []string{
		`"$upload" --dest "$root"`,
		"exit 240", "exit 241", "exit 242",
		`command -v "$shell_bin"`,
		`printf '%s\n' "$sha" > "$marker"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("prepare script missing %q:\n%s", want, script)
		}
	}
	runIdx := strings.Index(script, `"$upload" --dest "$root"`)
	mkdirIdx := strings.Index(script, `mkdir -p "$root/incoming"`)
	if runIdx < 0 || mkdirIdx < 0 || mkdirIdx < runIdx {
		t.Fatalf("bundle must run before incoming/ is created:\n%s", script)
	}
	assertPOSIXShell(t, script)
}

func TestShellScriptKeepsSessionScopeAndIsPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell is not available")
	}
	p := Plan{
		Target: Target{
			Name:          "staging",
			Bundle:        "devbox",
			SSH:           SSH{Source: SourceConfig, Host: "box"},
			Shell:         ShellBash,
			CleanupOnExit: true,
		},
		BundleSHA256: strings.Repeat("a", 64),
		BundleTarget: "linux/amd64",
		Mode:         ModeShell,
	}
	script := p.ShellScript()
	for _, forbidden := range []string{"--install-init", "--dest", "sed -i", "-T "} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("shell script contains %q", forbidden)
		}
	}
	for _, want := range []string{
		"ZDOTDIR", "CPT_SSH_ORIG_ZDOTDIR", `do_cleanup="$cleanup"`,
		`trap cleanup_run EXIT`, `exit "$status"`, "shell_pref='bash'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("shell script missing %q:\n%s", want, script)
		}
	}
	// The interactive status must be the last thing the script yields, so it is
	// never rewritten by a sentinel check.
	trimmed := strings.TrimSpace(script)
	if !strings.HasSuffix(trimmed, `exit "$status"`) {
		t.Fatalf("shell script must end by propagating the session status:\n%s", script)
	}
	assertPOSIXShell(t, script)
}

func assertPOSIXShell(t *testing.T, script string) {
	t.Helper()
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script is not valid sh: %v\n%s\n%s", err, out, script)
	}
}

func TestPlanCommandsUseSSHConfigAndOverrides(t *testing.T) {
	p := Plan{
		Target: Target{
			Name:   "staging",
			Bundle: "devbox",
			SSH: SSH{
				Source:       SourceConfig,
				Host:         "staging-box",
				User:         "mike",
				Port:         2222,
				IdentityFile: "/tmp/key",
				ProxyJump:    "bastion",
			},
		},
		BundlePath:   "/tmp/devbox",
		BundleSHA256: strings.Repeat("a", 64),
		BundleTarget: "linux/amd64",
		Mode:         ModeShell,
	}
	upload := strings.Join(p.UploadCommand(t.Context()).Args, " ")
	for _, want := range []string{"scp", "-P 2222", "-i /tmp/key", "ProxyJump=bastion", p.UploadTarget()} {
		if !strings.Contains(upload, want) {
			t.Fatalf("upload command missing %q: %s", want, upload)
		}
	}
	prepare := strings.Join(p.PrepareCommand(t.Context()).Args, " ")
	for _, want := range []string{"ssh", "-T", "-p 2222", "-i /tmp/key", "ProxyJump=bastion", "mike@staging-box", "sh -c"} {
		if !strings.Contains(prepare, want) {
			t.Fatalf("prepare command missing %q: %s", want, prepare)
		}
	}
	if strings.Contains(prepare, "-tt") {
		t.Fatalf("prepare command must not allocate a tty: %s", prepare)
	}
	shell := strings.Join(p.ShellCommand(t.Context()).Args, " ")
	for _, want := range []string{"ssh", "-tt", "-p 2222", "-i /tmp/key", "ProxyJump=bastion", "mike@staging-box", "sh -c"} {
		if !strings.Contains(shell, want) {
			t.Fatalf("shell command missing %q: %s", want, shell)
		}
	}
}

func TestPlanUsesCustomRoot(t *testing.T) {
	p := Plan{
		Target: Target{
			Name:       "lab",
			Bundle:     "devbox",
			SSH:        SSH{Source: SourceCustom, Host: "10.0.0.7"},
			RemoteRoot: "/srv/cpt",
		},
		BundleTarget: "linux/amd64",
		Mode:         ModeShell,
	}
	for name, script := range map[string]string{"prepare": p.Prepare(), "shell": p.ShellScript()} {
		if !strings.Contains(script, "root='/srv/cpt'") {
			t.Fatalf("%s script is missing the custom root:\n%s", name, script)
		}
	}
}

// TestPrepareInstallsAndShellCleansUp runs both phases against a fake bundle for
// every supported shell branch, so a dropped shell_pref case cannot pass.
func TestPrepareInstallsAndShellCleansUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell is not available")
	}
	cases := []struct{ shell, bin string }{
		{ShellSh, "/bin/sh"},
		{ShellBash, "bash"},
		{ShellZsh, "zsh"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.shell, func(t *testing.T) {
			if _, err := exec.LookPath(tc.bin); err != nil {
				t.Skipf("%s is not installed", tc.bin)
			}
			home := t.TempDir()
			root := filepath.Join(t.TempDir(), "cpt-root")
			upload := filepath.Join(home, (Plan{Target: Target{Name: "staging"}}).UploadName())
			argsFile := filepath.Join(home, "bundle-args")
			body := `#!/bin/sh
{
  for a in "$@"; do printf '%s\n' "$a"; done
} > ` + shQuote(argsFile) + `
dest="$2"
if [ -e "$dest/incoming" ]; then
  echo "destination was pre-created with incoming/" >&2
  exit 66
fi
mkdir -p "$dest"
printf '# generated by cpt\n' > "$dest/cptinit.sh"
exit 0
`
			if err := os.WriteFile(upload, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			p := Plan{
				Target: Target{
					Name:       "staging",
					Bundle:     "devbox",
					SSH:        SSH{Source: SourceCustom, Host: "box"},
					RemoteRoot: root,
					Shell:      tc.shell,
					Force:      true,
				},
				BundleSHA256: strings.Repeat("a", 64),
				BundleTarget: platform.Current().String(),
				Mode:         ModeInstall,
			}
			run := func(script string) int {
				t.Helper()
				cmd := exec.Command("/bin/sh", "-c", script)
				cmd.Env = append(os.Environ(), "HOME="+home, "SHELL="+tc.bin)
				cmd.Stdin = strings.NewReader("exit 0\n")
				var out strings.Builder
				cmd.Stdout = &out
				cmd.Stderr = &out
				err := cmd.Run()
				if err == nil {
					return 0
				}
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					t.Logf("script output:\n%s", out.String())
					return ee.ExitCode()
				}
				t.Fatalf("run script: %v\n%s", err, out.String())
				return -1
			}

			if code := run(p.Prepare()); code != 0 {
				t.Fatalf("prepare phase exited %d", code)
			}
			bundlePath := filepath.Join(root, "incoming", "cpt-ssh-staging.bundle")
			if _, err := os.Stat(bundlePath); err != nil {
				t.Fatalf("cached bundle missing: %v", err)
			}
			marker := filepath.Join(root, "incoming", "cpt-ssh-staging.ok")
			data, err := os.ReadFile(marker)
			if err != nil || strings.TrimSpace(string(data)) != p.BundleSHA256 {
				t.Fatalf("marker = %q err=%v", data, err)
			}
			if _, err := os.Stat(filepath.Join(root, "cptinit.sh")); err != nil {
				t.Fatalf("cptinit missing: %v", err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("bundle was not run: %v", err)
			}
			if !strings.Contains(string(args), "--force") {
				t.Fatalf("--force was not passed to the bundle:\n%s", args)
			}
			if _, err := os.Stat(upload); !os.IsNotExist(err) {
				t.Fatalf("prepare left the home upload behind: %v", err)
			}

			p.Mode = ModeShell
			p.Target.CleanupOnExit = true
			if code := run(p.ShellScript()); code != 0 {
				t.Fatalf("shell phase exited %d", code)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("cleanup left remote root behind: %v", err)
			}
		})
	}
}

// TestShellScriptPropagatesSentinelLikeStatus documents that the shell phase
// never rewrites a session status, even one that collides with a prepare code.
func TestShellScriptPropagatesSentinelLikeStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell is not available")
	}
	dir := t.TempDir()
	for _, code := range []string{"240", "241", "242"} {
		fake := filepath.Join(dir, "sh-"+code)
		if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit "+code+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, code := range []string{"240", "241", "242"} {
		code := code
		t.Run(code, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(t.TempDir(), "root")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "cptinit.sh"), []byte("# init\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// A fake `sh` on PATH makes the interactive shell exit with the
			// sentinel-like status.
			bin := filepath.Join(t.TempDir(), "sh")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit "+code+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			p := Plan{Target: Target{Name: "staging", SSH: SSH{Source: SourceCustom, Host: "box"}, RemoteRoot: root, Shell: ShellSh}, Mode: ModeShell}
			cmd := exec.Command("/bin/sh", "-c", p.ShellScript())
			cmd.Env = append(os.Environ(), "HOME="+home, "SHELL="+filepath.Join(dir, "sh-"+code), "PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
			cmd.Stdin = strings.NewReader("")
			var out strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &out
			err := cmd.Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("shell script err = %v, want exit status %s\n%s", err, code, out.String())
			}
			if got := strconv.Itoa(ee.ExitCode()); got != code {
				t.Fatalf("shell script exited %s, want %s propagated verbatim\n%s", got, code, out.String())
			}
		})
	}
}

func TestResolveParsesSSHG(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh script is POSIX-only")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
printf 'hostname 10.0.0.9\nuser mike\nport 2222\nidentityfile ~/.ssh/id_test\nproxyjump bastion\n'
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := Resolve(t.Context(), "staging-box")
	if err != nil {
		t.Fatal(err)
	}
	if got.HostName != "10.0.0.9" || got.User != "mike" || got.Port != 2222 ||
		got.IdentityFile != "~/.ssh/id_test" || got.ProxyJump != "bastion" {
		t.Fatalf("unexpected resolve result: %+v", got)
	}
}

func TestHostsSkipsDirectoryAndUnreadableIncludes(t *testing.T) {
	dir := t.TempDir()
	confd := filepath.Join(dir, "conf.d")
	if err := os.MkdirAll(filepath.Join(confd, "backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "config")
	if err := os.WriteFile(main, []byte("Include conf.d/*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confd, "extra"), []byte("Host gamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A glob that matches a directory must not hide every other alias.
	hosts, err := Hosts(main)
	if err != nil {
		t.Fatalf("directory in Include broke discovery: %v", err)
	}
	if len(hosts) != 1 || hosts[0].Alias != "gamma" {
		t.Fatalf("hosts = %+v, want gamma", hosts)
	}
}

// TestPrepareRemapsBundleStatusesOutOfTheSentinelRange keeps a failing bundle
// from being reported as a cache or platform sentinel, or as an ssh failure.
func TestPrepareRemapsBundleStatusesOutOfTheSentinelRange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell is not available")
	}
	for _, exit := range []string{"240", "241", "242", "255"} {
		exit := exit
		t.Run(exit, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(t.TempDir(), "root")
			upload := filepath.Join(home, (Plan{Target: Target{Name: "staging"}}).UploadName())
			body := "#!/bin/sh\nexit " + exit + "\n"
			if err := os.WriteFile(upload, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			p := Plan{
				Target:       Target{Name: "staging", Bundle: "devbox", SSH: SSH{Source: SourceCustom, Host: "box"}, RemoteRoot: root, Shell: ShellSh},
				BundleSHA256: strings.Repeat("a", 64),
				Mode:         ModeInstall,
			}
			cmd := exec.Command("/bin/sh", "-c", p.Prepare())
			cmd.Env = append(os.Environ(), "HOME="+home, "SHELL=/bin/sh")
			err := cmd.Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("prepare err = %v, want a non-zero exit", err)
			}
			if got := ee.ExitCode(); got != 1 {
				t.Fatalf("bundle status %s surfaced as %d, want the remapped generic status 1", exit, got)
			}
		})
	}
}
