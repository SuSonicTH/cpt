// Package app wires configuration, state, lists and the network together.
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cpt "github.com/SuSonicTH/cpt"
	"github.com/SuSonicTH/cpt/internal/config"
	"github.com/SuSonicTH/cpt/internal/httpc"
	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/logx"
	"github.com/SuSonicTH/cpt/internal/out"
	"github.com/SuSonicTH/cpt/internal/source"
	"github.com/SuSonicTH/cpt/internal/state"
)

// Options configures opening an App.
type Options struct {
	Context context.Context
	Root    string
	Offline bool
	Verbose bool
	Quiet   bool
	JSON    bool
	NoColor bool
	Color   bool
	Stdout  io.Writer
	Stderr  io.Writer
}

// App is the shared runtime for every command.
type App struct {
	Paths   config.Paths
	Cfg     *config.Config
	State   *state.State
	Client  *http.Client
	Prober  *source.Prober
	Out     *out.Printer
	Log     *slog.Logger
	Offline bool

	closer func()
}

// Open loads configuration and state, creating the directory tree if needed.
func Open(opts Options) (*App, error) {
	root := opts.Root
	if strings.TrimSpace(root) == "" {
		var err error
		root, err = config.DefaultRoot()
		if err != nil {
			return nil, err
		}
	}
	paths := config.NewPaths(root)
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return nil, err
	}
	logger, closer := logx.New(paths.Logs, logx.Options{Verbose: opts.Verbose, Quiet: opts.Quiet, Level: cfg.Logs.Level})

	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	a := &App{
		Paths:   paths,
		Cfg:     cfg,
		Client:  httpc.New(cfg.HTTP),
		Out:     out.New(stdout, stderr, opts.JSON, opts.Color, opts.Quiet),
		Log:     logger,
		Offline: opts.Offline,
		closer:  func() { _ = closer.Close() },
	}
	a.Prober = &source.Prober{
		Client:   httpc.NewNoRedirect(cfg.HTTP),
		CacheDir: paths.ProbeCache,
		TTL:      cfg.ProbeTTL(),
		Attempts: cfg.HTTP.Retries,
		Offline:  opts.Offline,
		Log:      logger,
	}
	if err := list.EnsureBuiltin(paths.Lists, cpt.BuiltinList()); err != nil {
		return nil, err
	}
	if err := a.loadState(); err != nil {
		return nil, err
	}
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.ReconcileInit(ctx); err != nil {
		logger.Warn("could not reconcile shell init files", "error", err)
	}
	return a, nil
}

// Close releases resources held by the app.
func (a *App) Close() {
	if a.closer != nil {
		a.closer()
	}
}

// BinDir returns the effective binary directory.
func (a *App) BinDir() string { return a.Cfg.BinDirFor(a.Paths) }

func (a *App) loadState() error {
	st, err := state.Load(a.Paths.StateFile)
	if err != nil {
		return err
	}
	a.ensureCoreList(st)
	a.State = st
	return nil
}

func (a *App) ensureCoreList(st *state.State) {
	if _, ok := st.Lists["core"]; ok {
		return
	}
	packages := 0
	if l, err := list.Parse(cpt.BuiltinList(), "builtin"); err == nil {
		packages = len(l.Packages)
	}
	st.Lists["core"] = &state.List{
		Origin:    cpt.BuiltinListOrigin,
		Builtin:   true,
		FetchedAt: time.Now().UTC(),
		Packages:  packages,
	}
}

// reloadConfig picks up edits made while a long operation was running.
func (a *App) reloadConfig() {
	cfg, err := config.Load(a.Paths.ConfigFile)
	if err != nil {
		a.Log.Warn("could not reload config", "error", err)
		return
	}
	a.Cfg = cfg
}

// WithLock runs fn while holding the state lock, reloading state first and
// saving it afterwards. State is saved even when fn fails so that partial
// progress (for example one package out of several) is not lost.
func (a *App) WithLock(fn func() error) error {
	lock, err := state.Acquire(a.Paths.LockFile, 5*time.Second)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := a.loadState(); err != nil {
		return err
	}
	// No other cpt process can be staging files while we hold the lock.
	if err := os.RemoveAll(a.Paths.Tmp); err == nil {
		_ = os.MkdirAll(a.Paths.Tmp, 0o755)
	}
	fnErr := fn()
	// Apply a config change made while this operation was running before
	// saving, so the kill switch also wins over shell decisions made by fn.
	a.reloadConfig()
	if !a.Cfg.AllowShellPackages {
		if cat, catErr := a.Catalog(); catErr == nil {
			a.markShellDirty(cat)
		} else {
			a.Log.Warn("could not inspect lists while shell packages are disabled", "error", catErr)
			a.markShellStateDirty()
		}
	}
	saveErr := a.State.Save(a.Paths.StateFile)
	// Regenerate even when state could not be saved: the kill switch removes
	// snippets based on config alone and is security-relevant.
	if initErr := a.RegenerateInit(); initErr != nil {
		// Report the stale script but do not turn an otherwise successful
		// pin/list/install into a failure.
		a.Log.Error("could not write shell init files", "error", initErr)
		a.Out.Warnf("could not update shell init files: %v", initErr)
	}
	if fnErr != nil {
		if saveErr != nil {
			a.Log.Error("could not save state after failure", "error", saveErr)
		}
		return fnErr
	}
	return saveErr
}

// Catalog loads every list file from disk and merges them.
func (a *App) Catalog() (*list.Catalog, error) {
	dirEntries, err := os.ReadDir(a.Paths.Lists)
	if err != nil {
		return nil, fmt.Errorf("read lists directory: %w", err)
	}
	var named []list.Named
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(a.Paths.Lists, name)
		l, _, err := list.ReadFile(path)
		if err != nil {
			return nil, err
		}
		named = append(named, list.Named{Name: strings.TrimSuffix(name, ".json"), List: l})
	}
	sort.Slice(named, func(i, j int) bool { return named[i].Name < named[j].Name })
	return list.BuildCatalog(named)
}

// InstalledNames returns installed package names sorted alphabetically.
func (a *App) InstalledNames() []string {
	names := make([]string, 0, len(a.State.Packages))
	for name := range a.State.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LoadedLists returns the list identifiers that have a file on disk.
func (a *App) LoadedLists() ([]string, error) {
	entries, err := os.ReadDir(a.Paths.Lists)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

// ListsStale reports whether any writable list needs a refresh.
func (a *App) ListsStale() bool {
	for name, meta := range a.State.Lists {
		if meta == nil || meta.Readonly {
			continue
		}
		if meta.FetchedAt.IsZero() || time.Since(meta.FetchedAt) >= a.Cfg.ListTTL() {
			return true
		}
		if _, err := os.Stat(filepath.Join(a.Paths.Lists, list.FileName(name))); err != nil {
			return true
		}
	}
	return false
}

// MaybeRefresh refreshes stale lists, logging rather than failing. syncShell
// controls whether installed shell snippets are re-resolved; install and
// update skip it here because they sync after their own state mutation.
func (a *App) MaybeRefresh(ctx context.Context, force, syncShell bool) {
	if a.Offline {
		return
	}
	names := make([]string, 0, len(a.State.Lists))
	for name := range a.State.Lists {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		meta := a.State.Lists[name]
		if meta == nil || meta.Readonly {
			continue
		}
		file := filepath.Join(a.Paths.Lists, list.FileName(name))
		if !force && !meta.FetchedAt.IsZero() && time.Since(meta.FetchedAt) < a.Cfg.ListTTL() {
			if _, err := os.Stat(file); err == nil {
				continue
			}
		}
		if err := a.refreshList(ctx, name, meta, force); err != nil {
			a.Log.Warn("list refresh failed", "list", name, "error", err)
			a.Out.Warnf("could not refresh list %s: %v", name, err)
		}
	}
	if syncShell {
		if err := a.SyncShell(ctx, nil); err != nil {
			a.Log.Warn("could not sync shell snippets after refresh", "error", err)
		}
	}
}
