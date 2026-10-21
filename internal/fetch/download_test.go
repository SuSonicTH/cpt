package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadHonoursRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte("payload"))
	}))
	defer srv.Close()

	d := &Downloader{Client: srv.Client(), Attempts: 2}
	start := time.Now()
	sum, size, err := d.To(context.Background(), srv.URL, t.TempDir()+"/asset", "test")
	if err != nil {
		t.Fatalf("To: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("Retry-After was not honoured (waited %v)", elapsed)
	}
	want := sha256.Sum256([]byte("payload"))
	if sum != hex.EncodeToString(want[:]) || size != 7 {
		t.Fatalf("unexpected result: %s %d", sum, size)
	}
}

func TestDownloadDoesNotRetryNotFound(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	d := &Downloader{Client: srv.Client(), Attempts: 3}
	_, _, err := d.To(context.Background(), srv.URL+"/missing", t.TempDir()+"/asset", "test")
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Fatalf("404 was retried %d times", calls)
	}
}
