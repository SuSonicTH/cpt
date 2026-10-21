package resolve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SuSonicTH/cpt/internal/list"
	"github.com/SuSonicTH/cpt/internal/platform"
	"github.com/SuSonicTH/cpt/internal/source"
)

func probeServer(t *testing.T, tag string) *source.Prober {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.Redirect(w, r, strings.TrimSuffix(r.URL.Path, "/latest")+"/tag/"+tag, http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &source.Prober{Client: srv.Client(), BaseURL: srv.URL}
}

func testEntry(name, repo string) list.Entry {
	return list.Entry{
		List: "core",
		Pkg: &list.Package{
			Name: name,
			Source: list.Source{
				Type: "github",
				Repo: repo,
				Bins: []string{name},
				Asset: map[string]string{
					"linux/amd64": "tool-{tag}-linux-amd64.tar.gz",
				},
			},
		},
	}
}

func TestResolveLatest(t *testing.T) {
	prober := probeServer(t, "v1.2.3")
	entry := testEntry("tool", "acme/tool")
	res, err := Package(context.Background(), prober, entry, platform.Platform{OS: "linux", Arch: "amd64"}, Options{})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if res.Version != "1.2.3" || res.Tag != "v1.2.3" {
		t.Fatalf("unexpected version/tag: %+v", res)
	}
	if res.Asset != "tool-v1.2.3-linux-amd64.tar.gz" {
		t.Fatalf("asset = %q", res.Asset)
	}
	if !strings.HasSuffix(res.URL, "/releases/download/v1.2.3/tool-v1.2.3-linux-amd64.tar.gz") {
		t.Fatalf("url = %q", res.URL)
	}
	if res.Format != "tar.gz" {
		t.Fatalf("format = %q", res.Format)
	}
	if len(res.Checksums) == 0 || res.Checksums[0].Name != "tool-v1.2.3-linux-amd64.tar.gz.sha256" {
		t.Fatalf("unexpected checksum candidates: %+v", res.Checksums[0])
	}
}

func TestResolvePinnedVersion(t *testing.T) {
	prober := probeServer(t, "v2.0.0")
	entry := testEntry("tool", "acme/tool")
	res, err := Package(context.Background(), prober, entry, platform.Platform{OS: "linux", Arch: "amd64"}, Options{CLIVersion: "1.0.0"})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if res.Tag != "v1.0.0" || res.Version != "1.0.0" {
		t.Fatalf("unexpected version/tag: %+v", res)
	}
	if !strings.Contains(res.Asset, "v1.0.0") {
		t.Fatalf("asset = %q", res.Asset)
	}
}

func TestResolveJQPinnedTag(t *testing.T) {
	prober := probeServer(t, "jq-1.8.2")
	entry := list.Entry{List: "core", Pkg: &list.Package{
		Name: "jq",
		Source: list.Source{
			Type:         "github",
			Repo:         "jqlang/jq",
			VersionStrip: "^jq-",
			Bins:         []string{"jq"},
			Asset:        map[string]string{"linux/amd64": "jq-linux-amd64"},
		},
	}}
	res, err := Package(context.Background(), prober, entry, platform.Platform{OS: "linux", Arch: "amd64"}, Options{CLIVersion: "1.7.1"})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if res.Tag != "jq-1.7.1" || res.Version != "1.7.1" {
		t.Fatalf("unexpected version/tag: %+v", res)
	}
}

func TestResolveUnsupportedPlatform(t *testing.T) {
	prober := probeServer(t, "v1.0.0")
	entry := testEntry("tool", "acme/tool")
	_, err := Package(context.Background(), prober, entry, platform.Platform{OS: "windows", Arch: "arm64"}, Options{})
	if err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("expected unsupported platform error, got %v", err)
	}
}

func TestResolveURLSource(t *testing.T) {
	entry := list.Entry{List: "x", Pkg: &list.Package{
		Name:    "tool",
		Version: "1.0.0",
		Source: list.Source{
			Type: "url",
			URL:  "https://example.test/tool-{version}-{os}-{arch}.tar.gz",
			Bins: []string{"tool"},
		},
	}}
	res, err := Package(context.Background(), nil, entry, platform.Platform{OS: "linux", Arch: "amd64"}, Options{})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if res.URL != "https://example.test/tool-1.0.0-linux-amd64.tar.gz" {
		t.Fatalf("url = %q", res.URL)
	}
	if res.Asset != "tool-1.0.0-linux-amd64.tar.gz" {
		t.Fatalf("asset = %q", res.Asset)
	}
}

func TestVersionHelpers(t *testing.T) {
	if got := versionOf("v1.2.3", ""); got != "1.2.3" {
		t.Fatalf("versionOf = %q", got)
	}
	if got := versionOf("jq-1.8.2", "^jq-"); got != "1.8.2" {
		t.Fatalf("versionOf = %q", got)
	}
	tag, err := tagForVersion("v2.0.0", "", "1.0.0")
	if err != nil || tag != "v1.0.0" {
		t.Fatalf("tagForVersion = %q, %v", tag, err)
	}
}

func TestResolveReinstallUsesCachedTag(t *testing.T) {
	// A server that always fails proves no probe is attempted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	prober := &source.Prober{Client: srv.Client(), BaseURL: srv.URL, Attempts: 1}
	entry := testEntry("tool", "acme/tool")
	res, err := Package(context.Background(), prober, entry, platform.Platform{OS: "linux", Arch: "amd64"}, Options{
		CLIVersion:   "1.2.3",
		InstalledTag: "v1.2.3",
		InstalledVer: "1.2.3",
	})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if res.Tag != "v1.2.3" || res.Version != "1.2.3" {
		t.Fatalf("unexpected resolution: %+v", res)
	}
}
