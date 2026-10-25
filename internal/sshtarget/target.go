// Package sshtarget stores saved SSH destinations and builds the commands that
// copy a bundle to a remote host and start a session-scoped cpt environment.
package sshtarget

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/SuSonicTH/cpt/internal/bundle"
	"github.com/SuSonicTH/cpt/internal/fsutil"
)

// CurrentSchema is the ssh.json schema version understood by this build.
const CurrentSchema = 1

// CacheTTL is how long a remote bundle copy is considered fresh.
const CacheTTL = 24 * time.Hour

// SSH source kinds.
const (
	SourceConfig = "config"
	SourceCustom = "custom"
)

// reservedNames are cpt ssh subcommand names. A target with one of these names
// could never be reached through the documented `cpt ssh <name>` short form, so
// they are rejected when the target is saved.
var reservedNames = map[string]bool{
	"list": true, "ls": true,
	"add": true, "create": true,
	"edit": true, "update": true,
	"remove": true, "rm": true, "delete": true,
	"connect": true, "help": true,
}

// Remote shell choices. Auto asks the remote host.
const (
	ShellAuto = "auto"
	ShellBash = "bash"
	ShellZsh  = "zsh"
	ShellSh   = "sh"
)

// SSH describes either an OpenSSH config alias or an explicit destination.
type SSH struct {
	Source       string `json:"source"`
	Host         string `json:"host"`
	User         string `json:"user,omitempty"`
	Port         int    `json:"port,omitempty"`
	IdentityFile string `json:"identity_file,omitempty"`
	ProxyJump    string `json:"proxy_jump,omitempty"`
}

// Target pairs one bundle with one SSH destination.
type Target struct {
	Name          string    `json:"name"`
	Bundle        string    `json:"bundle"`
	SSH           SSH       `json:"ssh"`
	RemoteRoot    string    `json:"remote_root,omitempty"`
	Shell         string    `json:"shell,omitempty"`
	CleanupOnExit bool      `json:"cleanup_on_exit,omitempty"`
	Force         bool      `json:"force,omitempty"`
	Created       time.Time `json:"created,omitzero"`
	Updated       time.Time `json:"updated,omitzero"`
	LastConnected time.Time `json:"last_connected,omitzero"`
	LastCopied    time.Time `json:"last_copied,omitzero"`
	CopiedSHA256  string    `json:"copied_sha256,omitempty"`
	CopiedKey     string    `json:"copied_key,omitempty"`
}

// File is the on-disk ~/.cpt/ssh.json file.
type File struct {
	Schema  int                `json:"schema"`
	Targets map[string]*Target `json:"targets"`
}

// NewFile returns an empty target file.
func NewFile() *File {
	return &File{Schema: CurrentSchema, Targets: map[string]*Target{}}
}

// Load reads ssh.json. A missing file yields an empty target set.
func Load(file string) (*File, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return NewFile(), nil
		}
		return nil, fmt.Errorf("read ssh targets: %w", err)
	}
	f := NewFile()
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("parse ssh targets: %w", err)
	}
	if f.Schema <= 0 {
		f.Schema = CurrentSchema
	}
	if f.Schema > CurrentSchema {
		return nil, fmt.Errorf("ssh target schema %d is newer than supported (%d)", f.Schema, CurrentSchema)
	}
	if f.Targets == nil {
		f.Targets = map[string]*Target{}
	}
	for name, t := range f.Targets {
		if t == nil {
			delete(f.Targets, name)
			continue
		}
		if t.Name == "" {
			t.Name = name
		}
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("invalid SSH target %q: %w", name, err)
		}
		if t.Name != name {
			return nil, fmt.Errorf("SSH target key %q does not match name %q", name, t.Name)
		}
	}
	return f, nil
}

// Save writes ssh.json atomically.
func (f *File) Save(file string) error {
	if f.Schema <= 0 {
		f.Schema = CurrentSchema
	}
	if f.Targets == nil {
		f.Targets = map[string]*Target{}
	}
	for name, t := range f.Targets {
		if t == nil {
			return fmt.Errorf("SSH target %q is nil", name)
		}
		if t.Name == "" {
			t.Name = name
		}
		if err := t.Validate(); err != nil {
			return fmt.Errorf("invalid SSH target %q: %w", name, err)
		}
		if t.Name != name {
			return fmt.Errorf("SSH target key %q does not match name %q", name, t.Name)
		}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsutil.WriteFileAtomic(file, data, 0o600)
}

// Get returns one target.
func (f *File) Get(name string) (*Target, error) {
	t := f.Targets[name]
	if t == nil {
		return nil, fmt.Errorf("unknown SSH target %q", name)
	}
	return t, nil
}

// IsReservedName reports whether name is a cpt ssh subcommand and therefore
// cannot be used as a target name.
func IsReservedName(name string) bool { return reservedNames[name] }

// Put inserts or replaces a target. Reserved names are rejected here rather
// than in Validate, so an ssh.json written by an older build still loads.
func (f *File) Put(t *Target) error {
	if t == nil {
		return fmt.Errorf("nil SSH target")
	}
	if err := t.Validate(); err != nil {
		return err
	}
	if IsReservedName(t.Name) {
		return fmt.Errorf("target name %q is reserved by a cpt ssh subcommand", t.Name)
	}
	f.Targets[t.Name] = t
	return nil
}

// Remove deletes one target and returns it.
func (f *File) Remove(name string) (*Target, error) {
	t, err := f.Get(name)
	if err != nil {
		return nil, err
	}
	delete(f.Targets, name)
	return t, nil
}

// EffectiveShell returns the configured shell, defaulting to auto.
func (t *Target) EffectiveShell() string {
	if t == nil || strings.TrimSpace(t.Shell) == "" {
		return ShellAuto
	}
	return t.Shell
}

// EffectiveRemoteRoot returns the remote root, or "~/.cpt" for the default.
func (t *Target) EffectiveRemoteRoot() string {
	if t == nil || t.RemoteRoot == "" {
		return DefaultRemoteDirectory
	}
	return t.RemoteRoot
}

// CopyKey identifies the settings that affect the uploaded artifact. It is
// stored alongside the last copy so changing a host or the remote root forces a
// fresh upload even when the bundle hash is unchanged.
func (t *Target) CopyKey(bundleSHA string) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			fmt.Fprintf(h, "%d:%s\n", len(p), p)
		}
	}
	write(
		t.Name,
		t.Bundle,
		bundleSHA,
		t.SSH.Source,
		t.SSH.Host,
		t.SSH.User,
		fmt.Sprintf("%d", t.SSH.Port),
		t.SSH.IdentityFile,
		t.SSH.ProxyJump,
		t.RemoteRoot,
		fmt.Sprintf("%t", t.Force),
	)
	return hex.EncodeToString(h.Sum(nil))
}

// CacheFresh reports whether the saved remote copy can be reused.
func (t *Target) CacheFresh(now time.Time, bundleSHA string) bool {
	if t == nil || t.CleanupOnExit || t.LastCopied.IsZero() {
		return false
	}
	if now.Sub(t.LastCopied) >= CacheTTL {
		return false
	}
	if !strings.EqualFold(t.CopiedSHA256, bundleSHA) {
		return false
	}
	return t.CopiedKey == t.CopyKey(bundleSHA)
}

// Validate checks every field used to build local and remote commands. It also
// normalises RemoteRoot and Shell in place; Load, Put, and Save call it so the
// stored file is always canonical.
func (t *Target) Validate() error {
	if t == nil {
		return fmt.Errorf("empty target")
	}
	if !bundle.ValidName(t.Name) {
		return fmt.Errorf("invalid target name %q", t.Name)
	}
	if !bundle.ValidName(t.Bundle) {
		return fmt.Errorf("invalid bundle name %q", t.Bundle)
	}
	switch t.SSH.Source {
	case SourceConfig, SourceCustom:
	default:
		return fmt.Errorf("ssh.source must be %q or %q", SourceConfig, SourceCustom)
	}
	if err := validHost(t.SSH.Host); err != nil {
		return err
	}
	if t.SSH.Source == SourceConfig && strings.ContainsAny(t.SSH.Host, "*?!:[]") {
		return fmt.Errorf("ssh.host must be a concrete ssh config alias")
	}
	if err := validUser(t.SSH.User); err != nil {
		return err
	}
	if t.SSH.Port < 0 || t.SSH.Port > 65535 {
		return fmt.Errorf("ssh port %d is out of range", t.SSH.Port)
	}
	for label, value := range map[string]string{
		"identity_file": t.SSH.IdentityFile,
		"proxy_jump":    t.SSH.ProxyJump,
	} {
		if hasControl(value) {
			return fmt.Errorf("ssh.%s contains a control character", label)
		}
	}
	if t.SSH.IdentityFile != "" && strings.HasPrefix(t.SSH.IdentityFile, "-") {
		return fmt.Errorf("ssh.identity_file must not start with '-'")
	}
	if t.SSH.ProxyJump != "" && strings.HasPrefix(t.SSH.ProxyJump, "-") {
		return fmt.Errorf("ssh.proxy_jump must not start with '-'")
	}
	if t.RemoteRoot != "" {
		if hasControl(t.RemoteRoot) {
			return fmt.Errorf("remote_root contains a control character")
		}
		if !strings.HasPrefix(t.RemoteRoot, "/") {
			return fmt.Errorf("remote_root must be an absolute POSIX path")
		}
		if strings.Contains(t.RemoteRoot, "/../") || strings.HasSuffix(t.RemoteRoot, "/..") {
			return fmt.Errorf("remote_root must not contain '..'")
		}
		clean := path.Clean(t.RemoteRoot)
		if clean == "/" {
			return fmt.Errorf("remote_root must not be the filesystem root")
		}
		if t.CleanupOnExit && strings.Count(strings.Trim(clean, "/"), "/") < 1 {
			return fmt.Errorf("cleanup requires a nested remote_root such as /srv/cpt")
		}
		t.RemoteRoot = clean
	}
	switch t.EffectiveShell() {
	case ShellAuto, ShellBash, ShellZsh, ShellSh:
	default:
		return fmt.Errorf("shell must be auto, bash, zsh or sh")
	}
	if t.EffectiveShell() != ShellAuto {
		t.Shell = t.EffectiveShell()
	}
	return nil
}

func validHost(host string) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("ssh.host is required")
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("ssh.host must not start with '-'")
	}
	if hasControl(host) || strings.ContainsAny(host, " \t/@\\") {
		return fmt.Errorf("ssh.host contains an invalid character")
	}
	return nil
}

func validUser(user string) error {
	if user == "" {
		return nil
	}
	if strings.HasPrefix(user, "-") {
		return fmt.Errorf("ssh.user must not start with '-'")
	}
	if hasControl(user) || strings.ContainsAny(user, " \t/@:") {
		return fmt.Errorf("ssh.user contains an invalid character")
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
