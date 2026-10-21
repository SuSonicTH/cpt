// Package state persists what cpt has installed and where it came from.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/SuSonicTH/cpt/internal/fsutil"
)

// CurrentSchema is the state schema version understood by this build.
const CurrentSchema = 1

// List records the provenance of one package list.
type List struct {
	Origin    string    `json:"origin,omitempty"`
	ETag      string    `json:"etag,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitempty"`
	Builtin   bool      `json:"builtin,omitempty"`
	Readonly  bool      `json:"readonly,omitempty"`
	Packages  int       `json:"packages,omitempty"`
}

// Package records one installed package.
type Package struct {
	Tag            string            `json:"tag"`
	Version        string            `json:"version"`
	List           string            `json:"list"`
	SourceID       string            `json:"source_id"`
	Asset          string            `json:"asset"`
	DownloadSHA256 string            `json:"download_sha256"`
	ChecksumStatus string            `json:"checksum_status"`
	ChecksumSource string            `json:"checksum_source,omitempty"`
	Bins           map[string]string `json:"bins"`
	Pinned         string            `json:"pinned,omitempty"`
	Origin         string            `json:"origin,omitempty"`
	InstalledAt    time.Time         `json:"installed_at"`
}

// Shell records approval state for a shell snippet. Reserved for M2.
type Shell struct {
	SnippetSHA256 string `json:"snippet_sha256,omitempty"`
	Approved      bool   `json:"approved,omitempty"`
}

// State is the contents of ~/.cpt/state.json.
type State struct {
	Schema    int                 `json:"schema"`
	Generated time.Time           `json:"generated"`
	Packages  map[string]*Package `json:"packages"`
	Provides  map[string]string   `json:"provides"`
	Lists     map[string]*List    `json:"lists"`
	Shell     map[string]*Shell   `json:"shell"`
}

// New returns an empty state.
func New() *State {
	return &State{
		Schema:   CurrentSchema,
		Packages: map[string]*Package{},
		Provides: map[string]string{},
		Lists:    map[string]*List{},
		Shell:    map[string]*Shell{},
	}
}

// Load reads state from path. A missing file yields empty state.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(), nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	st := New()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if st.Schema > CurrentSchema {
		return nil, fmt.Errorf("state schema %d is newer than supported (%d)", st.Schema, CurrentSchema)
	}
	st.ensureMaps()
	return st, nil
}

func (s *State) ensureMaps() {
	if s.Packages == nil {
		s.Packages = map[string]*Package{}
	}
	if s.Provides == nil {
		s.Provides = map[string]string{}
	}
	if s.Lists == nil {
		s.Lists = map[string]*List{}
	}
	if s.Shell == nil {
		s.Shell = map[string]*Shell{}
	}
}

// Save writes state atomically.
func (s *State) Save(path string) error {
	s.ensureMaps()
	s.Schema = CurrentSchema
	s.Generated = time.Now().UTC()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsutil.WriteFileAtomic(path, data, 0o600)
}

// Owner returns the package that provides bin, if any.
func (s *State) Owner(bin string) (string, bool) {
	pkg, ok := s.Provides[bin]
	return pkg, ok
}

// Remove deletes a package and its binary ownership records.
func (s *State) Remove(name string) {
	if p, ok := s.Packages[name]; ok {
		for bin := range p.Bins {
			if s.Provides[bin] == name {
				delete(s.Provides, bin)
			}
		}
	}
	delete(s.Packages, name)
}
