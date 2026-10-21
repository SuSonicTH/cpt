// Package source resolves upstream release versions without using the GitHub
// API. It relies on the redirect issued by /releases/latest.
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SuSonicTH/cpt/internal/fsutil"
	"github.com/SuSonicTH/cpt/internal/httpc"
)

// ErrNoReleases means the repository publishes no GitHub releases. Tag-only
// repositories are not supported.
var ErrNoReleases = errors.New("repository has no GitHub releases")

// DefaultBaseURL is the public GitHub host.
const DefaultBaseURL = "https://github.com"

// Probe is a cached answer to "what is the latest release tag?".
type Probe struct {
	Repo      string    `json:"repo"`
	Canonical string    `json:"canonical"`
	Tag       string    `json:"tag"`
	FetchedAt time.Time `json:"fetched_at"`
}

// Prober resolves latest release tags.
type Prober struct {
	Client   *http.Client
	BaseURL  string
	CacheDir string
	TTL      time.Duration
	Offline  bool
	NoCache  bool
	Log      *slog.Logger
}

// Latest returns the newest release tag for repo. Results are cached on disk
// for TTL. In offline mode only the cache is consulted.
func (p *Prober) Latest(ctx context.Context, repo string) (Probe, error) {
	repo = strings.TrimPrefix(strings.TrimSuffix(repo, "/"), "/")
	if !validRepo(repo) {
		return Probe{}, fmt.Errorf("invalid repository %q: want owner/name", repo)
	}
	cachePath := p.cachePath(repo)
	if !p.NoCache {
		if cached, ok := readCache(cachePath); ok {
			if p.Offline || (p.TTL > 0 && time.Since(cached.FetchedAt) < p.TTL) {
				return cached, nil
			}
		}
	}
	if p.Offline {
		if cached, ok := readCache(cachePath); ok {
			return cached, nil
		}
		return Probe{}, fmt.Errorf("offline and no cached version for %s", repo)
	}

	tag, canonical, err := p.probe(ctx, repo)
	if err != nil {
		return Probe{}, err
	}
	result := Probe{
		Repo:      repo,
		Canonical: canonical,
		Tag:       tag,
		FetchedAt: time.Now().UTC(),
	}
	if p.CacheDir != "" && !p.NoCache {
		if err := writeCache(cachePath, result); err != nil && p.Log != nil {
			p.Log.Warn("could not cache version probe", "repo", repo, "error", err)
		}
	}
	return result, nil
}

// probe walks the redirect chain of /releases/latest and extracts the tag from
// the final /releases/tag/<tag> location.
func (p *Prober) probe(ctx context.Context, repo string) (tag, canonical string, err error) {
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	client := p.noRedirectClient()
	current := fmt.Sprintf("%s/%s/releases/latest", strings.TrimSuffix(base, "/"), repo)
	for hop := 0; hop < 8; hop++ {
		resp, reqErr := httpc.Head(ctx, client, current)
		if reqErr != nil {
			return "", "", fmt.Errorf("probe %s: %w", repo, reqErr)
		}
		resp.Body.Close()

		switch {
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			loc := resp.Header.Get("Location")
			if loc == "" {
				return "", "", fmt.Errorf("probe %s: redirect without Location", repo)
			}
			next, parseErr := resolveURL(current, loc)
			if parseErr != nil {
				return "", "", fmt.Errorf("probe %s: %w", repo, parseErr)
			}
			if t, owner, repoName, ok := parseTagPath(next); ok {
				return t, owner + "/" + repoName, nil
			}
			current = next
		case resp.StatusCode == http.StatusOK:
			if t, owner, repoName, ok := parseTagPath(current); ok {
				return t, owner + "/" + repoName, nil
			}
			return "", "", fmt.Errorf("probe %s: unexpected response %s", repo, current)
		case resp.StatusCode == http.StatusNotFound:
			return "", "", fmt.Errorf("%s: %w", repo, ErrNoReleases)
		default:
			return "", "", fmt.Errorf("probe %s: HTTP %d", repo, resp.StatusCode)
		}
	}
	return "", "", fmt.Errorf("probe %s: too many redirects", repo)
}

// noRedirectClient returns a copy of the configured client that reports
// redirects instead of following them. Callers may pass a normal client.
func (p *Prober) noRedirectClient() *http.Client {
	base := p.Client
	if base == nil {
		base = http.DefaultClient
	}
	c := *base
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c
}

// ReleaseURL returns the download URL for an asset of a specific tag.
func (p *Prober) ReleaseURL(repo, tag, asset string) string {
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return fmt.Sprintf("%s/%s/releases/download/%s/%s",
		strings.TrimSuffix(base, "/"), repo, escapePath(tag), escapePath(asset))
}

// LatestAssetURL is the tag-less download URL, useful when an asset name does
// not contain the version.
func (p *Prober) LatestAssetURL(repo, asset string) string {
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return fmt.Sprintf("%s/%s/releases/latest/download/%s",
		strings.TrimSuffix(base, "/"), repo, escapePath(asset))
}

func (p *Prober) cachePath(repo string) string {
	if p.CacheDir == "" {
		return ""
	}
	return filepath.Join(p.CacheDir, hashKey(repo)+".json")
}

func readCache(path string) (Probe, bool) {
	if path == "" {
		return Probe{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Probe{}, false
	}
	var pr Probe
	if err := json.Unmarshal(data, &pr); err != nil || pr.Tag == "" {
		return Probe{}, false
	}
	return pr, true
}

func writeCache(path string, pr Probe) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(pr, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'), 0o644)
}

func validRepo(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return false
	}
	for _, s := range parts {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	return true
}

// parseTagPath recognises /owner/repo/releases/tag/<tag>.
func parseTagPath(raw string) (tag, owner, repo string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 5 {
		return "", "", "", false
	}
	if parts[2] != "releases" || parts[3] != "tag" {
		return "", "", "", false
	}
	tag = strings.Join(parts[4:], "/")
	if decoded, err := url.PathUnescape(tag); err == nil {
		tag = decoded
	}
	if tag == "" {
		return "", "", "", false
	}
	return tag, parts[0], parts[1], true
}

func resolveURL(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

func escapePath(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func hashKey(s string) string {
	// FNV-1a is sufficient for cache file names and keeps this dependency-free.
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)
}
