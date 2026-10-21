package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeFollowsRepoRename(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/old/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new/repo/releases/latest", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/new/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new/repo/releases/tag/v1.2.3", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &Prober{Client: srv.Client(), BaseURL: srv.URL}
	got, err := p.Latest(context.Background(), "old/repo")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got.Tag != "v1.2.3" || got.Canonical != "new/repo" {
		t.Fatalf("unexpected probe: %+v", got)
	}
}

func TestProbeNoReleases(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	p := &Prober{Client: srv.Client(), BaseURL: srv.URL}
	_, err := p.Latest(context.Background(), "acme/tool")
	if err == nil {
		t.Fatal("expected an error for a missing release")
	}
}

func TestProbeCacheAndOffline(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/acme/tool/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/acme/tool/releases/tag/v9.9.9", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	dir := t.TempDir()

	p := &Prober{Client: srv.Client(), BaseURL: srv.URL, CacheDir: dir, TTL: time.Hour}
	first, err := p.Latest(context.Background(), "acme/tool")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	srv.Close()

	offline := &Prober{Client: srv.Client(), BaseURL: srv.URL, CacheDir: dir, TTL: time.Hour, Offline: true}
	got, err := offline.Latest(context.Background(), "acme/tool")
	if err != nil {
		t.Fatalf("cached Latest: %v", err)
	}
	if got.Tag != first.Tag || got.Tag != "v9.9.9" {
		t.Fatalf("unexpected cached tag %q", got.Tag)
	}
}

func TestReleaseURLs(t *testing.T) {
	p := &Prober{BaseURL: "https://github.com"}
	if got := p.ReleaseURL("acme/tool", "v1.0.0", "tool.tar.gz"); got != "https://github.com/acme/tool/releases/download/v1.0.0/tool.tar.gz" {
		t.Fatalf("ReleaseURL = %q", got)
	}
	if got := p.LatestAssetURL("acme/tool", "tool.tar.gz"); got != "https://github.com/acme/tool/releases/latest/download/tool.tar.gz" {
		t.Fatalf("LatestAssetURL = %q", got)
	}
}
