package shellinit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SuSonicTH/cpt/internal/list"
)

func TestResolveInlineDeterministic(t *testing.T) {
	sh := &list.Shell{Bash: &list.ShellFlavor{
		Functions: "f() { :; }\n",
		Env:       map[string]string{"B": "2", "A": "a b"},
		Path:      []string{"/opt/tool/bin"},
	}}
	r := &Resolver{}
	bodies, err := r.Resolve(context.Background(), sh)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, want := range []string{
		"export A='a b'\n",
		"export B='2'\n",
		"case \":$PATH:\" in *:'/opt/tool/bin':*)",
		"f() { :; }\n",
	} {
		if !strings.Contains(bodies.Bash, want) {
			t.Errorf("body is missing %q:\n%s", want, bodies.Bash)
		}
	}
	if strings.Index(bodies.Bash, "export A") > strings.Index(bodies.Bash, "export B") {
		t.Fatal("environment variables are not sorted")
	}
	firstHash := bodies.Hash()
	again, err := r.Resolve(context.Background(), sh)
	if err != nil || again.Hash() != firstHash {
		t.Fatalf("second resolve changed the hash: %v", err)
	}
}

func TestResolvePwsh(t *testing.T) {
	sh := &list.Shell{Pwsh: &list.ShellFlavor{
		Functions: "function f { }",
		Env:       map[string]string{"A": "it's"},
		Path:      []string{`C:\Tools`},
	}}
	bodies, err := (&Resolver{}).Resolve(context.Background(), sh)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(bodies.Pwsh, "$env:A = 'it''s'") {
		t.Fatalf("unexpected env quoting:\n%s", bodies.Pwsh)
	}
	if !strings.Contains(bodies.Pwsh, "-notcontains 'C:\\Tools'") {
		t.Fatalf("unexpected path rendering:\n%s", bodies.Pwsh)
	}
	if bodies.Bash != "" {
		t.Fatal("bash body was fabricated for a pwsh-only snippet")
	}
}

func TestResolveURLChecksumAndCache(t *testing.T) {
	body := []byte("alias x='y'\n")
	sum := sha256.Sum256(body)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(body)
	}))

	r := &Resolver{Client: srv.Client(), Attempts: 1, CacheDir: t.TempDir()}
	sh := &list.Shell{Bash: &list.ShellFlavor{FunctionsURL: srv.URL, FunctionsSHA256: hex.EncodeToString(sum[:])}}
	bodies, err := r.Resolve(context.Background(), sh)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(bodies.Bash, "alias x='y'") {
		t.Fatalf("unexpected body:\n%s", bodies.Bash)
	}
	if hits.Load() != 1 {
		t.Fatalf("requests = %d, want 1", hits.Load())
	}

	// A second resolve is served from the content-addressed cache, even offline.
	srv.Close()
	r.Client = srv.Client()
	r.Offline = true
	if _, err := r.Resolve(context.Background(), sh); err != nil {
		t.Fatalf("cached resolve: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("cache miss caused another request: %d", hits.Load())
	}
}

func TestResolveURLRejectsBadChecksum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("body"))
	}))
	defer srv.Close()

	r := &Resolver{Client: srv.Client(), Attempts: 1, CacheDir: t.TempDir()}
	sh := &list.Shell{Bash: &list.ShellFlavor{FunctionsURL: srv.URL, FunctionsSHA256: strings.Repeat("0", 64)}}
	if _, err := r.Resolve(context.Background(), sh); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected a checksum mismatch, got %v", err)
	}
}

func TestResolveURLOfflineWithoutCache(t *testing.T) {
	sh := &list.Shell{Bash: &list.ShellFlavor{
		FunctionsURL:    "https://example.invalid/snippet.sh",
		FunctionsSHA256: strings.Repeat("a", 64),
	}}
	r := &Resolver{CacheDir: t.TempDir(), Offline: true}
	if _, err := r.Resolve(context.Background(), sh); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("expected an offline error, got %v", err)
	}
}

func TestResolveEmpty(t *testing.T) {
	if _, err := (&Resolver{}).Resolve(context.Background(), &list.Shell{}); err == nil {
		t.Fatal("expected an error for an empty shell block")
	}
}

func TestBodiesHashSeparatesFlavors(t *testing.T) {
	a := Bodies{Bash: "x", Pwsh: ""}
	b := Bodies{Bash: "", Pwsh: "x"}
	if a.Hash() == b.Hash() {
		t.Fatal("flavor bodies hash to the same value")
	}
}

func TestBodiesHashIsUnambiguous(t *testing.T) {
	a := Bodies{Bash: "a", Pwsh: "\x00pwsh\x00b"}
	b := Bodies{Bash: "a\x00pwsh\x00", Pwsh: "b"}
	if a.Hash() == b.Hash() {
		t.Fatal("length prefix did not separate the bodies")
	}
}

func TestResolveRejectsInvalidURLContent(t *testing.T) {
	cases := map[string][]byte{
		"invalid utf8": {0xff, 0xfe, 0x00},
		"nul byte":     []byte("a\x00b"),
		"escape":       []byte("a\x1bb"),
		"init marker":  []byte("# >>> cpt:init\n"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			sum := sha256.Sum256(body)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write(body)
			}))
			defer srv.Close()
			r := &Resolver{Client: srv.Client(), Attempts: 1, CacheDir: t.TempDir()}
			sh := &list.Shell{Bash: &list.ShellFlavor{
				FunctionsURL:    srv.URL,
				FunctionsSHA256: hex.EncodeToString(sum[:]),
			}}
			if _, err := r.Resolve(context.Background(), sh); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestResolveIsBestEffortWhenCacheWriteFails(t *testing.T) {
	body := []byte("alias x=y\n")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	cacheFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cacheFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var warned error
	r := &Resolver{
		Client: srv.Client(), Attempts: 1, CacheDir: cacheFile,
		Warn: func(err error) { warned = err },
	}
	sh := &list.Shell{Bash: &list.ShellFlavor{
		FunctionsURL:    srv.URL,
		FunctionsSHA256: hex.EncodeToString(sum[:]),
	}}
	bodies, err := r.Resolve(context.Background(), sh)
	if err != nil {
		t.Fatalf("verified snippet was discarded: %v", err)
	}
	if !strings.Contains(bodies.Bash, "alias x=y") {
		t.Fatalf("unexpected body: %q", bodies.Bash)
	}
	if warned == nil || !errors.Is(warned, os.ErrExist) && !strings.Contains(warned.Error(), "cache") {
		t.Fatalf("cache failure was not reported: %v", warned)
	}
}
