package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SuSonicTH/cpt/internal/progress"
)

func TestInstallPublishesProgressEvents(t *testing.T) {
	content := tarGz(t, map[string]string{"mytool": "#!/bin/sh\necho hi\n"})
	mux := http.NewServeMux()
	mux.HandleFunc("/tool-1.0.0-linux-amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	bus := progress.New()
	events, unsubscribe := bus.Subscribe()
	defer unsubscribe()
	a, err := Open(Options{Root: root, Quiet: true, Stdout: os.Stdout, Stderr: os.Stderr, Events: bus})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	doc := fmt.Sprintf(`{"schema":1,"name":"test","packages":[{"name":"mytool","version":"1.0.0","source":{"type":"url","url":%q,"bins":["mytool"]}}]}`,
		srv.URL+"/tool-{version}-{os}-{arch}.tar.gz")
	if err := os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := a.WithLock(func() error {
		_, err := a.Install(context.Background(), []string{"mytool"}, InstallOptions{})
		return err
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	want := map[progress.Kind]bool{
		progress.KindQueued:      false,
		progress.KindResolving:   false,
		progress.KindDownloading: false,
		progress.KindVerifying:   false,
		progress.KindExtracting:  false,
		progress.KindInstalling:  false,
		progress.KindDone:        false,
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Package != "mytool" {
				continue
			}
			if _, ok := want[e.Kind]; ok {
				want[e.Kind] = true
			}
			if e.Kind == progress.KindDone {
				for kind, seen := range want {
					if !seen {
						t.Fatalf("missing progress event %q (seen %+v)", kind, want)
					}
				}
				if e.Action != "installed" || e.Version != "1.0.0" {
					t.Fatalf("unexpected done event: %+v", e)
				}
				if rows, err := a.Rows(cancelledContext(), ListOptions{Updates: true}); !errors.Is(err, context.Canceled) || rows != nil {
					t.Fatalf("cancelled Rows = %v, %v; want nil, context.Canceled", rows, err)
				}
				return
			}
		case <-deadline:
			t.Fatalf("timed out; seen %+v", want)
		}
	}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
