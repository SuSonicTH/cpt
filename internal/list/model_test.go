package list

import (
	"strings"
	"testing"
)

func validDoc() string {
	return `{
	  "schema": 1,
	  "name": "test",
	  "packages": [
	    {
	      "name": "tool",
	      "source": {
	        "type": "github",
	        "repo": "acme/tool",
	        "bins": ["tool"],
	        "asset": {"linux/amd64": "tool-{version}.tar.gz"}
	      }
	    }
	  ]
	}`
}

func TestParseValid(t *testing.T) {
	l, err := Parse([]byte(validDoc()), "test")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if l.Name != "test" || len(l.Packages) != 1 {
		t.Fatalf("unexpected list: %+v", l)
	}
	if l.Packages[0].Source.VersionStrip != "" {
		t.Fatal("expected default strip to be empty")
	}
}

func TestParseToleratesUnknownFields(t *testing.T) {
	doc := strings.Replace(validDoc(), `"name": "tool",`, `"name": "tool", "future": true,`, 1)
	if _, err := Parse([]byte(doc), "test"); err != nil {
		t.Fatalf("unknown fields should be ignored: %v", err)
	}
}

func TestParseDuplicatePackage(t *testing.T) {
	doc := `{"schema":1,"name":"t","packages":[
	  {"name":"a","source":{"type":"github","repo":"o/r","asset":{"linux/amd64":"a"}}},
	  {"name":"a","source":{"type":"github","repo":"o/r","asset":{"linux/amd64":"a"}}}]}`
	if _, err := Parse([]byte(doc), "t"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestParseRejectsBadPlatform(t *testing.T) {
	doc := `{"schema":1,"name":"t","packages":[
	  {"name":"a","source":{"type":"github","repo":"o/r","asset":{"plan9/amd64":"a"}}}]}`
	if _, err := Parse([]byte(doc), "t"); err == nil || !strings.Contains(err.Error(), "unsupported os") {
		t.Fatalf("expected platform error, got %v", err)
	}
}

func TestParseURLRequiresVersion(t *testing.T) {
	doc := `{"schema":1,"name":"t","packages":[
	  {"name":"a","source":{"type":"url","url":"https://x/{version}/a"}}]}`
	if _, err := Parse([]byte(doc), "t"); err == nil || !strings.Contains(err.Error(), "pinned version") {
		t.Fatalf("expected version error, got %v", err)
	}
}

func TestParseInvalidName(t *testing.T) {
	doc := `{"schema":1,"name":"t","packages":[
	  {"name":"Bad Name","source":{"type":"github","repo":"o/r","asset":{"linux/amd64":"a"}}}]}`
	if _, err := Parse([]byte(doc), "t"); err == nil {
		t.Fatal("expected invalid name error")
	}
}

func TestCatalogConflict(t *testing.T) {
	a, err := Parse([]byte(validDoc()), "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte(validDoc()), "b")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := BuildCatalog([]Named{{Name: "a", List: a}, {Name: "b", List: b}})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	if _, err := cat.Lookup("tool"); err == nil || !strings.Contains(err.Error(), "multiple lists") {
		t.Fatalf("expected conflict error, got %v", err)
	}
	if got := cat.Conflicts()["tool"]; len(got) != 2 {
		t.Fatalf("unexpected conflicts: %v", got)
	}
}

func TestCatalogLookup(t *testing.T) {
	a, _ := Parse([]byte(validDoc()), "a")
	cat, err := BuildCatalog([]Named{{Name: "a", List: a}})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := cat.Lookup("tool")
	if err != nil || entry.List != "a" {
		t.Fatalf("Lookup: %+v, %v", entry, err)
	}
	if _, err := cat.Lookup("missing"); err == nil {
		t.Fatal("expected unknown package error")
	}
}

func TestParseRejectsTrailingData(t *testing.T) {
	doc := validDoc() + " trailing"
	if _, err := Parse([]byte(doc), "test"); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("expected trailing data error, got %v", err)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Core Tools": "core-tools",
		"../../etc":  "etc",
		"":           "list",
		"ok_name-1":  "ok_name-1",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseShellBlock(t *testing.T) {
	doc := `{"schema":1,"name":"t","packages":[{"name":"a","source":{"type":"url","url":"https://x/{version}/a","bins":["a"]},"version":"1.0.0",
	  "shell":{"bash":{"functions":"alias a='a'\n","env":{"A":"1"},"path":["/opt/a/bin"]},
	           "pwsh":{"functions_url":"https://x/a.ps1","functions_sha256":"` + strings.Repeat("a", 64) + `"}}}]}`
	if _, err := Parse([]byte(doc), "t"); err != nil {
		t.Fatalf("valid shell block rejected: %v", err)
	}
}

func TestParseRejectsVersionInjection(t *testing.T) {
	doc := `{"schema":1,"name":"t","packages":[{"name":"a","version":"1.0\necho pwn","source":{"type":"url","url":"https://x/a"}}]}`
	if _, err := Parse([]byte(doc), "t"); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("expected a version error, got %v", err)
	}
}

func TestParseShellURLPolicy(t *testing.T) {
	sha := strings.Repeat("a", 64)
	doc := func(url string) string {
		return `{"schema":1,"name":"t","packages":[{"name":"a","source":{"type":"url","url":"https://x/{version}/a"},"version":"1.0.0",
		  "shell":{"bash":{"functions_url":"` + url + `","functions_sha256":"` + sha + `"}}}]}`
	}
	if _, err := Parse([]byte(doc("http://127.0.0.1:8080/s.sh")), "t"); err != nil {
		t.Fatalf("loopback http should be accepted: %v", err)
	}
	if _, err := Parse([]byte(doc("http://example.com/s.sh")), "t"); err == nil {
		t.Fatal("non-loopback http should be rejected")
	}
	if _, err := Parse([]byte(doc("https://example.com/s.sh")), "t"); err != nil {
		t.Fatalf("https should be accepted: %v", err)
	}
}

func TestParseShellPwshPathSeparators(t *testing.T) {
	doc := func(paths string) string {
		return `{"schema":1,"name":"t","packages":[{"name":"a","source":{"type":"url","url":"https://x/a"},"version":"1.0.0",
		  "shell":{"pwsh":{"functions":"x","path":` + paths + `}}}]}`
	}
	if _, err := Parse([]byte(doc(`["C:\\Tools"]`)), "t"); err != nil {
		t.Fatalf("Windows drive path rejected: %v", err)
	}
	for _, paths := range []string{`["C:\\a:b"]`, `["a;b"]`} {
		if _, err := Parse([]byte(doc(paths)), "t"); err == nil {
			t.Errorf("ambiguous path %s was accepted", paths)
		}
	}
}

func TestParseRejectsBadShellBlocks(t *testing.T) {
	const pkg = `{"schema":1,"name":"t","packages":[{"name":"a","source":{"type":"url","url":"https://x/{version}/a"},"version":"1.0.0","shell":%s}]}`
	cases := map[string]string{
		"empty shell":       `{}`,
		"empty flavor":      `{"bash":{}}`,
		"both sources":      `{"bash":{"functions":"x","functions_url":"https://x/a"}}`,
		"url without hash":  `{"bash":{"functions_url":"https://x/a"}}`,
		"short hash":        `{"bash":{"functions_url":"https://x/a","functions_sha256":"abc"}}`,
		"hash without url":  `{"bash":{"functions":"x","functions_sha256":"` + strings.Repeat("a", 64) + `"}}`,
		"bad env name":      `{"bash":{"functions":"x","env":{"BAD-NAME":"1"}}}`,
		"newline env value": `{"bash":{"functions":"x","env":{"A":"a\nb"}}}`,
		"empty path":        `{"bash":{"functions":"x","path":[""]}}`,
		"colon in path":     `{"bash":{"functions":"x","path":["/a:b"]}}`,
		"init marker":       `{"bash":{"functions":"# >>> cpt:init"}}`,
		"bad url":           `{"bash":{"functions_url":"ftp://x/a","functions_sha256":"` + strings.Repeat("a", 64) + `"}}`,
	}
	for name, shell := range cases {
		doc := strings.Replace(pkg, "%s", shell, 1)
		if _, err := Parse([]byte(doc), "t"); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestPlatformsSorted(t *testing.T) {
	p := Package{Source: Source{Asset: map[string]string{
		"windows/amd64": "c",
		"linux/amd64":   "a",
		"darwin/arm64":  "b",
	}}}
	got := p.Platforms()
	want := []string{"darwin/arm64", "linux/amd64", "windows/amd64"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Platforms = %v, want %v", got, want)
		}
	}
}
