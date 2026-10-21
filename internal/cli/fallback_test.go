package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SuSonicTH/cpt/internal/progress"
)

func TestNoCommandNonTTYFallsBackToHelp(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run(context.Background(), nil, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errb.String(), "interactive mode needs a terminal") {
		t.Fatalf("missing fallback message: %q", errb.String())
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("missing command help: %q", out.String())
	}
}

func TestNoTUIForcesHelp(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run(context.Background(), []string{"--no-tui"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errb.String(), "disabled by --no-tui") {
		t.Fatalf("missing fallback message: %q", errb.String())
	}
}

func TestIsTTYRejectsDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTTY(f) {
		t.Fatal("/dev/null must not be treated as a terminal")
	}
	var buf bytes.Buffer
	if isTTY(&buf) {
		t.Fatal("a buffer must not be treated as a terminal")
	}
}

func TestVersionFlagWithoutCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"--version"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "dev") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestUIWriterTurnsOutputIntoNotices(t *testing.T) {
	bus := progress.New()
	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()
	w := &uiWriter{bus: bus}

	if _, err := w.Write([]byte("first\nsecond\npart")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first", "second"} {
		select {
		case e := <-ch:
			if e.Kind != progress.KindInfo || e.Message != want {
				t.Fatalf("got %+v, want %q", e, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	select {
	case e := <-ch:
		t.Fatalf("unterminated line should be buffered, got %+v", e)
	default:
	}
	if _, err := w.Write([]byte("ial\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-ch:
		if e.Message != "partial" {
			t.Fatalf("got %q, want partial", e.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for buffered line")
	}
}
