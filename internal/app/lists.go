package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/httpc"
	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/platform"
	"github.com/SuSonicTH/cpt/internal/resolve"
	"github.com/SuSonicTH/cpt/internal/state"
)

const maxListSize = 8 << 20

// RefreshResult reports the outcome of refreshing one list.
type RefreshResult struct {
	Name     string `json:"name"`
	Origin   string `json:"origin"`
	Changed  bool   `json:"changed"`
	Packages int    `json:"packages"`
}

// Refresh refreshes the named lists, or every known list when names is empty.
func (a *App) Refresh(ctx context.Context, names []string, force bool) ([]RefreshResult, error) {
	if a.Offline {
		return nil, fmt.Errorf("cannot refresh lists in offline mode")
	}
	if len(names) == 0 {
		for name := range a.State.Lists {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	var results []RefreshResult
	for _, name := range names {
		meta, ok := a.State.Lists[name]
		if !ok {
			return nil, fmt.Errorf("unknown list %q", name)
		}
		if meta.Readonly {
			return nil, fmt.Errorf("list %q is read-only", name)
		}
		changed, count, err := a.refreshListChanged(ctx, name, meta, force)
		if err != nil {
			return nil, fmt.Errorf("refresh %s: %w", name, err)
		}
		results = append(results, RefreshResult{Name: name, Origin: meta.Origin, Changed: changed, Packages: count})
	}
	return results, nil
}

func (a *App) refreshList(ctx context.Context, name string, meta *state.List, force bool) error {
	_, _, err := a.refreshListChanged(ctx, name, meta, force)
	return err
}

// refreshListChanged fetches a list if needed. It returns whether the content
// changed and how many packages the list contains.
func (a *App) refreshListChanged(ctx context.Context, name string, meta *state.List, force bool) (bool, int, error) {
	origin := meta.Origin
	if origin == "" && name == "core" {
		origin = "https://raw.githubusercontent.com/SuSonicTH/cpt/master/cpt.json"
	}
	if origin == "" {
		return false, 0, fmt.Errorf("list %q has no origin", name)
	}
	header := http.Header{}
	if meta.ETag != "" {
		header.Set("If-None-Match", meta.ETag)
	}
	data, hdr, err := httpc.Get(ctx, a.Client, origin, maxListSize, header)
	if err != nil {
		return false, 0, err
	}
	if data == nil {
		// 304 Not Modified.
		meta.FetchedAt = time.Now().UTC()
		return false, meta.Packages, nil
	}
	l, err := list.Parse(data, origin)
	if err != nil {
		return false, 0, err
	}
	if _, err := list.Write(a.Paths.Lists, name, data); err != nil {
		return false, 0, err
	}
	sum := sha256.Sum256(data)
	meta.Origin = origin
	meta.ETag = hdr.Get("ETag")
	meta.SHA256 = hex.EncodeToString(sum[:])
	meta.FetchedAt = time.Now().UTC()
	meta.Packages = len(l.Packages)
	return true, len(l.Packages), nil
}

// AddListOptions holds flags for AddList.
type AddListOptions struct {
	Name    string
	Replace bool
}

// AddResult reports a newly added list.
type AddResult struct {
	Name     string `json:"name"`
	Origin   string `json:"origin"`
	Packages int    `json:"packages"`
}

// AddList installs a new list from an HTTP URL or a GitHub repository spec.
func (a *App) AddList(ctx context.Context, spec string, opts AddListOptions) (AddResult, error) {
	parsed, err := ParseListSpec(spec)
	if err != nil {
		return AddResult{}, err
	}
	data, hdr, err := httpc.Get(ctx, a.Client, parsed.Origin, maxListSize, nil)
	if err != nil {
		return AddResult{}, fmt.Errorf("fetch %s: %w", parsed.Origin, err)
	}
	l, err := list.Parse(data, parsed.Origin)
	if err != nil {
		return AddResult{}, err
	}
	name := list.Slug(firstNonEmpty(opts.Name, l.Name, parsed.Name))
	if existing, exists := a.State.Lists[name]; exists {
		if existing.Builtin || name == "core" {
			return AddResult{}, cerr.New(cerr.Conflict, "the builtin list %q cannot be replaced", name)
		}
		if !opts.Replace {
			return AddResult{}, cerr.New(cerr.Conflict, "list %q already exists (use --replace)", name)
		}
	}
	if name == "core" {
		return AddResult{}, cerr.New(cerr.Conflict, "the name %q is reserved for the builtin list", name)
	}
	if !opts.Replace {
		if _, err := os.Stat(filepath.Join(a.Paths.Lists, list.FileName(name))); err == nil {
			return AddResult{}, cerr.New(cerr.Conflict, "list file %s already exists (use --replace)", list.FileName(name))
		}
	}
	if _, err := list.Write(a.Paths.Lists, name, data); err != nil {
		return AddResult{}, err
	}
	sum := sha256.Sum256(data)
	a.State.Lists[name] = &state.List{
		Origin:    parsed.Origin,
		ETag:      hdr.Get("ETag"),
		SHA256:    hex.EncodeToString(sum[:]),
		FetchedAt: time.Now().UTC(),
		Packages:  len(l.Packages),
	}
	return AddResult{Name: name, Origin: parsed.Origin, Packages: len(l.Packages)}, nil
}

// RemoveList deletes a list file and its metadata. The builtin list cannot be
// removed. It returns how many installed packages came from the list.
func (a *App) RemoveList(name string) (int, error) {
	meta, ok := a.State.Lists[name]
	if !ok {
		return 0, fmt.Errorf("unknown list %q", name)
	}
	if meta.Builtin {
		return 0, fmt.Errorf("the builtin list %q cannot be removed", name)
	}
	installed := 0
	for _, p := range a.State.Packages {
		if p.List == name {
			installed++
		}
	}
	if err := os.Remove(filepath.Join(a.Paths.Lists, list.FileName(name))); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	delete(a.State.Lists, name)
	return installed, nil
}

// ListInfo describes one configured list.
type ListInfo struct {
	Name      string    `json:"name"`
	Origin    string    `json:"origin"`
	Packages  int       `json:"packages"`
	FetchedAt time.Time `json:"fetched_at"`
	Builtin   bool      `json:"builtin"`
	Readonly  bool      `json:"readonly"`
	Stale     bool      `json:"stale"`
}

// Lists describes every configured list.
func (a *App) Lists() []ListInfo {
	names := make([]string, 0, len(a.State.Lists))
	for name := range a.State.Lists {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ListInfo, 0, len(names))
	for _, name := range names {
		meta := a.State.Lists[name]
		stale := meta.FetchedAt.IsZero() || time.Since(meta.FetchedAt) > a.Cfg.ListTTL()
		out = append(out, ListInfo{
			Name:      name,
			Origin:    meta.Origin,
			Packages:  meta.Packages,
			FetchedAt: meta.FetchedAt,
			Builtin:   meta.Builtin,
			Readonly:  meta.Readonly,
			Stale:     stale,
		})
	}
	return out
}

// ListSpec is a parsed list source specification.
type ListSpec struct {
	Origin string
	Name   string
}

// ParseListSpec accepts an https URL or a repository spec of the form
// [github.com/]owner/repo[#ref[:path]].
func ParseListSpec(spec string) (ListSpec, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return ListSpec{}, cerr.New(cerr.Usage, "empty list specification")
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return ListSpec{}, cerr.New(cerr.Usage, "unsupported URL %q: only http and https are allowed", spec)
		}
		name := strings.TrimSuffix(path.Base(u.Path), ".json")
		if u.Host == "raw.githubusercontent.com" {
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) >= 4 {
				name = parts[1]
			}
		}
		return ListSpec{Origin: s, Name: name}, nil
	}

	s = strings.TrimPrefix(s, "github.com/")
	repo := s
	ref := "HEAD"
	file := "cpt.json"
	if i := strings.Index(s, "#"); i >= 0 {
		repo = s[:i]
		rest := s[i+1:]
		if j := strings.Index(rest, ":"); j >= 0 {
			ref = rest[:j]
			file = rest[j+1:]
		} else if rest != "" {
			ref = rest
		}
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ListSpec{}, cerr.New(cerr.Usage, "invalid repository %q: want owner/repo", repo)
	}
	if ref == "" || file == "" {
		return ListSpec{}, cerr.New(cerr.Usage, "invalid list spec %q", spec)
	}
	origin := fmt.Sprintf("https://github.com/%s/raw/%s/%s", repo, ref, strings.TrimPrefix(file, "/"))
	name := parts[1]
	if name == "" {
		name = "list"
	}
	return ListSpec{Origin: origin, Name: name}, nil
}

// LintResult is one package/platform check.
type LintResult struct {
	Package  string `json:"package"`
	Platform string `json:"platform"`
	URL      string `json:"url"`
	Status   int    `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Lint validates every package. With network enabled it also checks that each
// asset URL resolves upstream.
func (a *App) Lint(ctx context.Context, network bool) ([]LintResult, error) {
	cat, err := a.Catalog()
	if err != nil {
		return nil, err
	}
	conflicts := cat.Conflicts()
	if len(conflicts) > 0 {
		names := make([]string, 0, len(conflicts))
		for name := range conflicts {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, cerr.New(cerr.Conflict, "packages defined by multiple lists: %s", strings.Join(names, ", "))
	}
	var results []LintResult
	for _, entry := range cat.Entries() {
		if !network {
			results = append(results, LintResult{Package: entry.Pkg.Name, Platform: "*"})
			continue
		}
		for _, key := range entry.Pkg.Platforms() {
			plat, err := platform.Parse(key)
			if err != nil {
				results = append(results, LintResult{Package: entry.Pkg.Name, Platform: key, Error: err.Error()})
				continue
			}
			res, err := resolve.Package(ctx, a.Prober, entry, plat, resolve.Options{})
			if err != nil {
				results = append(results, LintResult{Package: entry.Pkg.Name, Platform: key, Error: err.Error()})
				continue
			}
			resp, err := httpc.Head(ctx, a.Client, res.URL)
			if err != nil {
				results = append(results, LintResult{Package: res.Name, Platform: key, URL: res.URL, Error: err.Error()})
				continue
			}
			resp.Body.Close()
			status := resp.StatusCode
			if status == http.StatusNotFound {
				results = append(results, LintResult{Package: res.Name, Platform: key, URL: res.URL, Status: status, Error: "asset not found"})
				continue
			}
			results = append(results, LintResult{Package: res.Name, Platform: key, URL: res.URL, Status: status})
		}
	}
	return results, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
