// Package config loads and saves cpt's user configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SuSonicTH/cpt/internal/fsutil"
)

// CurrentSchema is the configuration schema version understood by this build.
const CurrentSchema = 1

// HTTP groups network settings.
type HTTP struct {
	TimeoutSeconds int    `json:"timeout_seconds"`
	Retries        int    `json:"retries"`
	UserAgent      string `json:"user_agent"`
}

// UI groups presentation settings.
type UI struct {
	Theme string `json:"theme"`
}

// Logs groups logging settings.
type Logs struct {
	Level string `json:"level"`
}

// Config is the contents of ~/.cpt/config.json.
type Config struct {
	Schema             int    `json:"schema"`
	BinDir             string `json:"bin_dir"`
	Parallel           int    `json:"parallel"`
	ListTTLHours       int    `json:"list_ttl_hours"`
	ProbeTTLHours      int    `json:"probe_ttl_hours"`
	ChecksumPolicy     string `json:"checksum_policy"`
	AllowShellPackages bool   `json:"allow_shell_packages"`
	HTTP               HTTP   `json:"http"`
	UI                 UI     `json:"ui"`
	Logs               Logs   `json:"logs"`
}

// Defaults returns the built-in configuration.
func Defaults() *Config {
	return &Config{
		Schema:             CurrentSchema,
		Parallel:           4,
		ListTTLHours:       24,
		ProbeTTLHours:      6,
		ChecksumPolicy:     "require-if-available",
		AllowShellPackages: true,
		HTTP: HTTP{
			TimeoutSeconds: 60,
			Retries:        3,
			UserAgent:      "cpt/dev (+https://github.com/SuSonicTH/cpt)",
		},
		UI:   UI{Theme: "auto"},
		Logs: Logs{Level: "info"},
	}
}

// Paths describes every location cpt uses under its root directory.
type Paths struct {
	Root       string
	Bin        string
	Lists      string
	Cache      string
	ProbeCache string
	Bundles    string
	Logs       string
	Tmp        string
	ConfigFile string
	StateFile  string
	LockFile   string
}

// DefaultRoot returns CPT_ROOT when set, otherwise ~/.cpt.
func DefaultRoot() (string, error) {
	if root := strings.TrimSpace(os.Getenv("CPT_ROOT")); root != "" {
		return filepath.Abs(root)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	return filepath.Join(home, ".cpt"), nil
}

// NewPaths builds the standard layout under root.
func NewPaths(root string) Paths {
	root, _ = filepath.Abs(root)
	return Paths{
		Root:       root,
		Bin:        filepath.Join(root, "bin"),
		Lists:      filepath.Join(root, "lists"),
		Cache:      filepath.Join(root, "cache"),
		ProbeCache: filepath.Join(root, "cache", "probe"),
		Bundles:    filepath.Join(root, "bundles"),
		Logs:       filepath.Join(root, "logs"),
		Tmp:        filepath.Join(root, "tmp"),
		ConfigFile: filepath.Join(root, "config.json"),
		StateFile:  filepath.Join(root, "state.json"),
		LockFile:   filepath.Join(root, ".lock"),
	}
}

// Ensure creates the directory tree.
func (p Paths) Ensure() error {
	for _, dir := range []string{p.Root, p.Bin, p.Lists, p.Cache, p.ProbeCache, p.Bundles, p.Logs, p.Tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// Load reads the configuration, returning defaults when the file is absent.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Defaults(), nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := Defaults()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks field ranges and enumerations.
func (c *Config) Validate() error {
	if c.Schema <= 0 {
		c.Schema = CurrentSchema
	}
	if c.Schema > CurrentSchema {
		return fmt.Errorf("config schema %d is newer than supported (%d)", c.Schema, CurrentSchema)
	}
	if c.Parallel <= 0 {
		c.Parallel = 1
	}
	if c.Parallel > 32 {
		c.Parallel = 32
	}
	if c.ListTTLHours < 0 {
		return fmt.Errorf("list_ttl_hours must not be negative")
	}
	if c.ProbeTTLHours < 0 {
		return fmt.Errorf("probe_ttl_hours must not be negative")
	}
	switch c.ChecksumPolicy {
	case "off", "prefer", "require-if-available", "require":
	default:
		return fmt.Errorf("unknown checksum_policy %q", c.ChecksumPolicy)
	}
	if c.HTTP.TimeoutSeconds <= 0 {
		c.HTTP.TimeoutSeconds = 60
	}
	if c.HTTP.Retries < 0 {
		c.HTTP.Retries = 0
	}
	if c.HTTP.UserAgent == "" {
		c.HTTP.UserAgent = Defaults().HTTP.UserAgent
	}
	return nil
}

// Save writes the configuration atomically.
func (c *Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsutil.WriteFileAtomic(path, data, 0o644)
}

// BinDir returns the configured binary directory, falling back to paths.Bin.
func (c *Config) BinDirFor(paths Paths) string {
	if c.BinDir == "" {
		return paths.Bin
	}
	if filepath.IsAbs(c.BinDir) {
		return c.BinDir
	}
	return filepath.Join(paths.Root, c.BinDir)
}

// ListTTL is the refresh interval for package lists.
func (c *Config) ListTTL() time.Duration { return time.Duration(c.ListTTLHours) * time.Hour }

// ProbeTTL is the cache lifetime for upstream version probes.
func (c *Config) ProbeTTL() time.Duration { return time.Duration(c.ProbeTTLHours) * time.Hour }
