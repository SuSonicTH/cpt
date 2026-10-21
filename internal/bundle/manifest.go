package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/SuSonicTH/cpt/internal/fsutil"
	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/platform"
)

// ManifestSchema is the bundle.json schema version written by this build.
const ManifestSchema = 1

var bundleNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Manifest is bundle.json, the description of a bundle's contents.
type Manifest struct {
	Schema     int                     `json:"schema"`
	Name       string                  `json:"name"`
	Created    time.Time               `json:"created"`
	Target     string                  `json:"target"`
	CptVersion string                  `json:"cpt_version"`
	Frozen     bool                    `json:"frozen"`
	Packages   []ManifestPackage       `json:"packages"`
	Lists      map[string]ManifestList `json:"lists"`
	// Output records where the executable was written, relative to the
	// bundles directory when it lives there. It keeps rebuilds and removal
	// pointing at the same file when --output was used.
	Output string `json:"output,omitempty"`
}

// ManifestPackage is one exact package version baked into a bundle.
type ManifestPackage struct {
	Name           string         `json:"name"`
	List           string         `json:"list,omitempty"`
	Tag            string         `json:"tag"`
	Version        string         `json:"version"`
	Asset          string         `json:"asset"`
	DownloadSHA256 string         `json:"download_sha256"`
	Bins           []string       `json:"bins"`
	ChecksumStatus string         `json:"checksum_status"`
	Shell          *ManifestShell `json:"shell,omitempty"`
}

// ManifestShell is an approved shell snippet carried by a bundle. Bundles are
// explicit artifacts, so approved snippets travel with them.
type ManifestShell struct {
	Version string `json:"version,omitempty"`
	Bash    string `json:"bash,omitempty"`
	Pwsh    string `json:"pwsh,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// ManifestList records a package list shipped in the payload.
type ManifestList struct {
	SHA256   string `json:"sha256"`
	Origin   string `json:"origin,omitempty"`
	Packages int    `json:"packages,omitempty"`
}

// Validate checks the manifest's identifiers and required fields.
func (m *Manifest) Validate() error {
	if m.Schema <= 0 {
		m.Schema = ManifestSchema
	}
	if m.Schema > ManifestSchema {
		return fmt.Errorf("bundle schema %d is newer than supported (%d)", m.Schema, ManifestSchema)
	}
	if !ValidName(m.Name) {
		return fmt.Errorf("invalid bundle name %q", m.Name)
	}
	if m.Output != "" {
		if m.Output == "." || m.Output == ".." {
			return fmt.Errorf("bundle output %q is not a file name", m.Output)
		}
		if !filepath.IsAbs(m.Output) && filepath.Base(m.Output) != m.Output {
			return fmt.Errorf("bundle output %q must be a bare file name or an absolute path", m.Output)
		}
	}
	if strings.TrimSpace(m.Target) == "" {
		return fmt.Errorf("bundle has no target platform")
	}
	if _, err := platform.Parse(m.Target); err != nil {
		return fmt.Errorf("invalid bundle target %q: %w", m.Target, err)
	}
	if m.Lists == nil {
		m.Lists = map[string]ManifestList{}
	}
	for name := range m.Lists {
		if !list.ValidName(name) {
			return fmt.Errorf("invalid list name %q", name)
		}
	}
	seen := map[string]bool{}
	binOwners := map[string]string{}
	for i := range m.Packages {
		p := &m.Packages[i]
		if !ValidName(p.Name) {
			return fmt.Errorf("package %d has invalid name %q", i, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate package %q", p.Name)
		}
		seen[p.Name] = true
		if p.Version == "" || p.Asset == "" || p.Tag == "" {
			return fmt.Errorf("package %s has no tag, version or asset", p.Name)
		}
		if strings.ContainsAny(p.Asset, `/\`) {
			return fmt.Errorf("package %s has an invalid asset name %q", p.Name, p.Asset)
		}
		if p.List != "" && !list.ValidName(p.List) {
			return fmt.Errorf("package %s has an invalid list name %q", p.Name, p.List)
		}
		if p.DownloadSHA256 != "" && !validSHA256(p.DownloadSHA256) {
			return fmt.Errorf("package %s has an invalid download_sha256", p.Name)
		}
		switch p.ChecksumStatus {
		case "", "verified", "tofu", "none", "forced":
		default:
			return fmt.Errorf("package %s has an unknown checksum_status %q", p.Name, p.ChecksumStatus)
		}
		if len(p.Bins) == 0 {
			return fmt.Errorf("package %s has no binaries", p.Name)
		}
		for _, bin := range p.Bins {
			if bin == "" || bin == "." || bin == ".." || strings.ContainsAny(bin, `/\`) {
				return fmt.Errorf("package %s has an invalid binary name %q", p.Name, bin)
			}
			if strings.EqualFold(bin, "cpt") || strings.EqualFold(bin, "cpt.exe") {
				return fmt.Errorf("package %s cannot provide the cpt binary itself", p.Name)
			}
			key := strings.ToLower(bin)
			if prev, ok := binOwners[key]; ok && prev != p.Name {
				return fmt.Errorf("packages %s and %s both provide %s", prev, p.Name, bin)
			}
			binOwners[key] = p.Name
		}
		sort.Strings(p.Bins)
	}
	return nil
}

func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// ValidName reports whether s is safe as a bundle identifier and file prefix.
func ValidName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.Contains(s, "..") && bundleNameRE.MatchString(s)
}

// FileName is the manifest file name for a bundle.
func FileName(name string) string { return name + ".json" }

// OutputName derives the self-extracting executable name for a manifest.
func (m Manifest) OutputName() string {
	name := m.Name + "-" + strings.ReplaceAll(m.Target, "/", "-")
	if strings.HasPrefix(m.Target, "windows/") {
		name += ".exe"
	}
	return name
}

// OutputPath resolves the executable path, honouring a custom --output.
func (m Manifest) OutputPath(bundleDir string) string {
	if strings.TrimSpace(m.Output) == "" {
		return filepath.Join(bundleDir, m.OutputName())
	}
	if filepath.IsAbs(m.Output) {
		return m.Output
	}
	return filepath.Join(bundleDir, m.Output)
}

// LoadManifest reads and validates a bundle manifest.
func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("invalid %s: %w", path, err)
	}
	return m, nil
}

// Save writes the manifest atomically.
func (m Manifest) Save(path string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data, 0o644)
}
