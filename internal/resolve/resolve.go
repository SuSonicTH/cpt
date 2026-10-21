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
	Checksums []fetch.Candidate
	Entry     *list.Package
}

// Options overrides the version chosen from the list or user pin.
type Options struct {
	CLIVersion string
	UserPin    string
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
		probe, err := prober.Latest(ctx, pkg.Source.Repo)
		if err != nil {
			return nil, err
		}
		version := versionOf(probe.Tag, pkg.Source.VersionStrip)
		tag := probe.Tag
		if requested != "" && requested != version {
			tag, err = tagForVersion(probe.Tag, pkg.Source.VersionStrip, requested)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", pkg.Name, err)
			}
			version = requested
		}
		res.Repo = pkg.Source.Repo
		res.Canonical = probe.Canonical
		res.Tag = tag
		res.Version = version
		res.SourceID = "github:" + probe.Canonical
		res.Asset = interpolate(template, tag, version)
		res.URL = prober.ReleaseURL(probe.Canonical, tag, res.Asset)
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
	// A caller may pass a full tag; accept it when the prefix is already present.
	if prefix != "" && strings.HasPrefix(version, prefix) && versionOf(version, strip) != version {
		return version, nil
	}
	candidate := prefix + version
	if versionOf(candidate, strip) != version {
		return "", fmt.Errorf("cannot derive tag for version %s from tag %s", version, latestTag)
	}
	return candidate, nil
}
