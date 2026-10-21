package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SuSonicTH/cpt/internal/archive"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/fetch"
	"github.com/SuSonicTH/cpt/internal/httpc"
	"github.com/SuSonicTH/cpt/internal/install"
	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/platform"
	"github.com/SuSonicTH/cpt/internal/progress"
	"github.com/SuSonicTH/cpt/internal/resolve"
	"github.com/SuSonicTH/cpt/internal/shellinit"
	"github.com/SuSonicTH/cpt/internal/state"
)

// InstallOptions controls install and update runs.
type InstallOptions struct {
	Force         bool
	ForceChecksum bool
	DryRun        bool
	SkipCurrent   bool
	// ApproveNewShell approves a package's first shell snippet as part of an
	// explicit install. Changed snippets always require separate approval.
	ApproveNewShell bool
}

// Result describes the outcome for one package.
type Result struct {
	Name     string   `json:"name"`
	List     string   `json:"list,omitempty"`
	Version  string   `json:"version,omitempty"`
	Previous string   `json:"previous,omitempty"`
	Action   string   `json:"action"`
	Tag      string   `json:"tag,omitempty"`
	Asset    string   `json:"asset,omitempty"`
	URL      string   `json:"url,omitempty"`
	Bins     []string `json:"bins,omitempty"`
	Err      error    `json:"-"`
	ErrText  string   `json:"error,omitempty"`
}

type request struct {
	entry   list.Entry
	version string
	pin     string
}

// Install resolves and installs the given package specs (name or name@version).
func (a *App) Install(ctx context.Context, specs []string, opt InstallOptions) ([]Result, error) {
	if len(specs) == 0 {
		return nil, cerr.New(cerr.Usage, "no packages given")
	}
	cat, err := a.Catalog()
	if err != nil {
		return nil, err
	}
	reqs := make([]request, 0, len(specs))
	for _, spec := range specs {
		name, version := splitSpec(spec)
		if name == "" {
			return nil, cerr.New(cerr.Usage, "invalid package spec %q", spec)
		}
		entry, err := cat.Lookup(name)
		if err != nil {
			return nil, err
		}
		pin := ""
		if p, ok := a.State.Packages[name]; ok {
			pin = p.Pinned
		}
		if version != "" {
			if err := list.ValidateVersion(version); err != nil {
				return nil, cerr.New(cerr.Usage, "%s: %v", name, err)
			}
		}
		reqs = append(reqs, request{entry: entry, version: version, pin: pin})
	}
	return a.installRequests(ctx, reqs, opt)
}

// Update installs newer versions of the named packages, or of every installed
// package when names is empty.
func (a *App) Update(ctx context.Context, names []string, force, dry bool) ([]Result, error) {
	cat, err := a.Catalog()
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		names = a.InstalledNames()
	}
	if len(names) == 0 {
		return nil, nil
	}
	var results []Result
	var reqs []request
	for _, name := range names {
		p, ok := a.State.Packages[name]
		if !ok {
			err := fmt.Errorf("%s is not installed", name)
			results = append(results, Result{Name: name, Action: "failed", Err: err, ErrText: err.Error()})
			a.emit(progress.Event{Kind: progress.KindFailed, Package: name, Message: err.Error()})
			continue
		}
		if p.Pinned != "" && !force {
			results = append(results, Result{Name: name, Version: p.Version, Action: "skipped (pinned)"})
			a.emit(progress.Event{Kind: progress.KindDone, Package: name, Version: p.Version, Action: "skipped (pinned)"})
			continue
		}
		if meta := a.State.Lists[p.List]; meta != nil && meta.Readonly {
			results = append(results, Result{Name: name, Version: p.Version, Action: "skipped (frozen)"})
			a.emit(progress.Event{Kind: progress.KindDone, Package: name, Version: p.Version, Action: "skipped (frozen)"})
			continue
		}
		entry, err := cat.Lookup(name)
		if err != nil {
			results = append(results, Result{Name: name, Action: "failed", Err: err, ErrText: err.Error()})
			a.emit(progress.Event{Kind: progress.KindFailed, Package: name, Message: err.Error()})
			continue
		}
		pin := p.Pinned
		if force {
			pin = ""
		}
		reqs = append(reqs, request{entry: entry, pin: pin})
	}
	updated, err := a.installRequests(ctx, reqs, InstallOptions{Force: force, DryRun: dry, SkipCurrent: !force})
	results = append(results, updated...)
	if err != nil {
		return results, err
	}
	return results, firstResultError(results)
}

// Remove deletes the named packages and their binaries.
func (a *App) Remove(ctx context.Context, names []string, force bool) ([]Result, error) {
	if len(names) == 0 {
		return nil, cerr.New(cerr.Usage, "no packages given")
	}
	names = dedupe(names)
	for _, name := range names {
		if _, ok := a.State.Packages[name]; !ok {
			return nil, fmt.Errorf("%s is not installed", name)
		}
	}
	type planned struct {
		path   string
		bin    string
		exists bool
	}
	plans := make([][]planned, len(names))
	for i, name := range names {
		p := a.State.Packages[name]
		if p == nil {
			continue
		}
		for bin, want := range p.Bins {
			owner, ok := a.State.Owner(bin)
			if !ok || owner != name {
				continue
			}
			path := filepath.Join(a.BinDir(), bin)
			got, err := install.FileSHA256(path)
			if err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("hash %s: %w", path, err)
			}
			if err == nil && want != "" && got != want && !force {
				return nil, fmt.Errorf("%s was modified since installation; use --force to remove it", path)
			}
			plans[i] = append(plans[i], planned{path: path, bin: bin, exists: err == nil})
		}
	}

	var results []Result
	for i, name := range names {
		p := a.State.Packages[name]
		if p == nil {
			continue
		}
		a.emit(progress.Event{Kind: progress.KindRemoving, Package: name, Version: p.Version})
		for _, d := range plans[i] {
			if d.exists {
				if err := os.Remove(d.path); err != nil && !os.IsNotExist(err) {
					// Keep the failed bin tracked so a retry can remove it.
					a.emit(progress.Event{Kind: progress.KindFailed, Package: name, Message: err.Error()})
					return results, fmt.Errorf("remove %s: %w", d.path, err)
				}
			}
			delete(p.Bins, d.bin)
			delete(a.State.Provides, d.bin)
		}
		a.State.Remove(name)
		results = append(results, Result{Name: name, Version: p.Version, Action: "removed"})
		a.emit(progress.Event{Kind: progress.KindDone, Package: name, Version: p.Version, Action: "removed"})
	}
	return results, nil
}

// Pin fixes an installed package to a version. The version is stored as given
// and reconstructed into a tag on the next install.
func (a *App) Pin(name, version string) error {
	p, ok := a.State.Packages[name]
	if !ok {
		return fmt.Errorf("%s is not installed", name)
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return cerr.New(cerr.Usage, "a version is required, for example cpt pin %s@1.2.3", name)
	}
	if err := list.ValidateVersion(version); err != nil {
		return cerr.New(cerr.Usage, "%v", err)
	}
	p.Pinned = version
	return nil
}

// Unpin removes a version pin.
func (a *App) Unpin(name string) error {
	p, ok := a.State.Packages[name]
	if !ok {
		return fmt.Errorf("%s is not installed", name)
	}
	p.Pinned = ""
	return nil
}

type staged struct {
	res    *resolve.Resolution
	dir    string
	asset  string
	sha    string
	status string
	source string
	bins   []archive.Bin
	action string
	err    error
}

func (a *App) installRequests(ctx context.Context, reqs []request, opt InstallOptions) ([]Result, error) {
	results := make([]Result, len(reqs))
	for i := range reqs {
		results[i] = Result{Name: reqs[i].entry.Pkg.Name, List: reqs[i].entry.List}
		a.emit(progress.Event{
			Kind:    progress.KindQueued,
			Package: reqs[i].entry.Pkg.Name,
			Version: reqs[i].version,
		})
	}
	if len(reqs) == 0 {
		// Update may have nothing to install (all pinned or their lists are
		// gone), but list changes still need to be reflected in shell state.
		if !opt.DryRun {
			if err := a.SyncShell(ctx, nil); err != nil {
				a.Log.Warn("could not sync shell snippets", "error", err)
			}
		}
		return results, nil
	}
	plat := platform.Current()

	previous := make(map[string]*state.Package, len(a.State.Packages))
	for name, p := range a.State.Packages {
		cp := *p
		previous[name] = &cp
	}

	stages := make([]*staged, len(reqs))
	parallel := a.Cfg.Parallel
	if parallel < 1 {
		parallel = 1
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			stages[i] = a.stage(ctx, reqs[i], plat, previous, opt)
		}(i)
	}
	wg.Wait()

	manager := &install.Manager{BinDir: a.BinDir(), State: a.State, Force: opt.Force}
	for i, st := range stages {
		r := &results[i]
		if st == nil {
			r.Action = "failed"
			r.Err = errors.New("internal error: missing result")
			r.ErrText = r.Err.Error()
			a.emit(progress.Event{Kind: progress.KindFailed, Package: r.Name, Message: r.ErrText})
			continue
		}
		if st.err != nil {
			r.Action = "failed"
			r.Err = st.err
			r.ErrText = st.err.Error()
			a.emit(progress.Event{Kind: progress.KindFailed, Package: r.Name, Message: r.ErrText})
			continue
		}
		res := st.res
		r.Version = res.Version
		r.Tag = res.Tag
		r.Asset = res.Asset
		r.URL = res.URL
		if prev := previous[res.Name]; prev != nil {
			r.Previous = prev.Version
		}
		if st.action != "" {
			r.Action = st.action
			a.emit(progress.Event{Kind: progress.KindDone, Package: r.Name, Version: r.Version, Action: r.Action})
			continue
		}
		if opt.DryRun {
			if previous[res.Name] == nil {
				r.Action = "would install"
			} else {
				r.Action = "would update"
			}
			a.emit(progress.Event{Kind: progress.KindDone, Package: r.Name, Version: r.Version, Action: r.Action})
			continue
		}
		a.emit(progress.Event{Kind: progress.KindInstalling, Package: res.Name, Version: res.Version})
		installed, err := manager.Place(res.Name, st.bins)
		if err != nil {
			r.Action = "failed"
			r.Err = err
			r.ErrText = err.Error()
			a.emit(progress.Event{Kind: progress.KindFailed, Package: r.Name, Message: r.ErrText})
			continue
		}
		r.Bins = sortedKeys(installed)
		switch {
		case previous[res.Name] == nil:
			r.Action = "installed"
		case previous[res.Name].Version == res.Version:
			r.Action = "reinstalled"
		default:
			r.Action = "updated"
		}
		pin := ""
		if prev := previous[res.Name]; prev != nil {
			pin = prev.Pinned
		}
		a.State.Packages[res.Name] = &state.Package{
			Tag:            res.Tag,
			Version:        res.Version,
			List:           res.List,
			SourceID:       res.SourceID,
			Asset:          res.Asset,
			DownloadSHA256: st.sha,
			ChecksumStatus: st.status,
			ChecksumSource: st.source,
			Bins:           installed,
			Pinned:         pin,
			Origin:         "install",
			InstalledAt:    time.Now().UTC(),
		}
		a.emit(progress.Event{Kind: progress.KindDone, Package: r.Name, Version: r.Version, Action: r.Action})
	}
	for _, w := range manager.Warnings {
		a.Out.Warnf("%s", w)
	}
	for _, st := range stages {
		if st != nil && st.dir != "" {
			_ = os.RemoveAll(st.dir)
		}
	}
	if !opt.DryRun {
		approve := map[string]bool{}
		if opt.ApproveNewShell {
			for _, r := range results {
				switch r.Action {
				case "installed", "updated", "reinstalled", "up-to-date":
					approve[r.Name] = true
				}
			}
		}
		if err := a.SyncShell(ctx, approve); err != nil {
			a.Log.Warn("could not sync shell snippets", "error", err)
		}
	}
	return results, firstResultError(results)
}

// downloadAsset downloads res.URL with the latest/download fallback for
// version-less, unpinned assets. It is shared by install and bundle build so
// both paths handle release assets identically.
func (a *App) downloadAsset(ctx context.Context, res *resolve.Resolution, down *fetch.Downloader, dest string) (string, error) {
	sha, _, err := down.To(ctx, res.URL, dest, res.Name)
	if err != nil && errors.Is(err, httpc.ErrNotFound) && !res.AssetVersioned && !res.Pinned && res.Canonical != "" {
		alt := a.Prober.LatestAssetURL(res.Canonical, res.Asset)
		if alt != res.URL {
			if altSHA, _, altErr := down.To(ctx, alt, dest, res.Name); altErr == nil {
				sha, err = altSHA, nil
				res.URL = alt
				if tagErr := a.adoptLatestTag(ctx, res); tagErr != nil {
					return "", fmt.Errorf("%s: %w", res.Name, tagErr)
				}
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", res.Name, err)
	}
	return sha, nil
}

// adoptLatestTag refreshes the resolved tag after a latest/download fallback,
// so state and checksum candidates refer to the bytes actually downloaded.
func (a *App) adoptLatestTag(ctx context.Context, res *resolve.Resolution) error {
	probe := *a.Prober
	probe.NoCache = true
	latest, err := probe.Latest(ctx, res.Repo)
	if err != nil {
		return fmt.Errorf("resolve the final tag after fallback: %w", err)
	}
	if latest.Tag == "" {
		return fmt.Errorf("resolve the final tag after fallback: empty tag for %s", res.Repo)
	}
	res.Canonical = latest.Canonical
	res.SourceID = "github:" + latest.Canonical
	if latest.Tag == res.Tag {
		return nil
	}
	res.Tag = latest.Tag
	res.Version = resolve.VersionOf(latest.Tag, res.Entry.Source.VersionStrip)
	res.URL = a.Prober.ReleaseURL(latest.Canonical, latest.Tag, res.Asset)
	res.Checksums = resolve.ChecksumCandidates(res.Entry, &probe, res)
	return nil
}

func (a *App) stage(ctx context.Context, req request, plat platform.Platform, previous map[string]*state.Package, opt InstallOptions) *staged {
	name := req.entry.Pkg.Name
	prev := previous[name]
	requested := req.version
	if requested == "" {
		requested = req.pin
	}
	if requested == "" {
		requested = strings.TrimSpace(req.entry.Pkg.Version)
	}

	// Declaring a current, intact install up-to-date must not require the
	// network, but only an explicit version (or offline mode) may skip the
	// probe. Without a requested version cpt must still look for updates.
	if prev != nil && opt.SkipCurrent && !opt.Force {
		exact := requested != "" && (requested == prev.Version || requested == prev.Tag)
		switch {
		case exact && a.installedCurrent(prev):
			return &staged{
				res:    &resolve.Resolution{Name: name, List: req.entry.List, Version: prev.Version, Tag: prev.Tag},
				action: "up-to-date",
			}
		case exact && a.Offline:
			return &staged{err: fmt.Errorf("%s: offline and the installed copy is damaged; reconnect to repair it", name)}
		case requested == "" && a.Offline:
			if a.installedCurrent(prev) {
				return &staged{
					res:    &resolve.Resolution{Name: name, List: req.entry.List, Version: prev.Version, Tag: prev.Tag},
					action: "up-to-date",
				}
			}
			return &staged{err: fmt.Errorf("%s: offline and the installed copy is damaged; reconnect to repair it", name)}
		}
	}

	resolveOpt := resolve.Options{CLIVersion: req.version, UserPin: req.pin}
	if prev != nil {
		resolveOpt.InstalledTag = prev.Tag
		resolveOpt.InstalledVer = prev.Version
	}
	a.emit(progress.Event{Kind: progress.KindResolving, Package: name})
	res, err := resolve.Package(ctx, a.Prober, req.entry, plat, resolveOpt)
	if err != nil {
		return &staged{err: fmt.Errorf("%s: %w", name, err)}
	}
	st := &staged{res: res}
	if prev != nil && opt.SkipCurrent && prev.Version == res.Version && !opt.Force {
		if a.installedCurrent(prev) {
			st.action = "up-to-date"
			return st
		}
	}
	if opt.DryRun {
		return st
	}
	if a.Offline {
		st.err = fmt.Errorf("%s: offline mode cannot download %s", res.Name, res.URL)
		return st
	}

	dir, err := os.MkdirTemp(a.Paths.Tmp, "stage-*")
	if err != nil {
		st.err = err
		return st
	}
	st.dir = dir
	assetPath := filepath.Join(dir, safeAssetName(res.Asset))
	var lastProgress time.Time
	down := &fetch.Downloader{
		Client:   a.Client,
		Attempts: max(1, a.Cfg.HTTP.Retries),
		Progress: func(label string, done, total int64) {
			// Bound the event rate: a fast download would otherwise flood the
			// TUI and starve terminal events.
			now := time.Now()
			// Throttle both known- and unknown-length downloads; a known final
			// chunk is always reported so the UI can reach 100%.
			if now.Sub(lastProgress) < 100*time.Millisecond && (total <= 0 || done < total) {
				return
			}
			lastProgress = now
			a.emit(progress.Event{Kind: progress.KindDownloading, Package: label, Done: done, Total: total})
		},
	}
	a.emit(progress.Event{Kind: progress.KindDownloading, Package: res.Name, Version: res.Version})
	sha, err := a.downloadAsset(ctx, res, down, assetPath)
	if err != nil {
		st.err = err
		return st
	}
	st.sha = sha

	a.emit(progress.Event{Kind: progress.KindVerifying, Package: res.Name, Version: res.Version})
	status, source, err := a.verify(ctx, res, sha, previous, opt)
	if err != nil {
		st.err = err
		return st
	}
	st.status, st.source = status, source

	a.emit(progress.Event{Kind: progress.KindExtracting, Package: res.Name, Version: res.Version})
	extractDir := filepath.Join(dir, "x")
	files, err := archive.Extract(assetPath, res.Format, extractDir)
	if err != nil {
		st.err = fmt.Errorf("%s: %w", res.Name, err)
		return st
	}
	bins, err := archive.Select(files, extractDir, archive.SelectOptions{
		Bins:        req.entry.Pkg.Source.Bins,
		Windows:     plat.IsWindows(),
		StripPrefix: stripPrefix(req.entry.Pkg.Source.StripPrefix),
	})
	if err != nil {
		st.err = fmt.Errorf("%s: %w", res.Name, err)
		return st
	}
	st.bins = bins
	return st
}

// installedCurrent reports whether every binary recorded for p still exists
// with the recorded hash.
func (a *App) installedCurrent(p *state.Package) bool {
	if len(p.Bins) == 0 {
		return false
	}
	for bin, want := range p.Bins {
		path := filepath.Join(a.BinDir(), bin)
		got, err := install.FileSHA256(path)
		if err != nil {
			return false
		}
		if want != "" && got != want {
			return false
		}
	}
	return true
}

func (a *App) verify(ctx context.Context, res *resolve.Resolution, sha string, previous map[string]*state.Package, opt InstallOptions) (string, string, error) {
	policy := a.Cfg.ChecksumPolicy
	if policy == "off" {
		return "none", "", nil
	}
	match, err := fetch.Discover(ctx, a.Client, res.Checksums, res.Asset, max(1, a.Cfg.HTTP.Retries))
	switch {
	case err == nil:
		if !strings.EqualFold(match.Hash, sha) {
			if !opt.ForceChecksum {
				return "", "", cerr.New(cerr.Integrity,
					"checksum mismatch for %s %s: expected %s, got %s (from %s)",
					res.Name, res.Version, match.Hash, sha, match.Source)
			}
			a.Out.Warnf("checksum mismatch for %s overridden by --force-checksum", res.Name)
			return "forced", match.Source, nil
		}
		return "verified", match.Source, nil
	case errors.Is(err, fetch.ErrNoChecksum):
		if policy == "require" {
			return "", "", cerr.New(cerr.Integrity, "%s: no checksum published (%v)", res.Name, err)
		}
	default:
		return "", "", fmt.Errorf("%s: checksum lookup: %w", res.Name, err)
	}

	prev := previous[res.Name]
	if prev != nil && prev.Version == res.Version && prev.DownloadSHA256 != "" && prev.DownloadSHA256 != sha && !opt.ForceChecksum {
		return "", "", cerr.New(cerr.Integrity,
			"%s %s changed since it was first installed (pinned %s, got %s); use --force-checksum to accept",
			res.Name, res.Version, prev.DownloadSHA256, sha)
	}
	return "tofu", "", nil
}

// VerifyResult is the integrity state of one installed binary.
type VerifyResult struct {
	Package string `json:"package"`
	Bin     string `json:"bin"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// Verify re-hashes installed binaries.
func (a *App) Verify(names []string) ([]VerifyResult, error) {
	if len(names) == 0 {
		names = a.InstalledNames()
	}
	var results []VerifyResult
	for _, name := range names {
		p, ok := a.State.Packages[name]
		if !ok {
			return nil, fmt.Errorf("%s is not installed", name)
		}
		for bin, want := range p.Bins {
			path := filepath.Join(a.BinDir(), bin)
			got, err := install.FileSHA256(path)
			switch {
			case err != nil:
				results = append(results, VerifyResult{Package: name, Bin: bin, Status: "missing", Detail: err.Error()})
			case want != "" && got != want:
				results = append(results, VerifyResult{Package: name, Bin: bin, Status: "modified", Detail: got})
			default:
				results = append(results, VerifyResult{Package: name, Bin: bin, Status: "ok"})
			}
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Package != results[j].Package {
			return results[i].Package < results[j].Package
		}
		return results[i].Bin < results[j].Bin
	})
	return results, nil
}

// Check is one doctor finding.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Doctor inspects the installation for common problems.
func (a *App) Doctor() ([]Check, error) {
	var checks []Check
	add := func(name, status, detail string) {
		checks = append(checks, Check{Name: name, Status: status, Detail: detail})
	}

	if err := os.MkdirAll(a.Paths.Root, 0o755); err != nil {
		add("root", "fail", err.Error())
	} else {
		add("root", "ok", a.Paths.Root)
	}
	if info, err := os.Stat(a.BinDir()); err != nil || !info.IsDir() {
		add("bin-dir", "fail", "missing "+a.BinDir())
	} else {
		add("bin-dir", "ok", a.BinDir())
	}
	if pathListed(a.BinDir()) {
		add("path", "ok", "bin directory is on PATH")
	} else {
		add("path", "warn", fmt.Sprintf("%s is not on PATH", a.BinDir()))
	}

	for name, p := range a.State.Packages {
		for bin := range p.Bins {
			if _, err := os.Stat(filepath.Join(a.BinDir(), bin)); err != nil {
				add("missing-binary", "fail", name+" provides "+bin+" but it is gone")
			}
		}
	}
	for bin, owner := range a.State.Provides {
		if _, ok := a.State.Packages[owner]; !ok {
			add("orphan-owner", "warn", bin+" is attributed to missing package "+owner)
		}
	}

	entries, err := os.ReadDir(a.BinDir())
	if err != nil {
		add("bin-scan", "warn", err.Error())
	} else {
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), ".new") {
				continue
			}
			if _, owned := a.State.Owner(e.Name()); !owned {
				add("untracked", "warn", e.Name()+" is not managed by cpt")
			}
		}
	}

	for _, backup := range install.RecoveryBackups(a.BinDir()) {
		add("recovery-backup", "warn", backup)
	}

	if cat, err := a.Catalog(); err != nil {
		add("lists", "fail", err.Error())
	} else if conflicts := cat.Conflicts(); len(conflicts) > 0 {
		names := make([]string, 0, len(conflicts))
		for name := range conflicts {
			names = append(names, name)
		}
		sort.Strings(names)
		add("conflicts", "fail", strings.Join(names, ", "))
	} else {
		add("lists", "ok", fmt.Sprintf("%d packages", len(cat.Entries())))
	}

	initProblems := false
	customInit := false
	for _, fl := range []shellinit.Flavor{shellinit.FlavorBash, shellinit.FlavorPwsh} {
		path := a.InitScriptPath(fl)
		data, err := os.ReadFile(path)
		switch {
		case err != nil:
			initProblems = true
			add("shell-init", "fail", "missing "+path)
		case !looksGenerated(data):
			customInit = true
			add("shell-init", "warn", path+" was not generated by cpt; leaving it untouched")
		case string(data) != a.InitScript(fl):
			initProblems = true
			add("shell-init", "warn", path+" is out of date and will be regenerated")
		}
	}
	if !initProblems && !customInit {
		add("shell-init", "ok", "generated scripts are current")
	}
	if !a.Cfg.AllowShellPackages {
		add("shell-packages", "warn", "disabled by allow_shell_packages=false")
	} else if n := a.PendingShellCount(); n > 0 {
		add("shell-approvals", "warn", fmt.Sprintf("%d snippet update(s) await approval; run cpt shell", n))
	} else {
		add("shell-approvals", "ok", "no pending snippet changes")
	}
	if a.Cfg.AllowShellPackages {
		for _, info := range a.ShellInfos() {
			if info.SyncPending {
				add("shell-sync", "warn", info.Package+": snippet sync pending; cpt will retry or run cpt lists refresh")
			}
			if info.LastError != "" {
				add("shell-snippet", "warn", info.Package+": "+info.LastError)
			}
			switch {
			case info.Removal:
				add("shell-removal", "warn", info.Package+": list no longer declares a snippet; run cpt shell diff "+info.Package)
			case info.Live && !info.Declared && !info.Rejected:
				add("shell-removal", "warn", info.Package+": snippet is not declared by the current lists; run cpt lists refresh")
			}
		}
	}

	if tmpEntries, err := os.ReadDir(a.Paths.Tmp); err == nil {
		for _, e := range tmpEntries {
			add("stale-tmp", "warn", filepath.Join(a.Paths.Tmp, e.Name()))
		}
	}
	return checks, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstResultError(results []Result) error {
	for _, r := range results {
		if r.Err != nil {
			return r.Err
		}
	}
	return nil
}

func dedupe(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// ResultsError returns the first per-package failure, if any.
func ResultsError(results []Result) error { return firstResultError(results) }

// splitSpec splits "name@version" at the last '@'.
func splitSpec(spec string) (name, version string) {
	spec = strings.TrimSpace(spec)
	if i := strings.LastIndex(spec, "@"); i >= 0 {
		return strings.TrimSpace(spec[:i]), strings.TrimSpace(spec[i+1:])
	}
	return spec, ""
}

func stripPrefix(v *bool) bool {
	if v == nil {
		return true
	}
	return *v
}

func safeAssetName(asset string) string {
	base := filepath.Base(asset)
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == '+':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." {
		return "asset"
	}
	return out
}

func pathListed(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == "" {
			continue
		}
		pa, err := filepath.Abs(p)
		if err == nil && pa == abs {
			return true
		}
	}
	return false
}
