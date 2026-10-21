package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/SuSonicTH/cpt/internal/httpc"
)

// ErrNoChecksum reports that no candidate contained a hash for the asset.
var ErrNoChecksum = errors.New("no checksum available")

// Match is a checksum found for an asset.
type Match struct {
	Hash   string
	Source string
	Found  bool
}

// Candidate is a checksum file location to try.
type Candidate struct {
	Name string
	URL  string
}

var (
	hex64          = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	gnuLine        = regexp.MustCompile(`^([a-fA-F0-9]{64})\s+\*?(.+?)\s*$`)
	bsdLine        = regexp.MustCompile(`^SHA256\s*\((.+)\)\s*=\s*([a-fA-F0-9]{64})$`)
	hexToken       = regexp.MustCompile(`\b[a-fA-F0-9]{64}\b`)
	maxSums  int64 = 1 << 20
)

// Discover fetches the first checksum file that contains a hash for asset.
// A missing file (404) moves on to the next candidate. ErrNotFound is returned
// when no candidate yields a match.
func Discover(ctx context.Context, client *http.Client, candidates []Candidate, asset string) (Match, error) {
	var attempted []string
	for _, cand := range candidates {
		attempted = append(attempted, cand.Name)
		data, _, err := httpc.Get(ctx, client, cand.URL, maxSums, nil)
		if err != nil {
			if errors.Is(err, httpc.ErrNotFound) {
				continue
			}
			return Match{}, err
		}
		if hash, ok := matchChecksum(data, asset); ok {
			return Match{Hash: hash, Source: cand.Name, Found: true}, nil
		}
	}
	return Match{}, fmt.Errorf("%w for %s (tried %s)", ErrNoChecksum, asset, strings.Join(attempted, ", "))
}

// matchChecksum extracts the hash for asset from checksum file content.
func matchChecksum(data []byte, asset string) (string, bool) {
	base := strings.TrimSuffix(asset, "/")
	var bare string
	bareCount := 0
	for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := bsdLine.FindStringSubmatch(line); m != nil {
			if nameMatches(m[1], base) {
				return strings.ToLower(m[2]), true
			}
			continue
		}
		if m := gnuLine.FindStringSubmatch(line); m != nil {
			if nameMatches(m[2], base) {
				return strings.ToLower(m[1]), true
			}
			continue
		}
		if hex64.MatchString(line) {
			bare = strings.ToLower(line)
			bareCount++
		}
	}
	if bareCount == 1 {
		return bare, true
	}
	return "", false
}

func nameMatches(name, asset string) bool {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "./")
	if name == asset {
		return true
	}
	// Some projects prefix directories, e.g. "dist/tool_x.tar.gz".
	if strings.HasSuffix(name, "/"+asset) {
		return true
	}
	// Windows releases occasionally store the plain .exe name.
	return strings.EqualFold(name, asset)
}

// ExtractHex returns the lowercase hex hash embedded in a per-asset file such as
// "tool.tar.gz.sha256" when the file has no explicit file name.
func ExtractHex(data []byte) (string, bool) {
	s := strings.TrimSpace(string(data))
	if hex64.MatchString(s) {
		return strings.ToLower(s), true
	}
	if m := hexToken.FindString(s); m != "" {
		return strings.ToLower(m), true
	}
	return "", false
}
