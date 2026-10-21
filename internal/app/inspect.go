package app

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/platform"
	"github.com/SuSonicTH/cpt/internal/resolve"
)

// ListOptions filters the package listing.
type ListOptions struct {
	Installed bool
	Available bool
	Updates   bool
	Query     string
}

// Row is one package line in listings.
type Row struct {
	Name           string   `json:"name"`
	List           string   `json:"list"`
	Version        string   `json:"version,omitempty"`
	Latest         string   `json:"latest,omitempty"`
	Status         string   `json:"status"`
	Description    string   `json:"description,omitempty"`
	Tags           string   `json:"tags,omitempty"`
	Homepage       string   `json:"homepage,omitempty"`
	License        string   `json:"license,omitempty"`
	Asset          string   `json:"asset,omitempty"`
	Checksum       string   `json:"checksum_status,omitempty"`
	ChecksumSource string   `json:"checksum_source,omitempty"`
	SHA256         string   `json:"sha256,omitempty"`
	Bins           []string `json:"bins,omitempty"`
	Pinned         bool     `json:"pinned,omitempty"`
	Conflict       bool     `json:"conflict,omitempty"`
	Error          string   `json:"error,omitempty"`
}

// Rows builds the package table. With Updates enabled it probes upstream
// versions for installed packages.
func (a *App) Rows(ctx context.Context, opt ListOptions) ([]Row, error) {
	cat, err := a.Catalog()
	if err != nil {
		return nil, err
	}
	conflicts := cat.Conflicts()
	query := strings.ToLower(strings.TrimSpace(opt.Query))

	var rows []Row
	probeTargets := map[string]string{} // name -> list
	inCatalog := make(map[string]bool, len(cat.Entries()))
	for _, entry := range cat.Entries() {
		pkg := entry.Pkg
		inCatalog[pkg.Name] = true
		if query != "" && !matchesQuery(pkg.Name, pkg.Description, pkg.Tags, query) {
			continue
		}
		installed := a.State.Packages[pkg.Name]
		if opt.Installed && installed == nil {
			continue
		}
		if opt.Available && installed != nil {
			continue
		}
		row := Row{
			Name:        pkg.Name,
			List:        entry.List,
			Description: pkg.Description,
			Tags:        strings.Join(pkg.Tags, ","),
			Homepage:    pkg.Homepage,
			License:     pkg.License,
			Status:      "available",
		}
		if len(conflicts[pkg.Name]) > 1 {
			row.Conflict = true
			row.Status = "conflict"
		}
		if installed != nil {
			row.Version = installed.Version
			row.Pinned = installed.Pinned != ""
			row.Asset = installed.Asset
			row.Checksum = installed.ChecksumStatus
			row.ChecksumSource = installed.ChecksumSource
			row.SHA256 = installed.DownloadSHA256
			row.Bins = sortedKeys(installed.Bins)
			if row.Status != "conflict" {
				row.Status = "installed"
			}
			if opt.Updates {
				probeTargets[pkg.Name] = entry.List
			}
		}
		rows = append(rows, row)
	}

	// Installed packages whose list was removed stay usable; surface them so
	// they can still be selected, verified and removed. They have no upstream
	// entry to probe.
	if !opt.Available {
		for name, p := range a.State.Packages {
			if inCatalog[name] || p == nil {
				continue
			}
			if query != "" && !matchesQuery(name, "", nil, query) {
				continue
			}
			rows = append(rows, Row{
				Name:           name,
				List:           p.List,
				Version:        p.Version,
				Latest:         "",
				Status:         "installed",
				Description:    "installed, but no configured list provides it",
				Asset:          p.Asset,
				Checksum:       p.ChecksumStatus,
				ChecksumSource: p.ChecksumSource,
				SHA256:         p.DownloadSHA256,
				Bins:           sortedKeys(p.Bins),
				Pinned:         p.Pinned != "",
			})
		}
	}
	if opt.Updates && !a.Offline && len(probeTargets) > 0 {
		a.probeLatest(ctx, cat, rows, probeTargets)
		if err := ctx.Err(); err != nil {
			// A cancelled refresh must not replace good rows with spurious
			// "context canceled" errors on every package.
			return nil, err
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].List < rows[j].List
	})
	return rows, nil
}

// probeLatest fills Latest and status for installed rows.
func (a *App) probeLatest(ctx context.Context, cat *list.Catalog, rows []Row, targets map[string]string) {
	type outcome struct {
		version string
		err     string
	}
	results := make(map[string]outcome, len(targets))
	var mu sync.Mutex
	parallel := a.Cfg.Parallel
	if parallel < 1 {
		parallel = 1
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for name := range targets {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var res outcome
			entry, err := cat.Lookup(name)
			if err != nil {
				res.err = err.Error()
			} else if latest, err := resolve.Package(ctx, a.Prober, entry, platform.Current(), resolve.Options{}); err != nil {
				res.err = err.Error()
			} else {
				res.version = latest.Version
			}
			mu.Lock()
			results[name] = res
			mu.Unlock()
		}(name)
	}
	wg.Wait()

	for i := range rows {
		res, ok := results[rows[i].Name]
		if !ok {
			continue
		}
		if res.err != "" {
			rows[i].Error = res.err
			continue
		}
		rows[i].Latest = res.version
		if rows[i].Pinned || rows[i].Status == "conflict" {
			continue
		}
		if rows[i].Version == res.version {
			rows[i].Status = "installed"
		} else {
			rows[i].Status = "update"
		}
	}
}

// Search returns packages matching a free-text query.
func (a *App) Search(ctx context.Context, query string) ([]Row, error) {
	return a.Rows(ctx, ListOptions{Query: query})
}

// Info is the detail view for one package.
type Info struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Homepage    string     `json:"homepage,omitempty"`
	License     string     `json:"license,omitempty"`
	Tags        []string   `json:"tags,omitempty"`
	List        string     `json:"list"`
	Platforms   []string   `json:"platforms,omitempty"`
	Installed   *Installed `json:"installed,omitempty"`
	Latest      *Latest    `json:"latest,omitempty"`
	Shell       *ShellInfo `json:"shell,omitempty"`
}

// Installed describes the locally installed copy.
type Installed struct {
	Version     string            `json:"version"`
	Tag         string            `json:"tag,omitempty"`
	Asset       string            `json:"asset,omitempty"`
	Checksum    string            `json:"checksum,omitempty"`
	ChecksumSrc string            `json:"checksum_source,omitempty"`
	SHA256      string            `json:"sha256,omitempty"`
	Pinned      string            `json:"pinned,omitempty"`
	InstalledAt string            `json:"installed_at,omitempty"`
	Bins        map[string]string `json:"bins,omitempty"`
}

// Latest describes the newest upstream release, when it could be resolved.
type Latest struct {
	Version string `json:"version,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Asset   string `json:"asset,omitempty"`
	URL     string `json:"url,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Detail resolves information about one package.
func (a *App) Detail(ctx context.Context, name string) (*Info, error) {
	cat, err := a.Catalog()
	if err != nil {
		return nil, err
	}
	entry, err := cat.Lookup(name)
	if err != nil {
		return nil, err
	}
	pkg := entry.Pkg
	info := &Info{
		Name:        pkg.Name,
		Description: pkg.Description,
		Homepage:    pkg.Homepage,
		License:     pkg.License,
		Tags:        pkg.Tags,
		List:        entry.List,
		Platforms:   pkg.Platforms(),
	}
	if p, ok := a.State.Packages[name]; ok {
		info.Installed = &Installed{
			Version:     p.Version,
			Tag:         p.Tag,
			Asset:       p.Asset,
			Checksum:    p.ChecksumStatus,
			ChecksumSrc: p.ChecksumSource,
			SHA256:      p.DownloadSHA256,
			Pinned:      p.Pinned,
			InstalledAt: p.InstalledAt.Format("2006-01-02 15:04:05"),
			Bins:        p.Bins,
		}
	}
	if sh := a.State.Shell[name]; sh != nil {
		si := shellInfoFor(name, sh)
		info.Shell = &si
	}
	if !a.Offline {
		res, err := resolve.Package(ctx, a.Prober, entry, platform.Current(), resolve.Options{})
		if err != nil {
			info.Latest = &Latest{Error: err.Error()}
		} else {
			info.Latest = &Latest{Version: res.Version, Tag: res.Tag, Asset: res.Asset, URL: res.URL}
		}
	}
	return info, nil
}

// MatchesQuery reports whether a package matches a lowercase free-text query.
// The CLI uses it directly; the TUI uses it as the fast path before applying
// its more permissive subsequence match.
func MatchesQuery(name, description string, tags []string, query string) bool {
	return matchesQuery(name, description, tags, query)
}

func matchesQuery(name, description string, tags []string, query string) bool {
	if strings.Contains(strings.ToLower(name), query) {
		return true
	}
	if strings.Contains(strings.ToLower(description), query) {
		return true
	}
	for _, tag := range tags {
		if strings.Contains(strings.ToLower(tag), query) {
			return true
		}
	}
	return false
}
