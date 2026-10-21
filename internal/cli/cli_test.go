package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run(context.Background(), []string{"version"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "dev") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestHelp(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "Completely Portable Tools") {
		t.Fatalf("unexpected help: %s", out.String())
	}
}

func TestListJSON(t *testing.T) {
	root := t.TempDir()
	var out, errb bytes.Buffer
	code := Run(context.Background(), []string{"--root", root, "--json", "list"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	var env struct {
		OK   bool             `json:"ok"`
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if !env.OK || len(env.Data) != 25 {
		t.Fatalf("unexpected envelope: ok=%v packages=%d", env.OK, len(env.Data))
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"nope"}, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestBadFlag(t *testing.T) {
	root := t.TempDir()
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"--root", root, "install", "--nope"}, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
