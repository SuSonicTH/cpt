package list

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	envNameRE      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	sha256RE       = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	windowsDriveRE = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
)

// Shell markers used by the generated rc-file block. Snippets must not contain
// them: doing so could confuse the opt-in rc-file editor.
const (
	shellBlockBegin = "# >>> cpt:init"
	shellBlockEnd   = "# <<< cpt:init"
)

// Validate checks a package's shell block.
func (s *Shell) Validate() error {
	if s.Bash == nil && s.Pwsh == nil {
		return fmt.Errorf("shell requires a bash or pwsh snippet")
	}
	if s.Bash != nil {
		if err := s.Bash.validate("bash"); err != nil {
			return err
		}
	}
	if s.Pwsh != nil {
		if err := s.Pwsh.validate("pwsh"); err != nil {
			return err
		}
	}
	return nil
}

func (f *ShellFlavor) validate(flavor string) error {
	if f.Functions != "" && f.FunctionsURL != "" {
		return fmt.Errorf("shell.%s: functions and functions_url are mutually exclusive", flavor)
	}
	if strings.TrimSpace(f.Functions) == "" && f.FunctionsURL == "" && len(f.Env) == 0 && len(f.Path) == 0 {
		return fmt.Errorf("shell.%s: snippet is empty", flavor)
	}
	if f.FunctionsURL != "" {
		u, err := url.Parse(f.FunctionsURL)
		if err != nil || u == nil || u.Host == "" || !shellURLAllowed(u) {
			return fmt.Errorf("shell.%s: functions_url must be an https URL (loopback may use http)", flavor)
		}
		if !sha256RE.MatchString(f.FunctionsSHA256) {
			return fmt.Errorf("shell.%s: functions_url requires functions_sha256 (64 hex characters)", flavor)
		}
	} else if f.FunctionsSHA256 != "" {
		return fmt.Errorf("shell.%s: functions_sha256 requires functions_url", flavor)
	}
	for key, value := range f.Env {
		if !envNameRE.MatchString(key) {
			return fmt.Errorf("shell.%s: invalid environment variable %q", flavor, key)
		}
		if !utf8.ValidString(value) {
			return fmt.Errorf("shell.%s: environment variable %s is not valid UTF-8", flavor, key)
		}
		if hasControl(value) {
			return fmt.Errorf("shell.%s: environment variable %s contains a control character", flavor, key)
		}
	}
	for _, dir := range f.Path {
		if strings.TrimSpace(dir) == "" {
			return fmt.Errorf("shell.%s: path entries must not be empty", flavor)
		}
		if !utf8.ValidString(dir) {
			return fmt.Errorf("shell.%s: path entry is not valid UTF-8", flavor)
		}
		if hasControl(dir) {
			return fmt.Errorf("shell.%s: path entry %q contains a control character", flavor, dir)
		}
		if flavor == "bash" && strings.Contains(dir, ":") {
			return fmt.Errorf("shell.%s: path entry %q must not contain ':'", flavor, dir)
		}
		if flavor == "pwsh" {
			// Deliberately restrict lists to a cross-platform path subset: the
			// same entry may be parsed on a different OS than it runs on. A
			// single leading drive colon is still accepted for Windows paths.
			if strings.Contains(dir, ";") {
				return fmt.Errorf("shell.%s: path entry %q must not contain ';'", flavor, dir)
			}
			if n := strings.Count(dir, ":"); n > 1 || (n == 1 && !windowsDriveRE.MatchString(dir)) {
				return fmt.Errorf("shell.%s: path entry %q has an ambiguous ':' separator", flavor, dir)
			}
		}
	}
	if err := ValidateFunctions(f.Functions); err != nil {
		return fmt.Errorf("shell.%s: %w", flavor, err)
	}
	return nil
}

// ValidateFunctions checks shell code for bytes that would corrupt the
// generated script, and for markers that would confuse cpt's rc-file editor.
// It accepts newlines, carriage returns and tabs; other control characters are
// rejected.
func ValidateFunctions(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("functions are not valid UTF-8")
	}
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if unsafeRune(r) {
			return fmt.Errorf("functions contain unsafe character %U", r)
		}
	}
	if strings.Contains(s, shellBlockBegin) || strings.Contains(s, shellBlockEnd) {
		return fmt.Errorf("functions must not contain cpt init markers")
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unsafeRune(r) {
			return true
		}
	}
	return false
}

// unsafeRune covers controls plus bidi/format characters and Unicode line
// separators, all of which can corrupt markers or spoof terminal output.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
}

func shellURLAllowed(u *url.URL) bool {
	if strings.EqualFold(u.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidateVersion rejects version strings that would corrupt generated marker
// comments. Release versions are printable tokens, not free-form text.
func ValidateVersion(version string) error {
	if !utf8.ValidString(version) {
		return fmt.Errorf("version is not valid UTF-8")
	}
	for _, r := range version {
		if unsafeRune(r) {
			return fmt.Errorf("version %q contains a control or format character", version)
		}
	}
	return nil
}
