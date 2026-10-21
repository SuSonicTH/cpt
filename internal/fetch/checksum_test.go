package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMatchChecksumFormats(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"gnu", hash + "  tool.tar.gz\n", true},
		{"gnu-star", hash + " *tool.tar.gz\n", true},
		{"bsd", "SHA256 (tool.tar.gz) = " + hash + "\n", true},
		{"bare", hash + "\n", true},
		{"other-file", hash + "  other.tar.gz\n", false},
		{"prefixed-path", hash + "  dist/tool.tar.gz\n", true},
		{"empty", "", false},
		{"short", "abc  tool.tar.gz\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := matchChecksum([]byte(tt.data), "tool.tar.gz")
			if ok != tt.want {
				t.Fatalf("ok = %v, want %v", ok, tt.want)
			}
			if ok && got != hash {
				t.Fatalf("hash = %q, want %q", got, hash)
			}
		})
	}
}

func TestDiscoverFallsThroughMissingCandidates(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			w.Write([]byte(hash + "  tool.tar.gz\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cands := []Candidate{
		{Name: "tool.tar.gz.sha256", URL: srv.URL + "/tool.tar.gz.sha256"},
		{Name: "SHA256SUMS", URL: srv.URL + "/SHA256SUMS"},
	}
	m, err := Discover(context.Background(), srv.Client(), cands, "tool.tar.gz", 1)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !m.Found || m.Hash != hash || m.Source != "SHA256SUMS" {
		t.Fatalf("unexpected match: %+v", m)
	}
	if len(hits) != 2 {
		t.Fatalf("expected two requests, got %v", hits)
	}
}

func TestDiscoverReportsMissing(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := Discover(context.Background(), srv.Client(), []Candidate{{Name: "sums", URL: srv.URL + "/sums"}}, "x", 1)
	if err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("expected a no-checksum error, got %v", err)
	}
}
