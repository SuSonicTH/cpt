package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/SuSonicTH/cpt/internal/bundle"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/install"
	"github.com/SuSonicTH/cpt/internal/platform"
	"github.com/SuSonicTH/cpt/internal/sshtarget"
	"github.com/SuSonicTH/cpt/internal/state"
)

// SSHTargetInfo describes a saved target for the CLI and TUI.
type SSHTargetInfo struct {
	sshtarget.Target
	BundlePath        string                  `json:"bundle_path,omitempty"`
	BundleTarget      string                  `json:"bundle_target,omitempty"`
	BundleExists      bool                    `json:"bundle_exists"`
	BundleValid       bool                    `json:"bundle_valid"`
	BundleSize        int64                   `json:"bundle_size,omitempty"`
	CacheFresh        bool                    `json:"cache_fresh"`
	RemoteRootDisplay string                  `json:"remote_root_display"`
	Destination       string                  `json:"destination"`
	Resolved          *sshtarget.ResolvedHost `json:"resolved,omitempty"`
	ResolveError      string                  `json:"resolve_error,omitempty"`
}

// SSHConnectOptions supplies the terminal used by the scp/ssh child processes.
type SSHConnectOptions struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// SSHConnectResult reports what a connection did.
type SSHConnectResult struct {
	Target       string `json:"target"`
	Mode         string `json:"mode"`
	Copied       bool   `json:"copied"`
	BundleSHA256 string `json:"bundle_sha256"`
	ExitCode     int    `json:"exit_code,omitempty"`
}

// SSHSessionError carries the remote shell's exit status through the CLI.
type SSHSessionError struct {
	Target string
	Code   int
}

func (e *SSHSessionError) Error() string {
	return fmt.Sprintf("SSH session for %s exited with status %d", e.Target, e.Code)
}

// ExitCode implements the exit-code interface used by cerr.ExitCode.
func (e *SSHSessionError) ExitCode() int { return e.Code }

func (a *App) withSSHFile(fn func(*sshtarget.File) error) error {
	lock, err := state.Acquire(a.Paths.SSHLockFile, 5*time.Second)
	if err != nil {
		return err
	}
	defer lock.Release()
	f, err := sshtarget.Load(a.Paths.SSHFile)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	return f.Save(a.Paths.SSHFile)
}

// SSHTargets lists saved targets with resolved bundle and SSH information.
func (a *App) SSHTargets(ctx context.Context) ([]SSHTargetInfo, error) {
	f, err := sshtarget.Load(a.Paths.SSHFile)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]SSHTargetInfo, 0, len(f.Targets))
	hashes := map[string]string{}
	for _, t := range f.Targets {
		info := SSHTargetInfo{
			Target:            *t,
			RemoteRootDisplay: t.EffectiveRemoteRoot(),
			Destination:       t.Destination(),
		}
		manifestPath := bundleManifestPath(a.Paths.Bundles, t.Bundle)
		if m, err := bundle.LoadManifest(manifestPath); err == nil {
			info.BundlePath = m.OutputPath(a.Paths.Bundles)
			info.BundleTarget = m.Target
			if st, err := os.Stat(info.BundlePath); err == nil && st.Mode().IsRegular() {
				info.BundleExists = true
				info.BundleSize = st.Size()
				info.BundleValid = bundle.IsBundle(info.BundlePath)
			}
		}
		if info.BundleValid && !t.CleanupOnExit && !t.LastCopied.IsZero() && now.Sub(t.LastCopied) < sshtarget.CacheTTL {
			sha, ok := hashes[info.BundlePath]
			if !ok {
				if got, err := install.FileSHA256(info.BundlePath); err == nil {
					sha = got
					hashes[info.BundlePath] = got
				}
			}
			if sha != "" {
				info.CacheFresh = t.CacheFresh(now, sha)
			}
		}
		if t.SSH.Source == sshtarget.SourceConfig {
			resolved, resolveErr := sshtarget.Resolve(ctx, t.SSH.Host)
			if resolveErr == nil {
				if t.SSH.User != "" {
					resolved.User = t.SSH.User
				}
				if t.SSH.Port > 0 {
					resolved.Port = t.SSH.Port
				}
				if t.SSH.IdentityFile != "" {
					resolved.IdentityFile = t.SSH.IdentityFile
				}
				if t.SSH.ProxyJump != "" {
					resolved.ProxyJump = t.SSH.ProxyJump
				}
				info.Resolved = &resolved
			} else {
				info.ResolveError = resolveErr.Error()
			}
		}
		out = append(out, info)
	}
	sortSSHTargets(out)
	return out, nil
}

func bundleManifestPath(dir, name string) string {
	return filepath.Join(dir, bundle.FileName(name))
}

func sortSSHTargets(out []SSHTargetInfo) {
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
}

// SaveSSHTarget creates or updates one target. Bundle references must name an
// existing manifest.
func (a *App) SaveSSHTarget(t *sshtarget.Target) error {
	if t == nil {
		return cerr.New(cerr.Usage, "no SSH target supplied")
	}
	if err := t.Validate(); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	manifestPath := bundleManifestPath(a.Paths.Bundles, t.Bundle)
	m, err := bundle.LoadManifest(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cerr.New(cerr.Usage, "unknown bundle %q; create it before adding the SSH target", t.Bundle)
		}
		return fmt.Errorf("bundle %s: %w", t.Bundle, err)
	}
	if p, err := platform.Parse(m.Target); err != nil || p.OS == "windows" {
		return cerr.New(cerr.Usage, "SSH targets support POSIX bundles only; bundle %s targets %s", t.Bundle, m.Target)
	}
	if !bundle.IsBundle(m.OutputPath(a.Paths.Bundles)) {
		return cerr.New(cerr.Usage, "bundle %q has no valid executable; rebuild it before adding the SSH target", t.Bundle)
	}
	return a.withSSHFile(func(f *sshtarget.File) error {
		now := time.Now().UTC()
		prev := f.Targets[t.Name]
		if prev != nil {
			if t.Created.IsZero() {
				t.Created = prev.Created
			}
			t.LastConnected = prev.LastConnected
			if sameCopySettings(prev, t) {
				t.LastCopied = prev.LastCopied
				t.CopiedSHA256 = prev.CopiedSHA256
				t.CopiedKey = prev.CopiedKey
			} else {
				t.LastCopied = time.Time{}
				t.CopiedSHA256 = ""
				t.CopiedKey = ""
			}
		} else {
			t.LastCopied = time.Time{}
			t.CopiedSHA256 = ""
			t.CopiedKey = ""
		}
		if t.Created.IsZero() {
			t.Created = now
		}
		t.Updated = now
		return f.Put(t)
	})
}

func sameCopySettings(a, b *sshtarget.Target) bool {
	if a.Bundle != b.Bundle || a.RemoteRoot != b.RemoteRoot || a.Force != b.Force {
		return false
	}
	return a.SSH == b.SSH
}

// RemoveSSHTarget deletes a saved target. It never touches the remote host.
func (a *App) RemoveSSHTarget(name string) (*sshtarget.Target, error) {
	var removed *sshtarget.Target
	err := a.withSSHFile(func(f *sshtarget.File) error {
		t, err := f.Get(name)
		if err != nil {
			return cerr.New(cerr.Usage, "%v", err)
		}
		delete(f.Targets, name)
		removed = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// SSHTarget loads one raw saved target.
func (a *App) SSHTarget(name string) (*sshtarget.Target, error) {
	f, err := sshtarget.Load(a.Paths.SSHFile)
	if err != nil {
		return nil, err
	}
	t, err := f.Get(name)
	if err != nil {
		return nil, cerr.New(cerr.Usage, "%v", err)
	}
	return t, nil
}

// SSHConfigHosts lists concrete aliases from ~/.ssh/config.
func (a *App) SSHConfigHosts() ([]sshtarget.ConfigHost, error) {
	path, err := sshtarget.DefaultConfigPath()
	if err != nil {
		return nil, err
	}
	return sshtarget.Hosts(path)
}

// SSHConnect copies the bundle when necessary and opens the interactive
// session. A fresh cache skips both the upload and the bundle run.
func (a *App) SSHConnect(ctx context.Context, name string, opt SSHConnectOptions) (SSHConnectResult, error) {
	if a.Offline {
		return SSHConnectResult{}, cerr.New(cerr.Usage, "cannot connect to an SSH target in offline mode")
	}
	f, err := sshtarget.Load(a.Paths.SSHFile)
	if err != nil {
		return SSHConnectResult{}, err
	}
	target, err := f.Get(name)
	if err != nil {
		return SSHConnectResult{}, cerr.New(cerr.Usage, "%v", err)
	}
	if err := target.Validate(); err != nil {
		return SSHConnectResult{}, cerr.New(cerr.Usage, "%v", err)
	}
	manifestPath := bundleManifestPath(a.Paths.Bundles, target.Bundle)
	m, err := bundle.LoadManifest(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return SSHConnectResult{}, cerr.New(cerr.Usage, "SSH target %s references unknown bundle %s", name, target.Bundle)
		}
		return SSHConnectResult{}, err
	}
	bundlePath := m.OutputPath(a.Paths.Bundles)
	if p, err := platform.Parse(m.Target); err != nil || p.OS == "windows" {
		return SSHConnectResult{}, cerr.New(cerr.Usage,
			"SSH targets support POSIX bundles only; bundle %s targets %s", target.Bundle, m.Target)
	}
	if !bundle.IsBundle(bundlePath) {
		return SSHConnectResult{}, cerr.New(cerr.Integrity,
			"bundle %s has no valid executable at %s; rebuild it", target.Bundle, bundlePath)
	}
	sha, err := install.FileSHA256(bundlePath)
	if err != nil {
		return SSHConnectResult{}, fmt.Errorf("hash bundle %s: %w", bundlePath, err)
	}
	now := time.Now().UTC()
	plan := sshtarget.Plan{
		Target:       *target,
		BundlePath:   bundlePath,
		BundleSHA256: sha,
		BundleTarget: m.Target,
	}
	if target.CacheFresh(now, sha) {
		plan.Mode = sshtarget.ModeShell
	} else {
		plan.Mode = sshtarget.ModeInstall
	}

	in := opt.Stdin
	if in == nil {
		in = os.Stdin
	}
	stdout := opt.Stdout
	if stdout == nil {
		stdout = a.Out.Out
	}
	stderr := opt.Stderr
	if stderr == nil {
		stderr = a.Out.Err
	}
	ioOpt := SSHConnectOptions{Stdin: in, Stdout: stdout, Stderr: stderr}

	result := SSHConnectResult{
		Target:       target.Name,
		Mode:         plan.Mode,
		BundleSHA256: sha,
	}
	copyKey := target.CopyKey(sha)
	for attempt := 0; attempt < 2; attempt++ {
		if plan.Mode == sshtarget.ModeInstall {
			copyCmd := plan.UploadCommand(ctx)
			copyCmd.Stdin, copyCmd.Stdout, copyCmd.Stderr = ioOpt.Stdin, ioOpt.Stdout, ioOpt.Stderr
			if err := copyCmd.Run(); err != nil {
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				return result, cerr.Wrap(cerr.Network, fmt.Errorf("copy bundle to %s: %w", target.Destination(), err))
			}
			result.Copied = true
			result.Mode = sshtarget.ModeInstall
			_ = a.recordSSHCopy(target.Name, sha, copyKey, now)
		}
		session := plan.SessionCommand(ctx)
		session.Stdin, session.Stdout, session.Stderr = ioOpt.Stdin, ioOpt.Stdout, ioOpt.Stderr
		runErr := session.Run()
		if runErr != nil {
			var execErr *exec.Error
			if errors.As(runErr, &execErr) {
				return result, fmt.Errorf("cannot run ssh: %w", runErr)
			}
		}
		code := commandExitCode(runErr)
		if runErr == nil {
			result.ExitCode = 0
			_ = a.recordSSHConnected(target.Name)
			return result, nil
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		switch code {
		case sshtarget.ExitCacheInvalid:
			if attempt == 0 {
				plan.Mode = sshtarget.ModeInstall
				continue
			}
			return result, cerr.New(cerr.Integrity,
				"remote bundle cache for %s could not be refreshed; check that the remote home is writable", target.Name)
		case sshtarget.ExitPlatformMismatch:
			return result, cerr.New(cerr.Conflict,
				"bundle %s targets %s but the remote host reports a different platform", target.Bundle, m.Target)
		case sshtarget.ExitUnsupportedShell:
			return result, cerr.New(cerr.Usage,
				"the remote shell is not supported; configure bash, zsh or sh for target %s", target.Name)
		}
		result.ExitCode = code
		_ = a.recordSSHConnected(target.Name)
		return result, &SSHSessionError{Target: target.Name, Code: code}
	}
	return result, cerr.New(cerr.Integrity, "remote bundle cache for %s could not be refreshed", target.Name)
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code > 0 {
			return code
		}
	}
	return 1
}

func (a *App) recordSSHCopy(name, sha, key string, at time.Time) error {
	return a.withSSHFile(func(f *sshtarget.File) error {
		t := f.Targets[name]
		if t == nil {
			return nil
		}
		t.LastCopied = at
		t.CopiedSHA256 = sha
		t.CopiedKey = key
		return nil
	})
}

func (a *App) recordSSHConnected(name string) error {
	return a.withSSHFile(func(f *sshtarget.File) error {
		t := f.Targets[name]
		if t == nil {
			return nil
		}
		t.LastConnected = time.Now().UTC()
		return nil
	})
}
