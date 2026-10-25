package sshtarget

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ConfigHost is one usable Host alias from ~/.ssh/config.
type ConfigHost struct {
	Alias string `json:"alias"`
}

// ResolvedHost is the effective destination reported by ssh -G.
type ResolvedHost struct {
	Alias        string `json:"alias"`
	HostName     string `json:"hostname,omitempty"`
	User         string `json:"user,omitempty"`
	Port         int    `json:"port,omitempty"`
	IdentityFile string `json:"identity_file,omitempty"`
	ProxyJump    string `json:"proxy_jump,omitempty"`
}

// DefaultConfigPath returns ~/.ssh/config.
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

// Hosts lists concrete Host aliases from the user's SSH config, including
// Include files. Wildcard and negated patterns are skipped because they are not
// destinations a user can select.
func Hosts(file string) ([]ConfigHost, error) {
	aliases := map[string]bool{}
	visited := map[string]bool{}
	if err := parseConfigFile(file, aliases, visited, 0); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]ConfigHost, 0, len(aliases))
	for alias := range aliases {
		out = append(out, ConfigHost{Alias: alias})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}

func parseConfigFile(file string, aliases, visited map[string]bool, depth int) error {
	if depth > 16 {
		return fmt.Errorf("ssh config Include nesting is too deep")
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		abs = file
	}
	if visited[abs] {
		return nil
	}
	visited[abs] = true

	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// SSH config lines are normally short, but do not fail on a long comment.
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(stripComment(sc.Text()))
		if line == "" {
			continue
		}
		key, value := splitDirective(line)
		switch strings.ToLower(key) {
		case "host":
			for _, alias := range configFields(value) {
				if alias == "" || strings.ContainsAny(alias, "*?!\t:[]") || strings.HasPrefix(alias, "-") || hasControl(alias) || validHost(alias) != nil {
					continue
				}
				aliases[alias] = true
			}
		case "include":
			for _, pattern := range configFields(value) {
				path := expandHome(pattern)
				if !filepath.IsAbs(path) {
					path = filepath.Join(filepath.Dir(file), path)
				}
				matches, globErr := filepath.Glob(path)
				if globErr != nil {
					return fmt.Errorf("ssh config include %q: %w", pattern, globErr)
				}
				for _, match := range matches {
					if err := parseConfigFile(match, aliases, visited, depth+1); err != nil && !os.IsNotExist(err) {
						return err
					}
				}
			}
		}
	}
	return sc.Err()
}

func splitDirective(line string) (string, string) {
	if i := strings.IndexAny(line, " \t="); i >= 0 {
		key := strings.TrimSpace(line[:i])
		value := strings.TrimSpace(strings.TrimLeft(line[i:], " \t="))
		return key, value
	}
	return line, ""
}

// stripComment removes an unquoted # comment.
func stripComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#':
			return line[:i]
		}
	}
	return line
}

func configFields(value string) []string {
	var fields []string
	var b strings.Builder
	var quote rune
	flush := func() {
		if b.Len() > 0 {
			fields = append(fields, b.String())
			b.Reset()
		}
	}
	for _, r := range value {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return fields
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

// Resolve asks OpenSSH for the effective values of an alias. It never connects
// to the host; ssh -G only expands configuration.
func Resolve(ctx context.Context, alias string) (ResolvedHost, error) {
	if strings.TrimSpace(alias) == "" || strings.HasPrefix(alias, "-") || strings.ContainsAny(alias, " \t\r\n\x00") {
		return ResolvedHost{}, fmt.Errorf("invalid ssh config host %q", alias)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "ssh", "-G", alias)
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return ResolvedHost{}, fmt.Errorf("ssh -G %s failed", alias)
		}
		return ResolvedHost{}, fmt.Errorf("resolve ssh host %s: %w", alias, err)
	}
	res := ResolvedHost{Alias: alias}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.ToLower(fields[0])
		value := strings.Join(fields[1:], " ")
		switch key {
		case "hostname":
			if res.HostName == "" {
				res.HostName = value
			}
		case "user":
			if res.User == "" {
				res.User = value
			}
		case "port":
			if res.Port == 0 {
				if p, err := strconv.Atoi(value); err == nil {
					res.Port = p
				}
			}
		case "identityfile":
			if res.IdentityFile == "" {
				res.IdentityFile = value
			}
		case "proxyjump":
			if res.ProxyJump == "" {
				res.ProxyJump = value
			}
		}
	}
	return res, nil
}

// Destination is the user@host token passed to ssh and scp.
func (t Target) Destination() string {
	host := t.SSH.Host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	if t.SSH.User != "" {
		return t.SSH.User + "@" + host
	}
	return host
}
