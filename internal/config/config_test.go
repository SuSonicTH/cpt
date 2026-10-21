package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultRootHonoursCPTHome(t *testing.T) {
	t.Setenv("CPT_ROOT", "")
	t.Setenv("CPT_HOME", "/opt/cpt-home")
	root, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Clean("/opt/cpt-home") {
		t.Fatalf("root = %q", root)
	}

	t.Setenv("CPT_ROOT", "/opt/cpt-root")
	root, err = DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Clean("/opt/cpt-root") {
		t.Fatalf("CPT_ROOT should win, got %q", root)
	}
}
