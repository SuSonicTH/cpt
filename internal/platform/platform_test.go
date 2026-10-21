package platform

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Platform
		ok   bool
	}{
		{"linux/amd64", Platform{"linux", "amd64"}, true},
		{"linux/x86_64", Platform{"linux", "amd64"}, true},
		{"macos/aarch64", Platform{"darwin", "arm64"}, true},
		{"windows/arm64", Platform{"windows", "arm64"}, true},
		{"plan9/amd64", Platform{}, false},
		{"linux/mips", Platform{}, false},
		{"linux", Platform{}, false},
	}
	for _, tt := range cases {
		got, err := Parse(tt.in)
		if tt.ok != (err == nil) {
			t.Errorf("Parse(%q) error = %v", tt.in, err)
			continue
		}
		if tt.ok && got != tt.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestStringAndKnown(t *testing.T) {
	p := Current()
	if !p.Known() {
		t.Fatalf("current platform %s should be known", p)
	}
	if p.String() != p.OS+"/"+p.Arch {
		t.Fatalf("String = %q", p.String())
	}
}
