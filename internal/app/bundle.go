package app

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	cpt "github.com/SuSonicTH/cpt"
	"github.com/SuSonicTH/cpt/internal/archive"
	"github.com/SuSonicTH/cpt/internal/bundle"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/fetch"
	"github.com/SuSonicTH/cpt/internal/fsutil"
	"github.com/SuSonicTH/cpt/internal/install"
	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/platform"
	"github.com/SuSonicTH/cpt/internal/progress"
	"github.com/SuSonicTH/cpt/internal/resolve"
	"github.com/SuSonicTH/cpt/internal/shellinit"
	"github.com/SuSonicTH/cpt/internal/state"
	"github.com/SuSonicTH/cpt/internal/version"
)

// BundlePin selects an exact release when rebuilding a bundle.
type BundlePin struct {
	Name    string
	Tag     string
	Version string
	SHA256  string
	Shell   *bundle.ManifestShell
}

// BundleOptions controls bundle creation.
type BundleOptions struct {
	Name          string
	Target        string
	Packages      []string
	Frozen        bool
	BasePath      string
	Output        string
	ForceChecksum bool
	Pins          []BundlePin
	// Catalog and SnapshotLists let rebuild use the lists saved with the
	// original bundle instead of whatever happens to be configured locally.
	Catalog          *list.Catalog
	SnapshotLists    map[string][]byte
	SnapshotManifest *bundle.Manifest
}

// BundleBuildResult describes a finished bundle.
type BundleBuildResult struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Target   string   `json:"target"`
	Frozen   bool     `json:"frozen"`
	Size     int64    `json:"size"`
	SHA256   string   `json:"sha256"`
	Packages []string `json:"packages"`
	Lists    []string `json:"lists"`
	Warnings []string `json:"warnings,omitempty"`
}

// BundleInfo describes a saved bundle for listings and the TUI.
type BundleInfo struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Manifest string    `json:"manifest"`
	Target   string    `json:"target"`
	Frozen   bool      `json:"frozen"`
	Packages int       `json:"packages"`
	Size     int64     `json:"size"`
	Created  time.Time `json:"created"`
}

type builtPackage struct {
	entry  list.Entry
	res    *resolve.Resolution
	dir    string
	bins   []archive.Bin
	sha    string
	status string
	shell  *bundle.ManifestShell
	err    error
}

// WithBuildLock snapshots state under the lock and then runs fn without holding
// it. Bundle builds spend most of their time downloading; keeping the state
// lock for that would block installs and the TUI.
func (a *App) WithBuildLock(fn func() error) error {
	lock, err := state.Acquire(a.Paths.LockFile, 5*time.Second)
	if err != nil {
		return err
	}
	if err := a.loadState(); err != nil {
		lock.Release()
		return err
	}
	a.State = cloneState(a.State)
	if err := os.MkdirAll(a.Paths.Tmp, 0o755); err == nil {
		_ = fsutil.CleanTmp(a.Paths.Tmp)
	}
	lock.Release()
	return fn()
}

func cloneState(st *state.State) *state.State {
	if st == nil {
		return state.New()
	}
	data, err := json.Marshal(st)
	if err != nil {
		return st
	}
	out := state.New()
	if err := json.Unmarshal(data, out); err != nil {
		return st
	}
	return out
}

// BuildBundle resolves packages for the target platform and writes a
// self-extracting bundle. It never installs anything into the local bin dir.
func (a *App) BuildBundle(ctx context.Context, opt BundleOptions) (BundleBuildResult, error) {
	if !bundle.ValidName(opt.Name) {
		return BundleBuildResult{}, cerr.New(cerr.Usage, "invalid bundle name %q", opt.Name)
	}
	target := platform.Current()
	if strings.TrimSpace(opt.Target) != "" {
		var err error
		target, err = platform.Parse(opt.Target)
		if err != nil {
			return BundleBuildResult{}, cerr.New(cerr.Usage, "%v", err)
		}
	}
	if a.Offline {
		return BundleBuildResult{}, cerr.New(cerr.Usage, "cannot build a bundle in offline mode")
	}
	cat := opt.Catalog
	if cat == nil {
		var err error
		cat, err = a.Catalog()
		if err != nil {
			return BundleBuildResult{}, err
		}
	}

	specs, pins, skipped, err := a.bundleSelection(opt, target, cat)
	if err != nil {
		return BundleBuildResult{}, err
	}
	selectionWarnings := make([]string, 0, len(skipped))
	for _, name := range skipped {
		selectionWarnings = append(selectionWarnings, fmt.Sprintf("skipped %s: no entry for %s in the configured lists", name, target))
	}

	// Keep build staging outside <root>/tmp: releasing the state lock lets
	// unrelated installs clean that directory while a long build is running.
	rootTmp, err := os.MkdirTemp("", "cpt-build-*")
	if err != nil {
		return BundleBuildResult{}, err
	}
	defer os.RemoveAll(rootTmp)

	built := make([]*builtPackage, len(specs))
	parallel := a.Cfg.Parallel
	if parallel < 1 {
		parallel = 1
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pin := pins[splitSpecName(spec)]
			built[i] = a.buildOne(ctx, cat, target, spec, pin, opt.ForceChecksum, filepath.Join(rootTmp, fmt.Sprintf("p%d", i)))
		}(i, spec)
	}
	wg.Wait()

	payload := bundle.Payload{
		Lists: map[string][]byte{},
		Init:  map[string][]byte{},
	}
	manifest := bundle.Manifest{
		Schema:     bundle.ManifestSchema,
		Name:       opt.Name,
		Created:    time.Now().UTC(),
		Target:     target.String(),
		CptVersion: version.Version,
		Frozen:     opt.Frozen,
		Lists:      map[string]bundle.ManifestList{},
	}
	owners := map[string]string{}
	for i, p := range built {
		name := splitSpecName(specs[i])
		if p == nil {
			return BundleBuildResult{}, fmt.Errorf("%s: no build result", name)
		}
		if p.err != nil {
			return BundleBuildResult{}, p.err
		}
		if p.res == nil {
			return BundleBuildResult{}, fmt.Errorf("%s: no resolution result", name)
		}
		mp := bundle.ManifestPackage{
			Name:           p.res.Name,
			List:           p.res.List,
			Tag:            p.res.Tag,
			Version:        p.res.Version,
			Asset:          p.res.Asset,
			DownloadSHA256: p.sha,
			ChecksumStatus: p.status,
			Shell:          p.shell,
		}
		for _, bin := range p.bins {
			if strings.EqualFold(bin.Name, "cpt") || strings.EqualFold(bin.Name, "cpt.exe") {
				return BundleBuildResult{}, cerr.New(cerr.Conflict,
					"package %s provides %s, which is reserved for cpt itself", p.res.Name, bin.Name)
			}
			if prev, ok := owners[strings.ToLower(bin.Name)]; ok {
				return BundleBuildResult{}, cerr.New(cerr.Conflict,
					"packages %s and %s both provide %s", prev, p.res.Name, bin.Name)
			}
			owners[strings.ToLower(bin.Name)] = p.res.Name
			mp.Bins = append(mp.Bins, bin.Name)
			payload.Bins = append(payload.Bins, bundle.PayloadBin{Name: bin.Name, Path: bin.Path})
		}
		sort.Strings(mp.Bins)
		manifest.Packages = append(manifest.Packages, mp)
	}
	sort.Slice(manifest.Packages, func(i, j int) bool {
		return manifest.Packages[i].Name < manifest.Packages[j].Name
	})

	var lists map[string][]byte
	if opt.SnapshotLists != nil {
		lists = opt.SnapshotLists
		if opt.SnapshotManifest != nil {
			for name, entry := range opt.SnapshotManifest.Lists {
				data, ok := lists[name]
				if !ok {
					return BundleBuildResult{}, cerr.New(cerr.Integrity,
						"bundle snapshot is missing list %s", name)
				}
				if entry.SHA256 != "" && hashData(data) != entry.SHA256 {
					return BundleBuildResult{}, cerr.New(cerr.Integrity,
						"bundle snapshot list %s does not match its manifest hash", name)
				}
				manifest.Lists[name] = entry
			}
		}
	} else {
		used := map[string]bool{}
		for _, p := range manifest.Packages {
			if p.List != "" {
				used[p.List] = true
			}
		}
		lists, err = a.bundleLists(used, manifest.Lists)
		if err != nil {
			return BundleBuildResult{}, err
		}
	}
	payload.Lists = lists
	payload.Init = a.bundleInit(built)

	base, err := a.bundleBase(ctx, target, opt.BasePath)
	if err != nil {
		return BundleBuildResult{}, err
	}
	if err := validateBaseBinary(base, target); err != nil {
		return BundleBuildResult{}, err
	}

	outPath := opt.Output
	if strings.TrimSpace(outPath) == "" {
		outPath = filepath.Join(a.Paths.Bundles, manifest.OutputName())
	} else if target.IsWindows() && !strings.EqualFold(filepath.Ext(outPath), ".exe") {
		outPath += ".exe"
	}
	manifest.Output = bundleOutputRef(a.Paths.Bundles, outPath)
	payload.Manifest = manifest

	manifestPath := filepath.Join(a.Paths.Bundles, bundle.FileName(manifest.Name))
	buildLock, err := state.Acquire(filepath.Join(a.Paths.Bundles, ".lock"), 5*time.Second)
	if err != nil {
		return BundleBuildResult{}, err
	}
	defer buildLock.Release()
	var oldOutput string
	if old, err := bundle.LoadManifest(manifestPath); err == nil {
		oldOutput = old.OutputPath(a.Paths.Bundles)
	}
	size, sum, err := writeBundle(outPath, base, manifest, payload)
	if err != nil {
		return BundleBuildResult{}, err
	}
	warnings := append([]string{}, selectionWarnings...)
	if target.OS == "darwin" {
		if runtime.GOOS == "darwin" {
			if err := codesignBundle(ctx, outPath); err != nil {
				warnings = append(warnings, "codesign failed: "+err.Error())
			}
		} else {
			warnings = append(warnings, "cross-built darwin bundle is unsigned; run codesign --force --sign - on macOS before distributing it")
		}
	}
	// codesign rewrites the file, so hash and size must be measured after it.
	info, err := os.Stat(outPath)
	if err != nil {
		return BundleBuildResult{}, err
	}
	size = info.Size()
	sum, err = install.FileSHA256(outPath)
	if err != nil {
		return BundleBuildResult{}, err
	}
	if err := saveBundleSnapshot(a.bundleSnapshotDir(manifest.Name), lists); err != nil {
		return BundleBuildResult{}, err
	}
	if err := manifest.Save(manifestPath); err != nil {
		return BundleBuildResult{}, err
	}
	if oldOutput != "" && filepath.Clean(oldOutput) != filepath.Clean(outPath) && bundle.IsBundle(oldOutput) {
		warnings = append(warnings, "replaced the previous "+filepath.Base(oldOutput)+" bundle for another target")
		if err := os.Remove(oldOutput); err != nil {
			warnings = append(warnings, "could not remove the previous bundle: "+err.Error())
		}
	}

	res := BundleBuildResult{
		Name:     manifest.Name,
		Path:     outPath,
		Target:   manifest.Target,
		Frozen:   manifest.Frozen,
		Size:     size,
		SHA256:   sum,
		Lists:    sortedManifestLists(manifest.Lists),
		Warnings: warnings,
	}
	for _, p := range manifest.Packages {
		res.Packages = append(res.Packages, p.Name+" "+p.Version)
	}
	return res, nil
}

// bundleOutputRef stores a path relative to the bundles directory when
// possible, so a bundle tree can be moved without breaking rebuild/remove.
func bundleOutputRef(bundleDir, outPath string) string {
	dirAbs, err1 := filepath.Abs(bundleDir)
	outAbs, err2 := filepath.Abs(outPath)
	if err1 == nil && err2 == nil && filepath.Dir(outAbs) == dirAbs {
		return filepath.Base(outAbs)
	}
	if err2 == nil {
		return outAbs
	}
	return outPath
}

func hashData(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sortedManifestLists(m map[string]bundle.ManifestList) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func splitSpecName(spec string) string {
	name, _ := splitSpec(spec)
	return name
}

func (a *App) bundleSelection(opt BundleOptions, target platform.Platform, cat *list.Catalog) ([]string, map[string]*BundlePin, []string, error) {
	pins := map[string]*BundlePin{}
	for i := range opt.Pins {
		p := &opt.Pins[i]
		pins[p.Name] = p
	}
	var specs []string
	var skipped []string
	switch {
	case len(opt.Pins) > 0:
		for _, p := range opt.Pins {
			specs = append(specs, p.Name)
		}
	case len(opt.Packages) > 0:
		for _, spec := range opt.Packages {
			name := splitSpecName(strings.TrimSpace(spec))
			if name == "" {
				return nil, nil, nil, cerr.New(cerr.Usage, "invalid package spec %q", spec)
			}
			specs = append(specs, strings.TrimSpace(spec))
		}
	default:
		for _, name := range a.InstalledNames() {
			entry, err := cat.Lookup(name)
			if err != nil {
				skipped = append(skipped, name+" (not in any configured list)")
				continue
			}
			if entry.Pkg.Source.Type != "url" && entry.Pkg.Source.Asset[target.String()] == "" {
				skipped = append(skipped, name+" (unsupported on "+target.String()+")")
				continue
			}
			specs = append(specs, name)
		}
	}
	if len(specs) == 0 {
		msg := fmt.Sprintf("a bundle needs at least one package for %s", target)
		if len(skipped) > 0 {
			msg += "; skipped " + strings.Join(skipped, ", ")
		}
		return nil, nil, skipped, cerr.New(cerr.Usage, "%s", msg)
	}
	return dedupe(specs), pins, skipped, nil
}

func (a *App) buildOne(ctx context.Context, cat *list.Catalog, target platform.Platform, spec string, pin *BundlePin, forceChecksum bool, dir string) *builtPackage {
	name, cliVersion := splitSpec(spec)
	out := &builtPackage{dir: dir}
	fail := func(err error) *builtPackage {
		out.err = fmt.Errorf("%s: %w", name, err)
		a.emit(progress.Event{Kind: progress.KindFailed, Package: name, Message: err.Error()})
		return out
	}
	entry, err := cat.Lookup(name)
	if err != nil {
		return fail(err)
	}
	out.entry = entry

	resolveOpt := resolve.Options{}
	if pin != nil {
		resolveOpt.UserPin = pin.Version
		resolveOpt.InstalledTag = pin.Tag
		resolveOpt.InstalledVer = pin.Version
		out.shell = pin.Shell
	} else if cliVersion != "" {
		if err := list.ValidateVersion(cliVersion); err != nil {
			return fail(cerr.Wrap(cerr.Usage, err))
		}
		resolveOpt.CLIVersion = cliVersion
	} else if installed := a.State.Packages[name]; installed != nil && installed.Pinned != "" {
		// Bundle creation follows the same precedence as install: an explicit
		// spec wins, then the user pin in state, then the list pin/latest.
		resolveOpt.UserPin = installed.Pinned
	}

	a.emit(progress.Event{Kind: progress.KindQueued, Package: name, Version: cliVersion})
	a.emit(progress.Event{Kind: progress.KindResolving, Package: name})
	res, err := resolve.Package(ctx, a.Prober, entry, target, resolveOpt)
	if err != nil {
		return fail(err)
	}
	out.res = res
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err)
	}
	assetPath := filepath.Join(dir, safeAssetName(res.Asset))
	var last time.Time
	down := &fetch.Downloader{
		Client:   a.Client,
		Attempts: max(1, a.Cfg.HTTP.Retries),
		Progress: func(_ string, done, total int64) {
			now := time.Now()
			if now.Sub(last) < 100*time.Millisecond && (total <= 0 || done < total) {
				return
			}
			last = now
			a.emit(progress.Event{Kind: progress.KindDownloading, Package: name, Version: res.Version, Done: done, Total: total})
		},
	}
	a.emit(progress.Event{Kind: progress.KindDownloading, Package: name, Version: res.Version})
	sha, err := a.downloadAsset(ctx, res, down, assetPath)
	if err != nil {
		return fail(err)
	}
	out.sha = sha
	if pin != nil && pin.SHA256 != "" && !strings.EqualFold(pin.SHA256, sha) {
		if !forceChecksum {
			return fail(cerr.New(cerr.Integrity,
				"download hash %s does not match the saved bundle manifest (%s); use --force-checksum to accept", sha, pin.SHA256))
		}
		a.Out.Warnf("%s: rebuild hash differs from the saved manifest; accepted by --force-checksum", name)
	}

	a.emit(progress.Event{Kind: progress.KindVerifying, Package: name, Version: res.Version})
	status, _, err := a.verify(ctx, res, sha, map[string]*state.Package{}, InstallOptions{ForceChecksum: forceChecksum})
	if err != nil {
		return fail(err)
	}
	out.status = status

	a.emit(progress.Event{Kind: progress.KindExtracting, Package: name, Version: res.Version})
	extractDir := filepath.Join(dir, "x")
	files, err := archive.Extract(assetPath, res.Format, extractDir)
	if err != nil {
		return fail(err)
	}
	bins, err := archive.Select(files, extractDir, archive.SelectOptions{
		Bins:        entry.Pkg.Source.Bins,
		Windows:     target.IsWindows(),
		StripPrefix: stripPrefix(entry.Pkg.Source.StripPrefix),
	})
	if err != nil {
		return fail(err)
	}
	out.bins = bins
	if pin == nil {
		out.shell = a.bundleShell(entry)
	}
	a.emit(progress.Event{Kind: progress.KindDone, Package: name, Version: res.Version, Action: "built"})
	return out
}

func (a *App) bundleShell(entry list.Entry) *bundle.ManifestShell {
	if !a.Cfg.AllowShellPackages || entry.Pkg.Shell == nil {
		return nil
	}
	sh := a.State.Shell[entry.Pkg.Name]
	if sh == nil || sh.Live == nil {
		return nil
	}
	if strings.TrimSpace(sh.Live.Bash) == "" && strings.TrimSpace(sh.Live.Pwsh) == "" {
		return nil
	}
	version := sh.Live.Version
	if version == "" {
		version = entry.Pkg.Version
	}
	return &bundle.ManifestShell{
		Version: version,
		Bash:    sh.Live.Bash,
		Pwsh:    sh.Live.Pwsh,
		SHA256:  shellinit.Bodies{Bash: sh.Live.Bash, Pwsh: sh.Live.Pwsh}.Hash(),
	}
}

func (a *App) bundleLists(used map[string]bool, into map[string]bundle.ManifestList) (map[string][]byte, error) {
	names, err := a.LoadedLists()
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(names))
	for _, name := range names {
		if name == "" || strings.HasSuffix(name, ".lock") {
			continue
		}
		if len(used) > 0 && !used[name] {
			continue
		}
		l, data, err := list.ReadFile(filepath.Join(a.Paths.Lists, list.FileName(name)))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		origin := ""
		if meta := a.State.Lists[name]; meta != nil {
			origin = meta.Origin
		}
		if origin == "" && name == "core" {
			origin = cpt.BuiltinListOrigin
		}
		into[name] = bundle.ManifestList{
			SHA256:   hex.EncodeToString(sum[:]),
			Origin:   origin,
			Packages: len(l.Packages),
		}
		out[name] = data
	}
	return out, nil
}

func (a *App) bundleInit(built []*builtPackage) map[string][]byte {
	home, _ := shellinit.HomeDir()
	root := filepath.Join(home, ".cpt")
	if home == "" {
		root = ".cpt"
	}
	opt := shellinit.Options{Root: root, Bin: filepath.Join(root, "bin"), Home: home}
	out := map[string][]byte{}
	for _, fl := range []shellinit.Flavor{shellinit.FlavorBash, shellinit.FlavorPwsh} {
		var blocks []shellinit.Block
		for _, p := range built {
			if p == nil || p.shell == nil {
				continue
			}
			body := p.shell.Bash
			if fl == shellinit.FlavorPwsh {
				body = p.shell.Pwsh
			}
			if strings.TrimSpace(body) == "" {
				continue
			}
			blocks = append(blocks, shellinit.Block{
				Name:    p.res.Name,
				Version: p.shell.Version,
				SHA256:  p.shell.SHA256,
				Body:    body,
			})
		}
		out[shellinit.ScriptName(fl)] = []byte(shellinit.Render(fl, opt, blocks))
	}
	return out
}

func (a *App) bundleBase(ctx context.Context, target platform.Platform, explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	if target == platform.Current() {
		return bundle.SelfPath()
	}
	ver := strings.TrimPrefix(version.Version, "v")
	if ver == "" || ver == "dev" {
		return "", cerr.New(cerr.Usage,
			"cannot build a %s bundle from a development build; pass --base PATH or use a release build", target)
	}
	cache := filepath.Join(a.Paths.SelfCache, fmt.Sprintf("cpt_%s_%s_%s", ver, target.OS, target.Arch))
	if info, err := os.Stat(cache); err == nil && info.Mode().IsRegular() {
		return cache, nil
	}
	if err := os.MkdirAll(a.Paths.SelfCache, 0o755); err != nil {
		return "", err
	}
	asset := fmt.Sprintf("cpt_%s_%s_%s.tar.gz", ver, target.OS, target.Arch)
	url := fmt.Sprintf("https://github.com/SuSonicTH/cpt/releases/download/v%s/%s", ver, asset)
	tmp, err := os.MkdirTemp("", "cpt-self-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	archivePath := filepath.Join(tmp, asset)
	var last time.Time
	down := &fetch.Downloader{
		Client:   a.Client,
		Attempts: max(1, a.Cfg.HTTP.Retries),
		Progress: func(_ string, done, total int64) {
			now := time.Now()
			if now.Sub(last) < 100*time.Millisecond && (total <= 0 || done < total) {
				return
			}
			last = now
			a.emit(progress.Event{Kind: progress.KindDownloading, Package: "cpt", Version: ver, Done: done, Total: total})
		},
	}
	a.emit(progress.Event{Kind: progress.KindDownloading, Package: "cpt", Version: ver})
	sha, _, err := down.To(ctx, url, archivePath, "cpt")
	if err != nil {
		return "", fmt.Errorf("fetch the %s cpt binary: %w", target, err)
	}
	baseDir := strings.TrimSuffix(url, "/"+asset)
	candidates := []fetch.Candidate{
		{Name: fmt.Sprintf("cpt_%s_checksums.txt", ver), URL: baseDir + "/" + fmt.Sprintf("cpt_%s_checksums.txt", ver)},
		{Name: "checksums.txt", URL: baseDir + "/checksums.txt"},
		{Name: "SHA256SUMS", URL: baseDir + "/SHA256SUMS"},
	}
	match, sumErr := fetch.Discover(ctx, a.Client, candidates, asset, max(1, a.Cfg.HTTP.Retries))
	switch {
	case sumErr == nil:
		if !strings.EqualFold(match.Hash, sha) {
			return "", cerr.New(cerr.Integrity,
				"cpt base binary checksum mismatch: expected %s, got %s (from %s)", match.Hash, sha, match.Source)
		}
	case errors.Is(sumErr, fetch.ErrNoChecksum):
		if a.Cfg.ChecksumPolicy == "require" {
			return "", cerr.New(cerr.Integrity, "no checksum published for the %s cpt base binary", target)
		}
	default:
		return "", fmt.Errorf("look up the cpt base checksum: %w", sumErr)
	}
	extractDir := filepath.Join(tmp, "x")
	files, err := archive.Extract(archivePath, archive.DetectFormat(asset), extractDir)
	if err != nil {
		return "", err
	}
	bins, err := archive.Select(files, extractDir, archive.SelectOptions{Bins: []string{"cpt"}, Windows: target.IsWindows()})
	if err != nil {
		return "", fmt.Errorf("%s release archive: %w", target, err)
	}
	tmpBase := filepath.Join(tmp, "base")
	if err := copyFileMode(bins[0].Path, tmpBase, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmpBase, cache); err != nil {
		// A concurrent build may have populated the cache first.
		if _, statErr := os.Stat(cache); statErr != nil {
			return "", err
		}
	}
	return cache, nil
}

// codesignBundle ad-hoc signs a darwin bundle after the payload is appended.
// Appending invalidates an existing Mach-O signature on Apple Silicon.
func codesignBundle(ctx context.Context, path string) error {
	exe, err := exec.LookPath("codesign")
	if err != nil {
		return nil // no toolchain available; the caller records no warning
	}
	cmd := exec.CommandContext(ctx, exe, "--force", "--sign", "-", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func copyFileMode(src, dest string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func writeBundle(outPath, base string, manifest bundle.Manifest, payload bundle.Payload) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return 0, "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".bundle-*")
	if err != nil {
		return 0, "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	in, err := os.Open(base)
	if err != nil {
		tmp.Close()
		return 0, "", err
	}
	baseSize, err := copyBase(tmp, in)
	in.Close()
	if err != nil {
		tmp.Close()
		return 0, "", err
	}

	sha := sha256.New()
	crc := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	count := &countingWriter{w: io.MultiWriter(tmp, sha, crc)}
	if err := bundle.WritePayload(count, payload); err != nil {
		tmp.Close()
		return 0, "", err
	}
	footer := bundle.Footer{
		PayloadOffset: uint64(baseSize),
		PayloadLen:    uint64(count.n),
		CRC32C:        crc.Sum32(),
		Version:       bundle.FormatVersion,
	}
	copy(footer.SHA256[:], sha.Sum(nil))
	if manifest.Frozen {
		footer.Flags |= bundle.FlagFrozen
	}
	if _, err := tmp.Write(footer.Marshal()); err != nil {
		tmp.Close()
		return 0, "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, "", err
	}
	if err := tmp.Close(); err != nil {
		return 0, "", err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return 0, "", err
	}
	if err := os.Rename(tmpName, outPath); err != nil {
		if runtime.GOOS != "windows" {
			return 0, "", err
		}
		// Some Windows filesystems refuse to replace an existing file. Move the
		// old bundle aside first and restore it if the replacement fails.
		backup := outPath + ".old"
		_ = os.Remove(backup)
		if moveErr := os.Rename(outPath, backup); moveErr == nil {
			if renameErr := os.Rename(tmpName, outPath); renameErr != nil {
				_ = os.Rename(backup, outPath)
				return 0, "", renameErr
			}
			_ = os.Remove(backup)
		} else {
			if rmErr := os.Remove(outPath); rmErr != nil && !os.IsNotExist(rmErr) {
				return 0, "", err
			}
			if renameErr := os.Rename(tmpName, outPath); renameErr != nil {
				return 0, "", renameErr
			}
		}
	}
	info, err := os.Stat(outPath)
	if err != nil {
		return 0, "", err
	}
	sum, err := install.FileSHA256(outPath)
	if err != nil {
		return 0, "", err
	}
	return info.Size(), sum, nil
}

// validateBaseBinary catches an accidentally wrong-arch --base. It silently
// accepts unrecognised formats (scripts, test fixtures) instead of guessing.
func validateBaseBinary(path string, target platform.Platform) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, _ := io.ReadFull(f, head)
	head = head[:n]

	gotOS, gotArch := "", ""
	switch {
	case len(head) >= 20 && string(head[:4]) == "\x7fELF":
		gotOS = "linux"
		var order binary.ByteOrder = binary.LittleEndian
		if head[5] == 2 {
			order = binary.BigEndian
		}
		switch order.Uint16(head[18:20]) {
		case 62:
			gotArch = "amd64"
		case 183:
			gotArch = "arm64"
		}
	case len(head) >= 0x40 && string(head[:2]) == "MZ":
		gotOS = "windows"
		off := int(binary.LittleEndian.Uint32(head[0x3c:0x40]))
		if off > 0 && off+6 <= len(head) && string(head[off:off+4]) == "PE\x00\x00" {
			switch binary.LittleEndian.Uint16(head[off+4 : off+6]) {
			case 0x8664:
				gotArch = "amd64"
			case 0xAA64:
				gotArch = "arm64"
			}
		}
	case len(head) >= 8:
		magic := binary.LittleEndian.Uint32(head[:4])
		if magic == 0xFEEDFACF || magic == 0xFEEDFACE {
			gotOS = "darwin"
			cpu := binary.LittleEndian.Uint32(head[4:8])
			switch cpu {
			case 0x01000007:
				gotArch = "amd64"
			case 0x0100000C:
				gotArch = "arm64"
			}
		}
	}
	if gotOS == "" || gotArch == "" {
		return nil
	}
	if gotOS != target.OS || gotArch != target.Arch {
		return cerr.New(cerr.Usage,
			"base executable is %s/%s but the bundle targets %s", gotOS, gotArch, target)
	}
	return nil
}

// copyBase copies a base executable, dropping any existing bundle payload.
func copyBase(dst io.Writer, src *os.File) (int64, error) {
	info, err := src.Stat()
	if err != nil {
		return 0, err
	}
	limit := info.Size()
	if footer, err := bundle.ReadFooter(src); err == nil {
		limit = int64(footer.PayloadOffset)
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.Copy(dst, io.LimitReader(src, limit))
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, cerr.New(cerr.Usage, "the base cpt executable is empty")
	}
	if n != limit {
		return 0, fmt.Errorf("base executable is truncated (%d of %d bytes)", n, limit)
	}
	return n, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (a *App) bundleSnapshotDir(name string) string {
	return filepath.Join(a.Paths.Bundles, name+".d")
}

func saveBundleSnapshot(dir string, lists map[string][]byte) error {
	if err := os.RemoveAll(filepath.Join(dir, "lists")); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "lists"), 0o755); err != nil {
		return err
	}
	for name, data := range lists {
		if !list.ValidName(name) {
			return fmt.Errorf("invalid list name %q", name)
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, "lists", list.FileName(name)), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func loadBundleSnapshot(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "lists"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "lists", e.Name()))
		if err != nil {
			return nil, err
		}
		out[strings.TrimSuffix(e.Name(), ".json")] = data
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func catalogFromListData(lists map[string][]byte) (*list.Catalog, error) {
	named := make([]list.Named, 0, len(lists))
	for name, data := range lists {
		l, err := list.Parse(data, name)
		if err != nil {
			return nil, err
		}
		named = append(named, list.Named{Name: name, List: l})
	}
	return list.BuildCatalog(named)
}

// RebuildBundle reproduces a saved bundle from its manifest's exact versions.
func (a *App) RebuildBundle(ctx context.Context, name string) (BundleBuildResult, error) {
	return a.rebuildBundle(ctx, name, false)
}

// RebuildBundleForced is RebuildBundle with an explicit checksum override.
func (a *App) RebuildBundleForced(ctx context.Context, name string, forceChecksum bool) (BundleBuildResult, error) {
	return a.rebuildBundle(ctx, name, forceChecksum)
}

func (a *App) rebuildBundle(ctx context.Context, name string, forceChecksum bool) (BundleBuildResult, error) {
	if !bundle.ValidName(name) {
		return BundleBuildResult{}, cerr.New(cerr.Usage, "invalid bundle name %q", name)
	}
	m, err := bundle.LoadManifest(filepath.Join(a.Paths.Bundles, bundle.FileName(name)))
	if err != nil {
		if os.IsNotExist(err) {
			return BundleBuildResult{}, fmt.Errorf("unknown bundle %q", name)
		}
		return BundleBuildResult{}, err
	}
	snapshotLists, err := loadBundleSnapshot(a.bundleSnapshotDir(m.Name))
	if err != nil {
		return BundleBuildResult{}, err
	}
	opt := BundleOptions{
		Name:             m.Name,
		Target:           m.Target,
		Frozen:           m.Frozen,
		Output:           m.OutputPath(a.Paths.Bundles),
		ForceChecksum:    forceChecksum,
		SnapshotLists:    snapshotLists,
		SnapshotManifest: &m,
	}
	if snapshotLists != nil {
		cat, err := catalogFromListData(snapshotLists)
		if err != nil {
			return BundleBuildResult{}, fmt.Errorf("rebuild from saved lists: %w", err)
		}
		opt.Catalog = cat
	}
	for _, p := range m.Packages {
		opt.Pins = append(opt.Pins, BundlePin{
			Name: p.Name, Tag: p.Tag, Version: p.Version, SHA256: p.DownloadSHA256, Shell: p.Shell,
		})
	}
	res, err := a.BuildBundle(ctx, opt)
	if err == nil && snapshotLists == nil {
		res.Warnings = append(res.Warnings,
			"saved list snapshot is missing; rebuilt from the current configured lists")
	}
	return res, err
}

// Bundles lists saved bundle manifests, newest name order.
func (a *App) Bundles() ([]BundleInfo, error) {
	entries, err := os.ReadDir(a.Paths.Bundles)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []BundleInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		manifestPath := filepath.Join(a.Paths.Bundles, e.Name())
		m, err := bundle.LoadManifest(manifestPath)
		if err != nil {
			a.Log.Warn("skipping invalid bundle manifest", "path", manifestPath, "error", err)
			continue
		}
		path := m.OutputPath(a.Paths.Bundles)
		size := int64(0)
		if info, err := os.Stat(path); err == nil {
			size = info.Size()
		}
		out = append(out, BundleInfo{
			Name:     m.Name,
			Path:     path,
			Manifest: manifestPath,
			Target:   m.Target,
			Frozen:   m.Frozen,
			Packages: len(m.Packages),
			Size:     size,
			Created:  m.Created,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Target < out[j].Target
	})
	return out, nil
}

// RemoveBundle deletes a bundle executable and its manifest. The bundle name
// identifies one saved build; target-specific files use the manifest target.
func (a *App) RemoveBundle(name string) (string, error) {
	return a.RemoveBundleForced(name, false)
}

// RemoveBundleForced removes a manifest-backed file even when its footer is
// damaged. It still refuses to delete an unrelated file unless force is set.
func (a *App) RemoveBundleForced(name string, force bool) (string, error) {
	if !bundle.ValidName(name) {
		return "", cerr.New(cerr.Usage, "invalid bundle name %q", name)
	}
	buildLock, err := state.Acquire(filepath.Join(a.Paths.Bundles, ".lock"), 5*time.Second)
	if err != nil {
		return "", err
	}
	defer buildLock.Release()
	manifestPath := filepath.Join(a.Paths.Bundles, bundle.FileName(name))
	m, err := bundle.LoadManifest(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("unknown bundle %q", name)
		}
		return "", err
	}
	path := m.OutputPath(a.Paths.Bundles)
	if info, err := os.Stat(path); err == nil && !info.IsDir() && !bundle.IsBundle(path) && !force {
		return "", fmt.Errorf("refusing to remove %s: it is not a cpt bundle (use --force only if you are sure)", path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.RemoveAll(a.bundleSnapshotDir(name)); err != nil {
		return "", err
	}
	return path, nil
}

// Unfreeze lists clears the read-only flag on the named lists, or on every
// frozen list when names is empty. It returns the names it changed.
func (a *App) Unfreeze(names []string) []string {
	var changed []string
	if len(names) == 0 {
		for name, meta := range a.State.Lists {
			if meta != nil && meta.Readonly {
				names = append(names, name)
			}
		}
		sort.Strings(names)
	}
	for _, name := range names {
		meta := a.State.Lists[name]
		if meta == nil || !meta.Readonly {
			continue
		}
		meta.Readonly = false
		changed = append(changed, name)
	}
	return changed
}
