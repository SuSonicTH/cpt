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
		Offline:  opts.Offline,
		Log:      logger,
	}
	if err := list.EnsureBuiltin(paths.Lists, cpt.BuiltinList()); err != nil {
		return nil, err
	}
	if err := a.loadState(); err != nil {
		return nil, err
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
	if _, ok := st.Lists["core"]; !ok {
		st.Lists["core"] = &state.List{
			Origin:    cpt.BuiltinListOrigin,
			Builtin:   true,
			FetchedAt: time.Now().UTC(),
		}
	}
	a.State = st
	return nil
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
	fnErr := fn()
	saveErr := a.State.Save(a.Paths.StateFile)
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

// MaybeRefresh refreshes stale lists, logging rather than failing.
func (a *App) MaybeRefresh(ctx context.Context, force bool) {
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
}
