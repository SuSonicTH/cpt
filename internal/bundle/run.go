package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/SuSonicTH/cpt/internal/archive"
	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/config"
	"github.com/SuSonicTH/cpt/internal/fsutil"
	"github.com/SuSonicTH/cpt/internal/install"
	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/platform"
	"github.com/SuSonicTH/cpt/internal/shellinit"
	"github.com/SuSonicTH/cpt/internal/state"
)

// RunOptions controls a self-extracting bundle install.
type RunOptions struct {
	Dest        string
	List        bool
	DryRun      bool
	Force       bool
	NoInit      bool
	InstallInit bool
}

// PackageResult reports the outcome for one bundled package.
type PackageResult struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Action  string `json:"action"`
}

// RunResult describes a bundle install (or inspection).
type RunResult struct {
	Name     string          `json:"name"`
	Target   string          `json:"target"`
	Dest     string          `json:"dest,omitempty"`
	Frozen   bool            `json:"frozen"`
	DryRun   bool            `json:"dry_run,omitempty"`
	Packages []PackageResult `json:"packages"`
	Lists    []string        `json:"lists,omitempty"`
	Cpt      string          `json:"cpt,omitempty"`
	Init     string          `json:"init,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
}

// Run unpacks and installs the bundle at self. It is idempotent: packages that
// are already current are left untouched.
func Run(ctx context.Context, self string, opt RunOptions) (RunResult, error) {
	c, err := Open(self)
	if err != nil {
		return RunResult{}, err
	}
	defer c.Close()

	if opt.InstallInit && opt.NoInit {
		return RunResult{}, cerr.New(cerr.Usage, "--no-init and --install-init are mutually exclusive")
	}
	if opt.List || opt.DryRun {
		// Validate the whole payload without writing files: a container hash
		// alone does not catch a manifest whose list or binary hashes disagree.
		vr, check := c.VerifyingPayload()
		m, inspectErr := InspectPayload(vr)
		if inspectErr == nil {
			inspectErr = check()
		}
		if inspectErr != nil {
			return RunResult{}, inspectErr
		}
		res, err := inspect(m, opt)
		if err != nil {
			return res, err
		}
		if opt.List {
			return res, nil
		}
		return res, nil
	}

	dest := opt.Dest
	if strings.TrimSpace(dest) == "" {
		dest, err = config.DefaultRoot()
		if err != nil {
			return RunResult{}, err
		}
	}
	if err := validateDest(dest); err != nil {
		return RunResult{}, err
	}

	// Extract and verify into a staging directory on the destination's
	// filesystem before touching the destination, so corruption cannot create
	// or modify the target root and large bundles do not fill a small /tmp.
	stage, err := stageDir(dest)
	if err != nil {
		return RunResult{}, err
	}
	defer os.RemoveAll(stage)
	vr, check := c.VerifyingPayload()
	manifest, err := ExtractPayload(vr, stage)
	if err == nil {
		err = check()
	}
	if err != nil {
		return RunResult{}, err
	}
	if err := checkTarget(manifest); err != nil {
		return RunResult{}, err
	}

	paths := config.NewPaths(dest)
	if err := paths.Ensure(); err != nil {
		return RunResult{}, err
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return RunResult{}, err
	}
	binDir := cfg.BinDirFor(paths)

	lock, err := state.Acquire(paths.LockFile, 5*time.Second)
	if err != nil {
		return RunResult{}, err
	}
	defer lock.Release()

	st, err := state.Load(paths.StateFile)
	if err != nil {
		return RunResult{}, err
	}
	if err := fsutil.CleanTmp(paths.Tmp); err != nil {
		return RunResult{}, err
	}
	res := RunResult{
		Name:   manifest.Name,
		Target: manifest.Target,
		Dest:   paths.Root,
		Frozen: manifest.Frozen,
	}
	res.Lists = sortedKeys(manifest.Lists)

	manager := &install.Manager{BinDir: binDir, State: st, Force: opt.Force}
	var runErr error
	for _, p := range manifest.Packages {
		if stateCurrent(st, binDir, p) {
			if prev := st.Packages[p.Name]; prev != nil {
				prev.List = p.List
				prev.Origin = "bundle"
				prev.SourceID = "bundle:" + manifest.Name
			}
			res.Packages = append(res.Packages, PackageResult{Name: p.Name, Version: p.Version, Action: "unchanged"})
			continue
		}
		bins := make([]archive.Bin, 0, len(p.Bins))
		for _, bin := range p.Bins {
			bins = append(bins, archive.Bin{Name: bin, Path: filepath.Join(stage, "bin", bin)})
		}
		action := "installed"
		if prev := st.Packages[p.Name]; prev != nil {
			action = "updated"
		}
		placed, err := manager.Place(p.Name, bins)
		if err != nil {
			runErr = fmt.Errorf("%s: %w", p.Name, err)
			break
		}
		entry := &state.Package{
			Tag:            p.Tag,
			Version:        p.Version,
			List:           p.List,
			SourceID:       "bundle:" + manifest.Name,
			Asset:          p.Asset,
			DownloadSHA256: p.DownloadSHA256,
			ChecksumStatus: p.ChecksumStatus,
			Bins:           placed,
			Origin:         "bundle",
			InstalledAt:    time.Now().UTC(),
		}
		if prev := st.Packages[p.Name]; prev != nil && prev.Pinned != "" && prev.Pinned == p.Version {
			entry.Pinned = prev.Pinned
		}
		st.Packages[p.Name] = entry
		res.Packages = append(res.Packages, PackageResult{Name: p.Name, Version: p.Version, Action: action})
	}

	res.Warnings = append(res.Warnings, manager.Warnings...)
	if runErr == nil {
		runErr = installLists(paths, st, stage, manifest)
	}
	if runErr == nil {
		runErr = applyBundledShell(cfg, st, manifest, &res)
	}
	if runErr == nil {
		cptPath, warning, err := installSelf(c, binDir)
		if err != nil {
			runErr = err
		} else {
			res.Cpt = cptPath
			if warning != "" {
				res.Warnings = append(res.Warnings, warning)
			}
		}
	}
	saveErr := st.Save(paths.StateFile)
	if runErr != nil {
		return res, runErr
	}
	if saveErr != nil {
		return res, saveErr
	}

	if !opt.NoInit {
		initPath, err := writeInitFiles(paths, cfg, st, &res)
		if err != nil {
			return res, err
		}
		res.Init = initPath
		if opt.InstallInit {
			if err := installInitBlock(ctx, paths, binDir); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

func inspect(m Manifest, opt RunOptions) (RunResult, error) {
	res := RunResult{
		Name:   m.Name,
		Target: m.Target,
		Frozen: m.Frozen,
		DryRun: opt.DryRun,
		Lists:  sortedKeys(m.Lists),
	}
	if !opt.DryRun {
		for _, p := range m.Packages {
			res.Packages = append(res.Packages, PackageResult{Name: p.Name, Version: p.Version, Action: "listed"})
		}
		return res, nil
	}
	if err := checkTarget(m); err != nil {
		return res, err
	}
	dest := opt.Dest
	if strings.TrimSpace(dest) == "" {
		var err error
		dest, err = config.DefaultRoot()
		if err != nil {
			return res, err
		}
	}
	if err := validateDest(dest); err != nil {
		return res, err
	}
	paths := config.NewPaths(dest)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return res, err
	}
	binDir := cfg.BinDirFor(paths)
	if info, err := os.Lstat(binDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return res, cerr.New(cerr.Conflict, "bin directory %s is not a real directory", binDir)
		}
	} else if !os.IsNotExist(err) {
		return res, err
	}
	st, err := state.Load(paths.StateFile)
	if err != nil {
		return res, err
	}
	for _, p := range m.Packages {
		for _, bin := range p.Bins {
			destPath := filepath.Join(binDir, bin)
			if owner, ok := st.Owner(bin); ok && owner != p.Name && !opt.Force {
				return res, cerr.New(cerr.Conflict,
					"binary %q is already provided by %s (use --force to replace)", bin, owner)
			}
			if info, err := os.Lstat(destPath); err == nil {
				if !info.Mode().IsRegular() {
					return res, cerr.New(cerr.Conflict, "%s exists and is not a regular file", destPath)
				}
				if _, owned := st.Owner(bin); !owned && !opt.Force {
					return res, cerr.New(cerr.Conflict,
						"%s already exists and is not managed by cpt (use --force to replace)", destPath)
				}
			}
		}
	}
	res.Dest = dest
	for _, p := range m.Packages {
		action := "would install"
		switch {
		case stateCurrent(st, binDir, p):
			action = "unchanged"
		case st.Packages[p.Name] != nil:
			action = "would update"
		}
		res.Packages = append(res.Packages, PackageResult{Name: p.Name, Version: p.Version, Action: action})
	}
	return res, nil
}

func checkTarget(m Manifest) error {
	if strings.TrimSpace(m.Target) == "" {
		return nil
	}
	target, err := platform.Parse(m.Target)
	if err != nil {
		return cerr.New(cerr.Integrity, "bundle has an invalid target %q: %v", m.Target, err)
	}
	if host := platform.Current(); target != host {
		return cerr.New(cerr.Conflict,
			"bundle targets %s but this machine is %s; run it on a %s host", target, host, target)
	}
	return nil
}

func stateCurrent(st *state.State, binDir string, p ManifestPackage) bool {
	prev := st.Packages[p.Name]
	if prev == nil || prev.Version != p.Version {
		return false
	}
	if p.DownloadSHA256 != "" && prev.DownloadSHA256 != "" && prev.DownloadSHA256 != p.DownloadSHA256 {
		return false
	}
	if len(prev.Bins) != len(p.Bins) {
		return false
	}
	for bin, want := range prev.Bins {
		if _, ok := findString(p.Bins, bin); !ok {
			return false
		}
		got, err := install.FileSHA256(filepath.Join(binDir, bin))
		if err != nil {
			return false
		}
		if want != "" && got != want {
			return false
		}
	}
	return true
}

func findString(list []string, want string) (int, bool) {
	for i, s := range list {
		if s == want {
			return i, true
		}
	}
	return 0, false
}

func installLists(paths config.Paths, st *state.State, stage string, m Manifest) error {
	now := time.Now().UTC()
	for _, name := range sortedKeys(m.Lists) {
		entry := m.Lists[name]
		if !list.ValidName(name) {
			return fmt.Errorf("bundle contains an invalid list name %q", name)
		}
		data, err := os.ReadFile(filepath.Join(stage, "lists", name+".json"))
		if err != nil {
			return err
		}
		l, err := list.Write(paths.Lists, name, data)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		st.Lists[name] = &state.List{
			Origin:    entry.Origin,
			SHA256:    hex.EncodeToString(sum[:]),
			FetchedAt: now,
			Builtin:   name == "core",
			Readonly:  m.Frozen,
			Packages:  len(l.Packages),
		}
	}
	return nil
}

func applyBundledShell(cfg *config.Config, st *state.State, m Manifest, res *RunResult) error {
	hasShell := false
	for _, p := range m.Packages {
		if p.Shell != nil && (strings.TrimSpace(p.Shell.Bash) != "" || strings.TrimSpace(p.Shell.Pwsh) != "") {
			hasShell = true
			break
		}
	}
	if !cfg.AllowShellPackages {
		if hasShell {
			res.Warnings = append(res.Warnings, "shell snippets were not installed because allow_shell_packages=false")
		}
		return nil
	}
	for _, p := range m.Packages {
		if p.Shell == nil {
			continue
		}
		if strings.TrimSpace(p.Shell.Bash) == "" && strings.TrimSpace(p.Shell.Pwsh) == "" {
			continue
		}
		hash := p.Shell.SHA256
		if want := (shellinit.Bodies{Bash: p.Shell.Bash, Pwsh: p.Shell.Pwsh}).Hash(); hash == "" {
			hash = want
		} else if !strings.EqualFold(hash, want) {
			return fmt.Errorf("bundle shell snippet for %s does not match its manifest hash", p.Name)
		}
		sh := st.Shell[p.Name]
		if sh == nil {
			sh = &state.Shell{}
			st.Shell[p.Name] = sh
		}
		sh.SnippetSHA256 = hash
		sh.Approved = true
		sh.LastError = ""
		sh.Pending = nil
		sh.PendingSHA256 = ""
		sh.RejectedSHA256 = ""
		sh.SyncPending = false
		sh.Live = &state.ShellSnippet{
			Version: p.Shell.Version,
			Bash:    p.Shell.Bash,
			Pwsh:    p.Shell.Pwsh,
		}
	}
	return nil
}

// installSelf writes the base executable (everything before the payload) to
// bin/cpt so the target gets a normal cpt binary, not another installer.
func installSelf(c *Container, binDir string) (string, string, error) {
	name := "cpt"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dest := filepath.Join(binDir, name)
	tmp, err := os.CreateTemp(binDir, ".cpt-self-*")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.CopyN(tmp, c.Base(), int64(c.Footer.PayloadOffset)); err != nil {
		tmp.Close()
		return "", "", fmt.Errorf("write %s: %w", dest, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		if runtime.GOOS == "windows" {
			// A running cpt.exe cannot be replaced; stage the swap for the next
			// start, matching the flat-bin manager's .new behavior.
			alt := dest + ".new"
			if altErr := os.Rename(tmpName, alt); altErr == nil {
				return alt, fmt.Sprintf("%s is in use; new cpt written to %s and will be used by the next cpt invocation", dest, alt), nil
			}
		}
		return "", "", fmt.Errorf("install %s: %w", dest, err)
	}
	return dest, "", nil
}

// SwapPendingSelf replaces a cpt executable with the staged .new copy left by a
// Windows bundle install. It is a no-op elsewhere. Replacing a running image is
// impossible on Windows, so the caller should surface the error as a warning.
func SwapPendingSelf() error {
	if runtime.GOOS != "windows" {
		return nil
	}
	path, err := SelfPath()
	if err != nil {
		return nil
	}
	pending := path + ".new"
	if _, err := os.Stat(pending); err != nil {
		return nil
	}
	if err := os.Rename(pending, path); err == nil {
		return nil
	}
	// Move the current image aside, then place the new one.
	old := path + ".old"
	_ = os.Remove(old)
	if err := os.Rename(path, old); err != nil {
		return fmt.Errorf("cannot swap in %s while cpt is running; close cpt and rename it manually", pending)
	}
	if err := os.Rename(pending, path); err != nil {
		_ = os.Rename(old, path)
		return fmt.Errorf("cannot move %s into place: %w", pending, err)
	}
	_ = os.Remove(old)
	return nil
}

func writeInitFiles(paths config.Paths, cfg *config.Config, st *state.State, res *RunResult) (string, error) {
	home, _ := shellinit.HomeDir()
	opt := shellinit.Options{Root: paths.Root, Bin: cfg.BinDirFor(paths), Home: home}
	var bashPath string
	for _, fl := range []shellinit.Flavor{shellinit.FlavorBash, shellinit.FlavorPwsh} {
		if fl == shellinit.FlavorBash {
			bashPath = filepath.Join(paths.Root, shellinit.ScriptName(fl))
		}
		script := shellinit.Render(fl, opt, stateShellBlocks(st, cfg.AllowShellPackages, fl))
		path := filepath.Join(paths.Root, shellinit.ScriptName(fl))
		existing, err := os.ReadFile(path)
		switch {
		case err == nil && string(existing) == script:
			continue
		case err == nil && !generatedInit(existing):
			res.Warnings = append(res.Warnings, path+" was not generated by cpt; leaving it untouched")
			continue
		case err == nil:
			if err := fsutil.WriteFileAtomic(path, []byte(script), 0o644); err != nil {
				return "", fmt.Errorf("write %s: %w", path, err)
			}
		case os.IsNotExist(err):
			if err := fsutil.WriteFileAtomic(path, []byte(script), 0o644); err != nil {
				return "", fmt.Errorf("write %s: %w", path, err)
			}
		default:
			return "", fmt.Errorf("read %s: %w", path, err)
		}
	}
	return bashPath, nil
}

func stateShellBlocks(st *state.State, allow bool, fl shellinit.Flavor) []shellinit.Block {
	if !allow || st == nil {
		return nil
	}
	names := make([]string, 0, len(st.Shell))
	for name := range st.Shell {
		names = append(names, name)
	}
	sort.Strings(names)
	var blocks []shellinit.Block
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
			SHA256:  shellinit.Bodies{Bash: sh.Live.Bash, Pwsh: sh.Live.Pwsh}.Hash(),
			Body:    body,
		})
	}
	return blocks
}

func generatedInit(data []byte) bool {
	text := strings.TrimPrefix(string(data), "\ufeff")
	return strings.TrimSpace(text) == "" || strings.HasPrefix(text, "# generated by cpt")
}

func installInitBlock(ctx context.Context, paths config.Paths, binDir string) error {
	home, err := shellinit.HomeDir()
	if err != nil {
		return err
	}
	shell := detectShell()
	rc, err := shellinit.ProfilePath(ctx, shell, home)
	if err != nil {
		return err
	}
	block := shellinit.MarkerBlock(shell, filepath.Join(paths.Root, shellinit.ScriptName(shell.Flavor())),
		shellinit.Options{Root: paths.Root, Bin: binDir, Home: home})
	_, err = shellinit.EditRCFile(rc, true, block)
	return err
}

// validateDest refuses to treat an arbitrary existing directory (or a system
// directory) as a cpt root. Accidentally running a bundle with --dest / would
// otherwise mix cpt files into the filesystem root.
// stageDir creates a staging directory next to dest (same filesystem) so large
// payloads do not exhaust a small system tmp, falling back to the OS temp.
func stageDir(dest string) (string, error) {
	abs, err := filepath.Abs(dest)
	if err != nil {
		abs = dest
	}
	if dir, err := os.MkdirTemp(filepath.Dir(abs), ".cpt-stage-*"); err == nil {
		return dir, nil
	}
	return os.MkdirTemp("", "cpt-bundle-*")
}

func validateDest(dest string) error {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	clean := filepath.Clean(abs)
	if clean == string(filepath.Separator) || filepath.Dir(clean) == clean {
		return cerr.New(cerr.Usage, "refusing to install into the filesystem root %s", clean)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		home = filepath.Clean(home)
		if clean == home {
			return cerr.New(cerr.Usage, "refusing to install into the home directory itself; choose a subdirectory")
		}
		if strings.HasPrefix(home, clean+string(filepath.Separator)) {
			return cerr.New(cerr.Usage, "refusing to install into %s, which contains the home directory", clean)
		}
	}
	info, err := os.Stat(clean)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return err
	case !info.IsDir():
		return cerr.New(cerr.Usage, "destination %s is not a directory", clean)
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	_, stateErr := os.Stat(filepath.Join(clean, "state.json"))
	_, lockErr := os.Stat(filepath.Join(clean, ".lock"))
	if stateErr != nil && lockErr != nil {
		// A pre-created root may hold only config or empty cpt directories, for
		// example to set bin_dir before the first install.
		for _, e := range entries {
			switch e.Name() {
			case "config.json", "cptinit.sh", "cptinit.ps1":
				continue
			case "bin", "lists", "cache", "logs", "bundles", "tmp":
				sub, err := os.ReadDir(filepath.Join(clean, e.Name()))
				if err == nil && len(sub) == 0 {
					continue
				}
			}
			return cerr.New(cerr.Usage,
				"destination %s is not empty and has no cpt state; use a new --dest directory", clean)
		}
	}
	for _, name := range []string{"bin", "lists", "cache", "logs", "bundles", "tmp"} {
		path := filepath.Join(clean, name)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return cerr.New(cerr.Usage, "destination component %s is a symlink; choose a real cpt root", path)
		}
		if !info.IsDir() {
			return cerr.New(cerr.Usage, "destination component %s is not a directory", path)
		}
	}
	return nil
}

func detectShell() shellinit.Shell {
	sh := strings.ToLower(filepath.Base(os.Getenv("SHELL")))
	if strings.Contains(sh, "zsh") {
		return shellinit.ShellZsh
	}
	if runtime.GOOS == "windows" {
		return shellinit.ShellPwsh
	}
	return shellinit.ShellBash
}
