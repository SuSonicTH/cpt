package shellinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditRCFilePreservesUTF16(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.ps1")
	original := "# profile\nSet-Alias ll Get-ChildItem\n"
	if err := os.WriteFile(path, encodeRC(original, rcUTF16LE), 0o600); err != nil {
		t.Fatal(err)
	}
	opt := Options{Root: "/home/u/.cpt", Home: "/home/u"}
	block := MarkerBlock(ShellPwsh, "/home/u/.cpt/cptinit.ps1", opt)
	if changed, err := EditRCFile(path, true, block); err != nil || !changed {
		t.Fatalf("EditRCFile: changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xFE {
		t.Fatalf("UTF-16 BOM was lost: %x", data[:minInt(len(data), 4)])
	}
	text, encoding, err := decodeRC(data)
	if err != nil || encoding != rcUTF16LE {
		t.Fatalf("decodeRC: %v, encoding %v", err, encoding)
	}
	if !strings.Contains(text, BeginMarker) || !strings.Contains(text, "Set-Alias ll") {
		t.Fatalf("unexpected UTF-16 profile:\n%s", text)
	}
	if changed, err := EditRCFile(path, false, block); err != nil || !changed {
		t.Fatalf("uninstall: changed=%v err=%v", changed, err)
	}
	data, _ = os.ReadFile(path)
	text, _, _ = decodeRC(data)
	if text != original {
		t.Fatalf("UTF-16 round trip changed content:\n%q", text)
	}
}

func TestEditRCFilePreservesUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bashrc")
	original := "# profile\n"
	if err := os.WriteFile(path, encodeRC(original, rcUTF8BOM), 0o600); err != nil {
		t.Fatal(err)
	}
	opt := Options{Root: "/home/u/.cpt", Home: "/home/u"}
	block := MarkerBlock(ShellBash, "/home/u/.cpt/cptinit.sh", opt)
	if _, err := EditRCFile(path, true, block); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if len(data) < 3 || data[0] != 0xEF || data[1] != 0xBB || data[2] != 0xBF {
		t.Fatalf("UTF-8 BOM was lost: %x", data[:minInt(len(data), 4)])
	}
	text, encoding, _ := decodeRC(data)
	if encoding != rcUTF8BOM || strings.Count(text, BeginMarker) != 1 {
		t.Fatalf("BOM block handling failed: encoding=%v\n%s", encoding, text)
	}
	if _, err := EditRCFile(path, false, block); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	text, _, _ = decodeRC(data)
	if text != original {
		t.Fatalf("UTF-8 BOM round trip changed content: %q", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
