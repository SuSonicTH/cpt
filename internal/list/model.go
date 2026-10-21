// Package list defines the cpt.json package-list schema.
package list

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/SuSonicTH/cpt/internal/platform"
)

// CurrentSchema is the list schema version understood by this build.
const CurrentSchema = 1

// Formats accepted in source.format.
var Formats = map[string]bool{
	"":        true,
	"auto":    true,
	"tar.gz":  true,
	"tgz":     true,
	"tar.xz":  true,
	"txz":     true,
	"tar.zst": true,
	"tzst":    true,
	"zip":     true,
	"raw":     true,
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// List is a cpt.json document.
type List struct {
	Schema      int       `json:"schema"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Homepage    string    `json:"homepage,omitempty"`
	Maintainer  string    `json:"maintainer,omitempty"`
	Updated     string    `json:"updated,omitempty"`
	Packages    []Package `json:"packages"`
}

// Package describes one installable tool.
type Package struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	License     string   `json:"license,omitempty"`
	Version     string   `json:"version,omitempty"`
	Source      Source   `json:"source"`
	Shell       *Shell   `json:"shell,omitempty"`
	Requires    []string `json:"requires,omitempty"`
}

// Source describes where a package's binaries come from.
type Source struct {
	Type         string            `json:"type"`
	Repo         string            `json:"repo,omitempty"`
	URL          string            `json:"url,omitempty"`
	VersionCheck string            `json:"version_check,omitempty"`
	VersionStrip string            `json:"version_strip,omitempty"`
	Format       string            `json:"format,omitempty"`
	Asset        map[string]string `json:"asset,omitempty"`
	Bins         []string          `json:"bins,omitempty"`
	Checksums    []string          `json:"checksums,omitempty"`
	StripPrefix  *bool             `json:"strip_prefix,omitempty"`
}

// Shell declares shell snippets for a package. Used from M2 onwards.
type Shell struct {
	Bash *ShellFlavor `json:"bash,omitempty"`
	Pwsh *ShellFlavor `json:"pwsh,omitempty"`
}

// ShellFlavor is one shell dialect's snippet and environment.
type ShellFlavor struct {
	Functions       string            `json:"functions,omitempty"`
	FunctionsURL    string            `json:"functions_url,omitempty"`
	FunctionsSHA256 string            `json:"functions_sha256,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Path            []string          `json:"path,omitempty"`
}

// Parse decodes and validates a list document. source names the origin used in
// error messages.
func Parse(data []byte, source string) (*List, error) {
	var l List
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("parse %s: %w", source, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("parse %s: trailing data after the list document", source)
	}
	if err := l.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", source, err)
	}
	return &l, nil
}

// Validate checks the whole document.
func (l *List) Validate() error {
	if l.Schema <= 0 {
		l.Schema = CurrentSchema
	}
	if l.Schema > CurrentSchema {
		return fmt.Errorf("schema %d is newer than supported (%d)", l.Schema, CurrentSchema)
	}
	l.Name = strings.TrimSpace(l.Name)
	if l.Name == "" {
		return fmt.Errorf("list has no name")
	}
	if !ValidName(l.Name) {
		return fmt.Errorf("list name %q must match %s", l.Name, nameRE)
	}
	seen := map[string]bool{}
	for i := range l.Packages {
		p := &l.Packages[i]
		if err := p.Validate(); err != nil {
			return fmt.Errorf("package %d (%s): %w", i, p.Name, err)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate package %q", p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// Validate checks a single package entry.
func (p *Package) Validate() error {
	p.Name = strings.TrimSpace(p.Name)
	if !ValidName(p.Name) {
		return fmt.Errorf("name must match %s", nameRE)
	}
	if p.Version != "" {
		if err := ValidateVersion(p.Version); err != nil {
			return err
		}
	}
	switch p.Source.Type {
	case "github":
		parts := strings.Split(p.Source.Repo, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("source.repo must be owner/name")
		}
		if len(p.Source.Asset) == 0 {
			return fmt.Errorf("source.asset must map platforms to asset names")
		}
	case "url":
		if p.Source.URL == "" {
			return fmt.Errorf("source.url is required")
		}
		if p.Version == "" {
			return fmt.Errorf("source type url requires a pinned version")
		}
		if p.Source.VersionCheck != "" && p.Source.VersionCheck != "none" {
			return fmt.Errorf("source.version_check %q is not supported", p.Source.VersionCheck)
		}
	default:
		return fmt.Errorf("source.type must be github or url, got %q", p.Source.Type)
	}
	for key, asset := range p.Source.Asset {
		if _, err := platform.Parse(key); err != nil {
			return fmt.Errorf("source.asset key: %w", err)
		}
		if strings.TrimSpace(asset) == "" {
			return fmt.Errorf("source.asset[%s] is empty", key)
		}
	}
	if !Formats[p.Source.Format] {
		return fmt.Errorf("unknown format %q", p.Source.Format)
	}
	if p.Source.VersionStrip != "" {
		if _, err := regexp.Compile(p.Source.VersionStrip); err != nil {
			return fmt.Errorf("version_strip: %w", err)
		}
	}
	for _, bin := range p.Source.Bins {
		if bin == "" || strings.ContainsAny(bin, `/\`) {
			return fmt.Errorf("invalid binary name %q", bin)
		}
	}
	for _, c := range p.Source.Checksums {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("empty checksum candidate")
		}
	}
	if p.Shell != nil {
		if err := p.Shell.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ValidName reports whether s is a valid list or package name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// PlatformAsset returns the asset template for plat and whether it exists.
func (p *Package) PlatformAsset(plat platform.Platform) (string, bool) {
	if p.Source.Asset == nil {
		return "", false
	}
	asset, ok := p.Source.Asset[plat.String()]
	return asset, ok
}

// Platforms lists the platforms a package supports, sorted by key.
func (p *Package) Platforms() []string {
	out := make([]string, 0, len(p.Source.Asset))
	for key := range p.Source.Asset {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Slug converts a human readable name into a filesystem-safe list identifier.
func Slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "list"
	}
	return out
}
