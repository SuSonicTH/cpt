// Package resolve turns a list entry into a concrete download plan.
package resolve

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/SuSonicTH/cpt/internal/archive"
	"github.com/SuSonicTH/cpt/internal/fetch"
	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/platform"
	"github.com/SuSonicTH/cpt/internal/source"
)

// Resolution is everything needed to download, verify and install a package.
type Resolution struct {
	Name      string
	List      string
	Platform  platform.Platform
	SourceID  string
	Repo      string
	Canonical string
	Tag       string
	Version   string
	Asset     string
	URL       string
	Format    string
	Pinned    bool
	// AssetVersioned is true when the asset template embeds {tag} or {version}.
	AssetVersioned bool
	Checksums      []fetch.Candidate
	Entry          *list.Package
}

// Options overrides the version chosen from the list or user pin and can supply
// a previously installed tag so a pinned reinstall needs no probe.
type Options struct {
	CLIVersion   string
	UserPin      string
	InstalledTag string
	InstalledVer string
}

const defaultStrip = `^v`

var defaultChecksums = []string{
	"{asset}.sha256",
	"{asset}.sha",
	"SHA256SUMS",
	"sha256sums.txt",
	"checksums.txt",
	"checksums",
	"SHA2-256SUMS",
	"{name}_{version}_checksums.txt",
}

// Package resolves one list entry for plat.
func Package(ctx context.Context, prober *source.Prober, entry list.Entry, plat platform.Platform, opt Options) (*Resolution, error) {
	pkg := entry.Pkg
	var template string
	if pkg.Source.Type == "github" {
		var ok bool
		template, ok = pkg.PlatformAsset(plat)
		if !ok {
			return nil, fmt.Errorf("%s does not support %s (supported: %s)", pkg.Name, plat, strings.Join(pkg.Platforms(), ", "))
		}
	}

	requested := strings.TrimSpace(opt.CLIVersion)
	if requested == "" {
		requested = strings.TrimSpace(opt.UserPin)
	}
	if requested == "" {
		requested = strings.TrimSpace(pkg.Version)
	}

	res := &Resolution{
		Name:     pkg.Name,
		List:     entry.List,
		Platform: plat,
		Entry:    pkg,
		Pinned:   requested != "",
	}

	switch pkg.Source.Type {
	case "github":
		if prober == nil {
			return nil, fmt.Errorf("%s: no version resolver available", pkg.Name)
		}
		strip := pkg.Source.VersionStrip
		tag := ""
		canonical := pkg.Source.Repo
		switch {
		case requested != "" && isTagLike(requested, strip):
			// The caller supplied a full release tag, so no probe is needed.
			tag = requested
			if cached, ok := prober.Cached(pkg.Source.Repo); ok {
				canonical = cached.Canonical
			}
		case requested != "" && requested == opt.InstalledVer && opt.InstalledTag != "":
			// Reinstalling the version that is already recorded needs no probe.
			tag = opt.InstalledTag
			if cached, ok := prober.Cached(pkg.Source.Repo); ok {
				canonical = cached.Canonical
			}
		default:
			probe, err := prober.Latest(ctx, pkg.Source.Repo)
			if err != nil {
				return nil, err
			}
			tag = probe.Tag
			canonical = probe.Canonical
			if requested != "" && requested != versionOf(probe.Tag, strip) {
				tag, err = tagForVersion(probe.Tag, strip, requested)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", pkg.Name, err)
				}
			}
		}
		res.Repo = pkg.Source.Repo
		res.Canonical = canonical
		res.Tag = tag
		res.Version = versionOf(tag, strip)
		res.SourceID = "github:" + canonical
		res.Asset = interpolate(template, tag, res.Version)
		res.AssetVersioned = strings.Contains(template, "{tag}") || strings.Contains(template, "{version}")
		res.URL = prober.ReleaseURL(canonical, tag, res.Asset)
	case "url":
		if requested == "" {
			return nil, fmt.Errorf("%s: url source needs a pinned version", pkg.Name)
		}
		res.Version = requested
		res.Tag = requested
		res.SourceID = "url:" + pkg.Source.URL
		res.URL = strings.NewReplacer(
			"{tag}", requested,
			"{version}", requested,
			"{os}", plat.OS,
			"{arch}", plat.Arch,
		).Replace(pkg.Source.URL)
		res.Asset = assetName(res.URL)
		if res.Asset == "" {
			return nil, fmt.Errorf("%s: cannot derive asset name from %s", pkg.Name, res.URL)
		}
	default:
		return nil, fmt.Errorf("%s: unsupported source type %q", pkg.Name, pkg.Source.Type)
	}

	res.Format = pkg.Source.Format
	if res.Format == "" || res.Format == "auto" {
		res.Format = archive.DetectFormat(res.Asset)
	}
	res.Checksums = checksumCandidates(pkg, prober, res)
	return res, nil
}

// VersionOf strips the configured prefix from a tag.
func VersionOf(tag, strip string) string { return versionOf(tag, strip) }

// ChecksumCandidates returns the checksum files to try for res.
func ChecksumCandidates(pkg *list.Package, prober *source.Prober, res *Resolution) []fetch.Candidate {
	return checksumCandidates(pkg, prober, res)
}

func checksumCandidates(pkg *list.Package, prober *source.Prober, res *Resolution) []fetch.Candidate {
	names := make([]string, 0, len(pkg.Source.Checksums)+len(defaultChecksums))
	names = append(names, pkg.Source.Checksums...)
	names = append(names, defaultChecksums...)

	seen := map[string]bool{}
	out := make([]fetch.Candidate, 0, len(names))
	for _, tmpl := range names {
		name := strings.NewReplacer(
			"{asset}", res.Asset,
			"{tag}", res.Tag,
			"{version}", res.Version,
			"{name}", pkg.Name,
			"{os}", res.Platform.OS,
			"{arch}", res.Platform.Arch,
		).Replace(tmpl)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		var url string
		switch pkg.Source.Type {
		case "github":
			url = prober.ReleaseURL(res.Canonical, res.Tag, name)
		default:
			url = siblingURL(res.URL, name)
		}
		out = append(out, fetch.Candidate{Name: name, URL: url})
	}
	return out
}

func interpolate(tmpl, tag, version string) string {
	return strings.NewReplacer("{tag}", tag, "{version}", version).Replace(tmpl)
}

func assetName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return path.Base(raw)
	}
	base := path.Base(u.Path)
	if decoded, err := url.PathUnescape(base); err == nil {
		base = decoded
	}
	if base == "." || base == "/" {
		return ""
	}
	return base
}

func siblingURL(raw, name string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw + "/" + name
	}
	dir := path.Dir(u.Path)
	if dir == "." {
		dir = ""
	}
	u.Path = dir + "/" + name
	u.RawQuery = ""
	return u.String()
}

func versionOf(tag, strip string) string {
	re := regexp.MustCompile(stripPattern(strip))
	return re.ReplaceAllString(tag, "")
}

// isTagLike reports whether s already looks like a release tag rather than a
// bare version: stripping the configured prefix changes it.
func isTagLike(s, strip string) bool {
	return versionOf(s, strip) != s
}

func stripPattern(strip string) string {
	if strings.TrimSpace(strip) == "" {
		return defaultStrip
	}
	return strip
}

// tagForVersion reconstructs a release tag from a version string using the
// prefix observed on the latest tag (for example "v" or "jq-").
func tagForVersion(latestTag, strip, version string) (string, error) {
	latestVersion := versionOf(latestTag, strip)
	if version == latestVersion {
		return latestTag, nil
	}
	if !strings.HasSuffix(latestTag, latestVersion) {
		return "", fmt.Errorf("cannot derive tag for version %s from tag %s", version, latestTag)
	}
	prefix := strings.TrimSuffix(latestTag, latestVersion)
	candidate := prefix + version
	if versionOf(candidate, strip) != version {
		return "", fmt.Errorf("cannot derive tag for version %s from tag %s", version, latestTag)
	}
	return candidate, nil
}
