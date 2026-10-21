package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/progress"
	"github.com/SuSonicTH/cpt/internal/state"
)

// TestScriptedSearchSelectInstall drives a full search/select/install session
// through Bubble Tea, with a local HTTP fixture instead of GitHub.
func TestScriptedSearchSelectInstall(t *testing.T) {
	content := tgz(t, map[string]string{"mytool": "#!/bin/sh\necho hi\n"})
	mux := http.NewServeMux()
	mux.HandleFunc("/tool-1.0.0.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	a, err := app.Open(app.Options{Root: root, Quiet: true, Stdout: io.Discard, Stderr: io.Discard, Events: progress.New()})
	if err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`{"schema":1,"name":"test","packages":[{"name":"mytool","description":"fixture tool","version":"1.0.0","source":{"type":"url","url":%q,"bins":["mytool"]}}]}`,
		srv.URL+"/tool-{version}.tar.gz")
	if err := os.WriteFile(filepath.Join(root, "lists", "test.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newModel(context.Background(), a)
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 30))
	t.Cleanup(func() {
		_ = tm.Quit()
		done := make(chan struct{})
		go func() {
			tm.GetProgram().Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		m.shutdown()
		a.Close()
	})

	teatest.WaitFor(t, tm.Output(), func(out []byte) bool {
		return bytes.Contains(out, []byte("mytool"))
	}, teatest.WithDuration(5*time.Second))

	tm.Type("/")
	tm.Type("mytool")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	tm.Send(tea.KeyMsg{Type: tea.KeySpace})
	tm.Type("i")

	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := state.Load(filepath.Join(root, "state.json"))
		if err == nil && st.Packages["mytool"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("install did not complete")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", binaryName("mytool"))); err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}

	tm.Type("q")
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))
}
