package shellinit

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/SuSonicTH/cpt/internal/fsutil"
	"github.com/SuSonicTH/cpt/internal/httpc"
	"github.com/SuSonicTH/cpt/internal/list"
)

// MaxSnippetSize caps a URL-referenced snippet body.
const MaxSnippetSize = 1 << 20

var hexRE = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

// Bodies is one package's rendered snippet bodies per dialect.
type Bodies struct {
	Bash string
	Pwsh string
}

// Hash returns the stable digest used in state and marker comments. Length
// prefixes make the encoding unambiguous even if a body contains NUL bytes.
func (b Bodies) Hash() string {
	h := sha256.New()
	writePart(h, "bash", b.Bash)
	writePart(h, "pwsh", b.Pwsh)
	return hex.EncodeToString(h.Sum(nil))
}

func writePart(h interface{ Write([]byte) (int, error) }, label, body string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(body)))
	h.Write([]byte(label))
	h.Write(size[:])
	h.Write([]byte(body))
}

// HashBytes is the hex SHA256 of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Resolver turns a list shell block into concrete snippet bodies, fetching and
// checksum-verifying URL snippets as needed.
type Resolver struct {
	Client   *http.Client
	Attempts int
	CacheDir string
	Offline  bool
	// Warn reports non-fatal problems such as a failed best-effort cache write.
	Warn func(error)
}

// Resolve renders every declared flavor of a shell block.
func (r *Resolver) Resolve(ctx context.Context, sh *list.Shell) (Bodies, error) {
	if sh == nil {
		return Bodies{}, fmt.Errorf("no shell block")
	}
	var out Bodies
	if sh.Bash != nil {
		body, err := r.flavor(ctx, sh.Bash, FlavorBash)
		if err != nil {
			return Bodies{}, fmt.Errorf("bash: %w", err)
		}
		out.Bash = body
	}
	if sh.Pwsh != nil {
		body, err := r.flavor(ctx, sh.Pwsh, FlavorPwsh)
		if err != nil {
			return Bodies{}, fmt.Errorf("pwsh: %w", err)
		}
		out.Pwsh = body
	}
	if out.Bash == "" && out.Pwsh == "" {
		return Bodies{}, fmt.Errorf("shell snippet is empty")
	}
	return out, nil
}

func (r *Resolver) flavor(ctx context.Context, f *list.ShellFlavor, flavor Flavor) (string, error) {
	functions := f.Functions
	if f.FunctionsURL != "" {
		data, err := r.fetchSnippet(ctx, f.FunctionsURL, f.FunctionsSHA256)
		if err != nil {
			return "", err
		}
		functions = string(data)
	}
	if err := list.ValidateFunctions(functions); err != nil {
		return "", err
	}
	return renderBody(flavor, f, functions), nil
}

func (r *Resolver) fetchSnippet(ctx context.Context, url, want string) ([]byte, error) {
	want = strings.ToLower(strings.TrimSpace(want))
	if !hexRE.MatchString(want) {
		return nil, fmt.Errorf("invalid functions_sha256")
	}
	cachePath := ""
	if r.CacheDir != "" {
		cachePath = filepath.Join(r.CacheDir, want)
		if data, err := os.ReadFile(cachePath); err == nil {
			if HashBytes(data) == want {
				return data, nil
			}
			// A corrupted cache entry is refetched when the network is available.
			_ = os.Remove(cachePath)
		}
	}
	if r.Offline {
		return nil, fmt.Errorf("offline: snippet %s is not cached", url)
	}
	if r.Client == nil {
		return nil, fmt.Errorf("fetch snippet %s: no HTTP client", url)
	}
	data, _, err := httpc.Get(ctx, r.Client, url, MaxSnippetSize, nil, max(1, r.Attempts))
	if err != nil {
		return nil, fmt.Errorf("fetch snippet %s: %w", url, err)
	}
	got := HashBytes(data)
	if got != want {
		return nil, fmt.Errorf("snippet %s: sha256 mismatch: expected %s, got %s", url, want, got)
	}
	if cachePath != "" {
		if err := os.MkdirAll(r.CacheDir, 0o755); err != nil {
			r.warn(fmt.Errorf("create snippet cache: %w", err))
		} else if err := fsutil.WriteFileAtomic(cachePath, data, 0o644); err != nil {
			r.warn(err)
		}
	}
	return data, nil
}

func (r *Resolver) warn(err error) {
	if r.Warn != nil {
		r.Warn(err)
	}
}

func renderBody(flavor Flavor, f *list.ShellFlavor, functions string) string {
	env := renderBashEnv(f)
	if flavor == FlavorPwsh {
		env = renderPwshEnv(f)
	}
	env = strings.TrimRight(env, "\n")
	var parts []string
	if env != "" {
		parts = append(parts, env)
	}
	if fn := normalizeFunctions(functions); fn != "" {
		parts = append(parts, fn)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func renderBashEnv(f *list.ShellFlavor) string {
	var b strings.Builder
	keys := make([]string, 0, len(f.Env))
	for key := range f.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "export %s=%s\n", key, posixQuote(f.Env[key]))
	}
	for _, dir := range f.Path {
		q := posixQuote(dir)
		fmt.Fprintf(&b, "case \":$PATH:\" in *:%s:*) ;; *) PATH=%s:\"$PATH\" ;; esac\n", q, q)
		b.WriteString("export PATH\n")
	}
	return b.String()
}

func renderPwshEnv(f *list.ShellFlavor) string {
	var b strings.Builder
	keys := make([]string, 0, len(f.Env))
	for key := range f.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "$env:%s = %s\n", key, pwshQuote(f.Env[key]))
	}
	for _, dir := range f.Path {
		q := pwshQuote(dir)
		fmt.Fprintf(&b, "if (($env:PATH -split [IO.Path]::PathSeparator) -notcontains %s) { $env:PATH = %s + [IO.Path]::PathSeparator + $env:PATH }\n", q, q)
	}
	return b.String()
}

func normalizeFunctions(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimRight(s, "\n")
}
