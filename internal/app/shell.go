package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/fsutil"
	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/shellinit"
	"github.com/SuSonicTH/cpt/internal/state"
)

// ShellInfo describes the approval state of one package's shell snippet.
type ShellInfo struct {
	Package        string `json:"package"`
	Declared       bool   `json:"declared"`
	Live           bool   `json:"live"`
	LiveVersion    string `json:"live_version,omitempty"`
	LiveSHA256     string `json:"live_sha256,omitempty"`
	Pending        bool   `json:"pending"`
	PendingVersion string `json:"pending_version,omitempty"`
	PendingSHA256  string `json:"pending_sha256,omitempty"`
	Removal        bool   `json:"removal,omitempty"`
	Rejected       bool   `json:"rejected,omitempty"`
	SyncPending    bool   `json:"sync_pending,omitempty"`
	Approved       bool   `json:"approved"`
	LastError      string `json:"last_error,omitempty"`
}

// ShellInfos lists every installed package with shell state. Declared reports
// whether the current catalog still provides a shell block for the package.
func (a *App) ShellInfos() []ShellInfo {
	names := make([]string, 0, len(a.State.Shell))
	for name := range a.State.Shell {
		names = append(names, name)
	}
	sort.Strings(names)
	cat, _ := a.Catalog()
	out := make([]ShellInfo, 0, len(names))
	for _, name := range names {
		sh := a.State.Shell[name]
		if sh == nil {
			continue
		}
		info := shellInfoFor(name, sh)
		if cat != nil {
			entry, err := cat.Lookup(name)
			info.Declared = err == nil && entry.Pkg.Shell != nil
		} else {
			// Unknown is safer than claiming a broken catalog removed the
			// snippet; the catalog failure is reported separately.
			info.Declared = true
		}
		out = append(out, info)
	}
	return out
}

func shellInfoFor(name string, sh *state.Shell) ShellInfo {
	info := ShellInfo{
		Package:     name,
		Approved:    sh.Approved,
		Rejected:    sh.RejectedSHA256 != "",
		SyncPending: sh.SyncPending,
		LastError:   sh.LastError,
	}
	if sh.Live != nil {
		info.Live = true
		info.LiveVersion = sh.Live.Version
		info.LiveSHA256 = shellSnippetHash(sh.Live)
	}
	if sh.Pending != nil {
		info.Pending = true
		info.PendingVersion = sh.Pending.Version
		info.PendingSHA256 = sh.PendingSHA256
		info.Removal = isEmptySnippet(sh.Pending)
	}
	return info
}

func isEmptySnippet(s *state.ShellSnippet) bool {
	return s != nil && strings.TrimSpace(s.Bash) == "" && strings.TrimSpace(s.Pwsh) == ""
}

// PendingShellCount returns how many snippets await approval.
func (a *App) PendingShellCount() int {
	n := 0
	for _, sh := range a.State.Shell {
		if sh != nil && sh.Pending != nil {
			n++
		}
	}
	return n
}

// SyncShell re-resolves every installed package's shell snippet against the
// loaded lists. Changed snippets become pending and are never rendered; the
// previous approved snippet stays live. Packages in approveNew are approved on
// first sight, which is used by an explicit install.
func (a *App) SyncShell(ctx context.Context, approveNew map[string]bool) error {
	a.reloadConfig()
	cat, err := a.Catalog()
	if err != nil {
		return err
	}
	if !a.Cfg.AllowShellPackages {
		// Remember that catalog work is outstanding so enabling snippets
		// later re-resolves changes instead of silently keeping stale state.
		a.markShellDirty(cat)
		return nil
	}
	resolver := &shellinit.Resolver{
		Client:   a.Client,
		Attempts: max(1, a.Cfg.HTTP.Retries),
		CacheDir: a.Paths.ShellCache,
		Offline:  a.Offline,
		Warn: func(err error) {
			a.Log.Warn("could not cache shell snippet", "error", err)
		},
	}
	type job struct {
		name    string
		sh      *list.Shell
		version string
	}
	var jobs []job
	for _, name := range a.InstalledNames() {
		entry, err := cat.Lookup(name)
		if err != nil {
			if cat.Has(name) {
				a.markShellConflict(name, err)
			} else {
				a.queueShellRemoval(name, "")
			}
			continue
		}
		if entry.Pkg.Shell == nil {
			a.queueShellRemoval(name, shellVersion(entry.Pkg, a.State.Packages[name]))
			continue
		}
		jobs = append(jobs, job{
			name:    name,
			sh:      entry.Pkg.Shell,
			version: shellVersion(entry.Pkg, a.State.Packages[name]),
		})
	}

	type outcome struct {
		name    string
		version string
		bodies  shellinit.Bodies
		err     error
	}
	outcomes := make([]outcome, len(jobs))
	parallel := a.Cfg.Parallel
	if parallel < 1 {
		parallel = 1
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			bodies, err := resolver.Resolve(ctx, jobs[i].sh)
			outcomes[i] = outcome{name: jobs[i].name, version: jobs[i].version, bodies: bodies, err: err}
		}(i)
	}
	wg.Wait()

	// The kill switch may have been flipped while snippets were downloading.
	a.reloadConfig()
	if !a.Cfg.AllowShellPackages {
		a.markShellDirty(cat)
		return nil
	}

	for _, out := range outcomes {
		if out.err != nil {
			a.setShellError(out.name, out.err)
			a.Log.Warn("could not resolve shell snippet", "package", out.name, "error", out.err)
			a.Out.Warnf("could not resolve the shell snippet for %s: %s", out.name, shellinit.DisplayLine(out.err.Error()))
			continue
		}
		if a.applyShellCandidate(out.name, out.bodies, out.version, approveNew[out.name]) {
			a.Out.Warnf("%s: shell snippet change queued; run cpt shell diff %s", out.name, out.name)
		}
	}
	for name := range a.State.Shell {
		if _, ok := a.State.Packages[name]; !ok {
			delete(a.State.Shell, name)
		}
	}
	return nil
}

// markShellDirty records packages whose shell state must be re-resolved when
// shell packages are enabled again. It performs no network access.
func (a *App) markShellDirty(cat *list.Catalog) {
	for _, name := range a.InstalledNames() {
		needs := false
		if sh := a.State.Shell[name]; sh != nil {
			needs = true
		}
		if entry, err := cat.Lookup(name); err == nil && entry.Pkg.Shell != nil {
			needs = true
		}
		if !needs {
			continue
		}
		sh := a.State.Shell[name]
		if sh == nil {
			sh = &state.Shell{}
			a.State.Shell[name] = sh
		}
		sh.SyncPending = true
		sh.SyncAttempt = time.Time{}
	}
	for name := range a.State.Shell {
		if _, ok := a.State.Packages[name]; !ok {
			delete(a.State.Shell, name)
		}
	}
}

// shellSyncRetry bounds how often a failed background snippet sync is retried.
const shellSyncRetry = time.Minute

// reconcileLockTimeout is a variable so tests can exercise lock contention
// without waiting the production timeout.
var reconcileLockTimeout = 5 * time.Second

// markShellStateDirty marks existing shell state without consulting lists.
// Used when the catalog itself cannot be read while snippets are disabled.
func (a *App) markShellStateDirty() {
	for name, sh := range a.State.Shell {
		if sh == nil {
			continue
		}
		if _, ok := a.State.Packages[name]; !ok {
			continue
		}
		sh.SyncPending = true
		sh.SyncAttempt = time.Time{}
	}
}

func shellSyncDue(st *state.State, now time.Time) bool {
	for _, sh := range st.Shell {
		if sh == nil || !sh.SyncPending {
			continue
		}
		if sh.SyncAttempt.IsZero() || now.Sub(sh.SyncAttempt) >= shellSyncRetry {
			return true
		}
	}
	return false
}

// SyncPendingShellCount returns how many packages need a snippet re-sync.
func (a *App) SyncPendingShellCount() int {
	n := 0
	for _, sh := range a.State.Shell {
		if sh != nil && sh.SyncPending {
			n++
		}
	}
	return n
}

func shellVersion(pkg *list.Package, installed *state.Package) string {
	if pkg != nil && strings.TrimSpace(pkg.Version) != "" {
		return pkg.Version
	}
	if installed != nil {
		return installed.Version
	}
	return ""
}

// applyShellCandidate updates approval state. It returns true when a new
// pending change was queued for the user.
func (a *App) applyShellCandidate(name string, bodies shellinit.Bodies, version string, approveNew bool) bool {
	if a.State.Packages[name] == nil {
		return false
	}
	candidate := bodies.Hash()
	sh := a.State.Shell[name]
	if sh == nil {
		sh = &state.Shell{}
		a.State.Shell[name] = sh
	}
	sh.SyncPending = false
	sh.SyncAttempt = time.Time{}

	if sh.Live != nil && shellSnippetHash(sh.Live) == candidate {
		// Content is unchanged. A version bump only refreshes the marker.
		sh.Live.Version = version
		sh.SnippetSHA256 = candidate
		sh.Pending = nil
		sh.PendingSHA256 = ""
		sh.RejectedSHA256 = ""
		sh.LastError = ""
		sh.Approved = true
		return false
	}
	if sh.RejectedSHA256 == candidate {
		// The user explicitly rejected this exact candidate. Do not nag again
		// until the list-provided body changes; any older pending candidate is
		// stale for the current list.
		sh.Pending = nil
		sh.PendingSHA256 = ""
		sh.LastError = ""
		sh.Approved = sh.Live != nil
		return false
	}
	if sh.Live == nil && approveNew {
		sh.Live = &state.ShellSnippet{Version: version, Bash: bodies.Bash, Pwsh: bodies.Pwsh}
		sh.SnippetSHA256 = candidate
		sh.Pending = nil
		sh.PendingSHA256 = ""
		sh.RejectedSHA256 = ""
		sh.LastError = ""
		sh.Approved = true
		return false
	}
	sh.LastError = ""
	sh.Approved = false
	if sh.Pending == nil || sh.PendingSHA256 != candidate {
		sh.Pending = &state.ShellSnippet{Version: version, Bash: bodies.Bash, Pwsh: bodies.Pwsh}
		sh.PendingSHA256 = candidate
		sh.RejectedSHA256 = ""
		return true
	}
	sh.Pending.Version = version
	return false
}

// queueShellRemoval asks for approval before an installed package's approved
// snippet disappears from its list. The live snippet keeps running until the
// removal is approved or rejected.
func (a *App) queueShellRemoval(name, version string) {
	sh := a.State.Shell[name]
	if sh == nil {
		return
	}
	sh.SyncPending = false
	sh.SyncAttempt = time.Time{}
	if sh.Live == nil {
		// A first snippet that was never approved has nothing to remove.
		delete(a.State.Shell, name)
		return
	}
	candidate := shellinit.Bodies{}.Hash()
	if sh.RejectedSHA256 == candidate {
		sh.Pending = nil
		sh.PendingSHA256 = ""
		sh.LastError = ""
		sh.Approved = true
		return
	}
	sh.LastError = ""
	sh.Approved = false
	if sh.Pending == nil || sh.PendingSHA256 != candidate {
		sh.Pending = &state.ShellSnippet{Version: version}
		sh.PendingSHA256 = candidate
		sh.RejectedSHA256 = ""
		a.Out.Warnf("%s: shell snippet was removed from the list; run cpt shell diff %s", name, name)
	}
}

func (a *App) markShellConflict(name string, err error) {
	sh := a.State.Shell[name]
	if sh == nil {
		return
	}
	// A stale pending candidate must not remain approvable while the list is
	// ambiguous.
	sh.Pending = nil
	sh.PendingSHA256 = ""
	sh.LastError = err.Error()
	sh.SyncPending = false
	sh.SyncAttempt = time.Time{}
	sh.Approved = sh.Live != nil
}

func (a *App) setShellError(name string, err error) {
	sh := a.State.Shell[name]
	if sh == nil {
		sh = &state.Shell{}
		a.State.Shell[name] = sh
	}
	sh.LastError = err.Error()
	// A candidate that cannot be resolved against the current list must not
	// remain approvable, especially a removal queued from an older catalog.
	sh.Pending = nil
	sh.PendingSHA256 = ""
	sh.SyncAttempt = time.Now().UTC()
	// Keep SyncPending for state that was dirty while the kill switch was off;
	// ReconcileInit retries after a short backoff.
	sh.Approved = sh.Live != nil
}

// ApproveShell promotes pending snippets to live. Approval is explicit because
// snippets are code that runs in every new shell.
func (a *App) ApproveShell(names []string, all bool) ([]string, error) {
	if !a.Cfg.AllowShellPackages {
		return nil, cerr.New(cerr.Usage, "shell packages are disabled by allow_shell_packages=false")
	}
	selected, err := a.selectPendingShells(names, all)
	if err != nil {
		return nil, err
	}
	for _, name := range selected {
		sh := a.State.Shell[name]
		if isEmptySnippet(sh.Pending) {
			delete(a.State.Shell, name)
			continue
		}
		sh.Live = sh.Pending
		sh.SnippetSHA256 = sh.PendingSHA256
		if sh.SnippetSHA256 == "" {
			sh.SnippetSHA256 = shellSnippetHash(sh.Live)
		}
		sh.Pending = nil
		sh.PendingSHA256 = ""
		sh.RejectedSHA256 = ""
		sh.LastError = ""
		sh.SyncPending = false
		sh.SyncAttempt = time.Time{}
		sh.Approved = true
	}
	return selected, nil
}

// RejectShell discards pending snippets and keeps the live ones.
func (a *App) RejectShell(names []string, all bool) ([]string, error) {
	selected, err := a.selectPendingShells(names, all)
	if err != nil {
		return nil, err
	}
	for _, name := range selected {
		sh := a.State.Shell[name]
		rejected := sh.PendingSHA256
		if rejected == "" {
			rejected = shellSnippetHash(sh.Pending)
		}
		sh.RejectedSHA256 = rejected
		sh.Pending = nil
		sh.PendingSHA256 = ""
		sh.LastError = ""
		if a.Cfg.AllowShellPackages {
			sh.SyncPending = false
			sh.SyncAttempt = time.Time{}
		} else {
			// A change may have been made while the switch was off; keep the
			// dirty marker so re-enabling re-resolves it.
			sh.SyncPending = true
			sh.SyncAttempt = time.Time{}
		}
		sh.Approved = sh.Live != nil
	}
	return selected, nil
}

// ResetShellRejection clears remembered rejections and re-resolves the current
// list snippet so the user can review it again.
func (a *App) ResetShellRejection(ctx context.Context, names []string, all bool) ([]string, error) {
	var selected []string
	if all {
		for name, sh := range a.State.Shell {
			if sh != nil && sh.RejectedSHA256 != "" {
				selected = append(selected, name)
			}
		}
		sort.Strings(selected)
	} else {
		selected = dedupe(names)
		if len(selected) == 0 {
			return nil, cerr.New(cerr.Usage, "specify at least one package or pass --all")
		}
		sort.Strings(selected)
		for _, name := range selected {
			sh := a.State.Shell[name]
			if sh == nil || sh.RejectedSHA256 == "" {
				return nil, fmt.Errorf("%s has no rejected shell snippet", name)
			}
		}
	}
	for _, name := range selected {
		a.State.Shell[name].RejectedSHA256 = ""
		a.State.Shell[name].SyncAttempt = time.Time{}
	}
	if err := a.SyncShell(ctx, nil); err != nil {
		return nil, err
	}
	return selected, nil
}

func (a *App) selectPendingShells(names []string, all bool) ([]string, error) {
	if all {
		selected := make([]string, 0, len(a.State.Shell))
		for name, sh := range a.State.Shell {
			if sh != nil && sh.Pending != nil {
				selected = append(selected, name)
			}
		}
		sort.Strings(selected)
		return selected, nil
	}
	nameList := dedupe(names)
	if len(nameList) == 0 {
		return nil, cerr.New(cerr.Usage, "specify at least one package or pass --all")
	}
	sort.Strings(nameList)
	for _, name := range nameList {
		sh := a.State.Shell[name]
		if sh == nil || sh.Pending == nil {
			return nil, fmt.Errorf("%s has no pending shell snippet", name)
		}
	}
	return nameList, nil
}

// ShellDiff renders the pending change for one package, or an empty string
// when nothing is pending.
func (a *App) ShellDiff(name string) (string, error) {
	sh := a.State.Shell[name]
	if sh == nil {
		return "", fmt.Errorf("%s has no shell snippet", name)
	}
	if sh.Pending == nil {
		return "", nil
	}
	var oldBash, oldPwsh string
	if sh.Live != nil {
		oldBash, oldPwsh = sh.Live.Bash, sh.Live.Pwsh
	}
	var out strings.Builder
	if d := shellinit.UnifiedDiff(oldBash, sh.Pending.Bash, "bash (live)", "bash (pending)"); d != "" {
		out.WriteString(d)
	}
	if d := shellinit.UnifiedDiff(oldPwsh, sh.Pending.Pwsh, "pwsh (live)", "pwsh (pending)"); d != "" {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(d)
	}
	return out.String(), nil
}

// ReconcileInit refreshes generated cptinit scripts when they are stale,
// including after a config change such as the shell-package kill switch. It
// takes the state lock and reloads state so a read-only command cannot clobber
// a newer script written by a concurrent install.
func (a *App) ReconcileInit(ctx context.Context) error {
	now := time.Now()
	needInit := a.initStale(a.State)
	needSync := a.Cfg.AllowShellPackages && shellSyncDue(a.State, now)
	if !needInit && !needSync {
		return nil
	}
	lock, err := state.Acquire(a.Paths.LockFile, reconcileLockTimeout)
	if err != nil {
		// Another cpt is mid-operation. Leave the repair for the next command,
		// but surface unsynced state for this one.
		if !a.Cfg.AllowShellPackages {
			if cat, catErr := a.Catalog(); catErr == nil {
				a.markShellDirty(cat)
			} else {
				a.Log.Warn("could not inspect lists while shell packages are disabled", "error", catErr)
				a.markShellStateDirty()
			}
		}
		return fmt.Errorf("lock state for shell init: %w", err)
	}
	defer lock.Release()
	a.reloadConfig()
	st, err := state.Load(a.Paths.StateFile)
	if err != nil {
		return err
	}
	a.ensureCoreList(st)
	a.State = st
	if !a.Cfg.AllowShellPackages {
		if cat, catErr := a.Catalog(); catErr == nil {
			a.markShellDirty(cat)
		} else {
			a.Log.Warn("could not inspect lists while shell packages are disabled", "error", catErr)
			a.markShellStateDirty()
		}
	} else if shellSyncDue(st, time.Now()) {
		// Snippet state changed while the kill switch was off. Resolve it now
		// that snippets are enabled again.
		if err := a.SyncShell(ctx, nil); err != nil {
			a.Log.Warn("could not sync shell snippets", "error", err)
		}
		a.reloadConfig()
		if !a.Cfg.AllowShellPackages {
			if cat, catErr := a.Catalog(); catErr == nil {
				a.markShellDirty(cat)
			} else {
				a.Log.Warn("could not inspect lists while shell packages are disabled", "error", catErr)
				a.markShellStateDirty()
			}
		}
	}
	saveErr := a.State.Save(a.Paths.StateFile)
	// Regenerate even when state could not be saved: the kill switch removes
	// snippets based on config alone and is security-relevant.
	var regenErr error
	if a.initStale(a.State) {
		regenErr = a.RegenerateInit()
	}
	if saveErr != nil {
		if regenErr != nil {
			a.Log.Error("shell init regeneration also failed", "error", regenErr)
		}
		return saveErr
	}
	return regenErr
}

func (a *App) initStale(st *state.State) bool {
	opt := a.ShellInitOptions()
	for _, fl := range []shellinit.Flavor{shellinit.FlavorBash, shellinit.FlavorPwsh} {
		path := a.InitScriptPath(fl)
		want := shellinit.Render(fl, opt, a.shellBlocksFrom(st, fl))
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return true
		}
		if err != nil {
			return true
		}
		if !looksGenerated(data) {
			continue
		}
		if shellinit.DecodeScript(data) != want {
			return true
		}
		// PowerShell 5.1 needs the BOM even when the contents are otherwise
		// identical, so a script written by an older cpt must be upgraded.
		if fl == shellinit.FlavorPwsh && !shellinit.HasUTF8BOM(data) {
			return true
		}
	}
	return false
}

// RegenerateInit writes cptinit.sh and cptinit.ps1 when their content changed.
// Existing files that cpt did not generate are left untouched.
func (a *App) RegenerateInit() error {
	opt := a.ShellInitOptions()
	for _, fl := range []shellinit.Flavor{shellinit.FlavorBash, shellinit.FlavorPwsh} {
		script := shellinit.Render(fl, opt, a.shellBlocks(fl))
		data := shellinit.ScriptBytes(fl, script)
		path := a.InitScriptPath(fl)
		existing, err := os.ReadFile(path)
		switch {
		case err == nil && shellinit.DecodeScript(existing) == script:
			if fl != shellinit.FlavorPwsh || shellinit.HasUTF8BOM(existing) {
				continue
			}
			if err := fsutil.WriteFileAtomic(path, data, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
		case err == nil && !looksGenerated(existing):
			a.Log.Warn("shell init file was not generated by cpt; leaving it alone", "path", path)
			continue
		case err == nil:
			if err := fsutil.WriteFileAtomic(path, data, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
		case os.IsNotExist(err):
			if err := writeInitExclusive(path, data); err != nil {
				return err
			}
		default:
			return fmt.Errorf("read %s: %w", path, err)
		}
	}
	return nil
}

func looksGenerated(data []byte) bool {
	text := strings.TrimPrefix(string(data), "\ufeff")
	return strings.TrimSpace(text) == "" || strings.HasPrefix(text, "# generated by cpt")
}

// writeInitExclusive creates a new script only if the path does not exist. It
// writes a temp file first and links it into place so a concurrently starting
// shell never sources a partial script.
func writeInitExclusive(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := os.Link(tmp, path); err != nil {
		if os.IsExist(err) {
			if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
				// A dangling symlink occupies the name but has no target.
				if rmErr := os.Remove(path); rmErr != nil {
					return fmt.Errorf("replace dangling %s: %w", path, rmErr)
				}
				if linkErr := os.Link(tmp, path); linkErr == nil || os.IsExist(linkErr) {
					return nil
				} else {
					return fmt.Errorf("create %s: %w", path, linkErr)
				}
			}
			return nil
		}
		// Filesystems without hard links: fall back to O_EXCL, which still
		// never overwrites an existing file. The write is not atomic on this
		// uncommon fallback.
		out, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if openErr != nil {
			if os.IsExist(openErr) {
				return nil
			}
			return fmt.Errorf("create %s: %w", path, openErr)
		}
		_, writeErr := out.Write(data)
		closeErr := out.Close()
		if writeErr != nil {
			_ = os.Remove(path)
			return fmt.Errorf("write %s: %w", path, writeErr)
		}
		if closeErr != nil {
			_ = os.Remove(path)
			return fmt.Errorf("close %s: %w", path, closeErr)
		}
	}
	return nil
}

// ShellInitOptions describes the paths embedded in generated scripts.
func (a *App) ShellInitOptions() shellinit.Options {
	home, _ := shellinit.HomeDir()
	return shellinit.Options{Root: a.Paths.Root, Bin: a.BinDir(), Home: home}
}

// InitScriptPath returns the generated script location for a flavor.
func (a *App) InitScriptPath(fl shellinit.Flavor) string {
	return filepath.Join(a.Paths.Root, shellinit.ScriptName(fl))
}

// InitScript renders the current script without writing it to disk.
func (a *App) InitScript(fl shellinit.Flavor) string {
	return shellinit.Render(fl, a.ShellInitOptions(), a.shellBlocks(fl))
}

func (a *App) shellBlocks(fl shellinit.Flavor) []shellinit.Block {
	return a.shellBlocksFrom(a.State, fl)
}

func (a *App) shellBlocksFrom(st *state.State, fl shellinit.Flavor) []shellinit.Block {
	if !a.Cfg.AllowShellPackages || st == nil {
		return nil
	}
	names := make([]string, 0, len(st.Packages))
	for name := range st.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	blocks := make([]shellinit.Block, 0, len(names))
	for _, name := range names {
		sh := st.Shell[name]
		if sh == nil || sh.Live == nil {
			continue
		}
		body := sh.Live.Bash
		if fl == shellinit.FlavorPwsh {
			body = sh.Live.Pwsh
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		version := sh.Live.Version
		if version == "" && st.Packages[name] != nil {
			version = st.Packages[name].Version
		}
		blocks = append(blocks, shellinit.Block{
			Name:    name,
			Version: version,
			SHA256:  shellSnippetHash(sh.Live),
			Body:    body,
		})
	}
	return blocks
}

func shellSnippetHash(s *state.ShellSnippet) string {
	if s == nil {
		return ""
	}
	return shellinit.Bodies{Bash: s.Bash, Pwsh: s.Pwsh}.Hash()
}
