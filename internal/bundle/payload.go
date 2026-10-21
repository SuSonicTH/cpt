package bundle

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/SuSonicTH/cpt/internal/archive"
	"github.com/SuSonicTH/cpt/internal/cerr"
)

const (
	// MaxPayloadFileSize bounds a single file inside a bundle payload.
	MaxPayloadFileSize = 1 << 30
	// MaxPayloadSize bounds the total uncompressed payload.
	MaxPayloadSize = 4 << 30
	// MaxManifestSize bounds bundle.json.
	MaxManifestSize = 8 << 20
)

// PayloadBin is a binary to copy into the payload's bin/ directory.
type PayloadBin struct {
	Name string
	Path string
}

// Payload is the logical content of a bundle payload.
type Payload struct {
	Manifest Manifest
	Lists    map[string][]byte
	Bins     []PayloadBin
	Init     map[string][]byte
}

// WritePayload writes a zstd-compressed tar payload to w.
func WritePayload(w io.Writer, p Payload) error {
	if err := p.Manifest.Validate(); err != nil {
		return err
	}
	enc, err := zstd.NewWriter(w)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(enc)
	pw := &payloadWriter{tw: tw}

	err = pw.addBytes("bundle.json", mustJSON(p.Manifest), 0o644)
	if err == nil {
		for _, name := range sortedKeys(p.Lists) {
			if err = pw.addBytes("lists/"+name+".json", p.Lists[name], 0o644); err != nil {
				break
			}
		}
	}
	bins := append([]PayloadBin(nil), p.Bins...)
	sort.Slice(bins, func(i, j int) bool { return bins[i].Name < bins[j].Name })
	if err == nil {
		for _, b := range bins {
			if err = pw.addFile("bin/"+b.Name, b.Path, 0o755); err != nil {
				break
			}
		}
	}
	if err == nil {
		for _, name := range sortedKeys(p.Init) {
			if err = pw.addBytes(name, p.Init[name], 0o644); err != nil {
				break
			}
		}
	}
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := enc.Close(); err == nil {
		err = closeErr
	}
	return err
}

type payloadWriter struct {
	tw      *tar.Writer
	total   int64
	entries int
}

func (p *payloadWriter) check(size int64) error {
	if size < 0 || size > MaxPayloadFileSize {
		return fmt.Errorf("payload entry exceeds %d bytes", MaxPayloadFileSize)
	}
	p.total += size
	p.entries++
	if p.total > MaxPayloadSize {
		return fmt.Errorf("bundle payload exceeds %d bytes", int64(MaxPayloadSize))
	}
	if p.entries > 10000 {
		return fmt.Errorf("bundle payload has too many entries")
	}
	return nil
}

func (p *payloadWriter) addBytes(name string, data []byte, mode int64) error {
	if err := p.check(int64(len(data))); err != nil {
		return err
	}
	hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}
	if err := p.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if _, err := p.tw.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func (p *payloadWriter) addFile(name, path string, mode int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if err := p.check(info.Size()); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	hdr := &tar.Header{Name: name, Mode: mode, Size: info.Size(), Typeflag: tar.TypeReg}
	if err := p.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if _, err := io.Copy(p.tw, f); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func mustJSON(v any) []byte {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(data, '\n')
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ExtractPayload decompresses r into dest and returns the parsed manifest. Every
// entry must stay inside dest. The manifest and list hashes are cross-checked
// after extraction.
func ExtractPayload(r io.Reader, dest string) (Manifest, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return Manifest{}, err
	}
	dec, err := zstd.NewReader(r)
	if err != nil {
		return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("open bundle payload: %w", err))
	}
	defer dec.Close()
	tr := tar.NewReader(dec)

	seen := map[string]bool{}
	var total int64
	entries := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("read bundle payload: %w", err))
		}
		entries++
		if entries > 10000 {
			return Manifest{}, cerr.New(cerr.Integrity, "bundle payload has too many entries")
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return Manifest{}, cerr.New(cerr.Integrity, "bundle payload contains unsupported entry %q", hdr.Name)
		}
		rel, err := archive.SafeRel(hdr.Name)
		if err != nil {
			return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("bundle payload: %w", err))
		}
		rel = filepath.ToSlash(rel)
		if seen[rel] {
			return Manifest{}, cerr.New(cerr.Integrity, "bundle payload contains %q twice", rel)
		}
		seen[rel] = true
		limit := int64(MaxPayloadFileSize)
		if rel == "bundle.json" {
			limit = MaxManifestSize
		}
		if hdr.Size < 0 || hdr.Size > limit {
			return Manifest{}, cerr.New(cerr.Integrity, "%s: entry too large", rel)
		}
		total += hdr.Size
		if total > MaxPayloadSize {
			return Manifest{}, cerr.New(cerr.Integrity, "bundle payload exceeds %d bytes", int64(MaxPayloadSize))
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return Manifest{}, err
		}
		mode := hdr.FileInfo().Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return Manifest{}, err
		}
		written, copyErr := io.Copy(f, io.LimitReader(tr, hdr.Size))
		closeErr := f.Close()
		if copyErr != nil {
			return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("extract %s: %w", rel, copyErr))
		}
		if closeErr != nil {
			return Manifest{}, closeErr
		}
		if written != hdr.Size {
			return Manifest{}, cerr.New(cerr.Integrity, "extract %s: expected %d bytes, got %d", rel, hdr.Size, written)
		}
	}
	// Force the decompressor to consume the rest of the frame so a streaming
	// verifier sees every compressed byte before we compare hashes.
	if _, err := io.Copy(io.Discard, dec); err != nil {
		return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("read bundle payload: %w", err))
	}

	data, err := os.ReadFile(filepath.Join(dest, "bundle.json"))
	if err != nil {
		return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("bundle payload has no bundle.json: %w", err))
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("parse bundle.json: %w", err))
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, cerr.Wrap(cerr.Integrity, err)
	}
	if err := verifyExtracted(dest, m); err != nil {
		return Manifest{}, cerr.Wrap(cerr.Integrity, err)
	}
	return m, nil
}

func verifyExtracted(dest string, m Manifest) error {
	for name, entry := range m.Lists {
		data, err := os.ReadFile(filepath.Join(dest, "lists", name+".json"))
		if err != nil {
			return fmt.Errorf("bundle is missing list %s: %w", name, err)
		}
		if entry.SHA256 != "" && hashHex(data) != entry.SHA256 {
			return fmt.Errorf("bundle list %s does not match its manifest hash", name)
		}
	}
	for _, p := range m.Packages {
		for _, bin := range p.Bins {
			path := filepath.Join(dest, "bin", bin)
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("bundle is missing binary %s for %s", bin, p.Name)
			}
		}
	}
	return nil
}

// ReadManifest verifies the container and parses bundle.json without writing to
// disk. It is used by --list and --dry-run.
func ReadManifest(c *Container) (Manifest, error) {
	return readManifest(c, true)
}

func readManifest(c *Container, verify bool) (Manifest, error) {
	if verify {
		if err := c.Verify(); err != nil {
			return Manifest{}, err
		}
	}
	dec, err := zstd.NewReader(c.Payload())
	if err != nil {
		return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("open bundle payload: %w", err))
	}
	defer dec.Close()
	tr := tar.NewReader(dec)
	var scanned int64
	entries := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("read bundle payload: %w", err))
		}
		entries++
		scanned += hdr.Size
		if entries > 10000 || scanned > MaxPayloadSize {
			return Manifest{}, cerr.New(cerr.Integrity, "bundle payload is too large")
		}
		if strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./") != "bundle.json" {
			continue
		}
		if hdr.Size < 0 || hdr.Size > MaxManifestSize {
			return Manifest{}, cerr.New(cerr.Integrity, "bundle.json is too large")
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxManifestSize+1))
		if err != nil {
			return Manifest{}, err
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("parse bundle.json: %w", err))
		}
		if err := m.Validate(); err != nil {
			return Manifest{}, cerr.Wrap(cerr.Integrity, err)
		}
		return m, nil
	}
	return Manifest{}, cerr.New(cerr.Integrity, "bundle payload has no bundle.json")
}

// InspectPayload validates a compressed payload without writing files. It
// checks list hashes and required binaries by streaming and discarding entries.
func InspectPayload(r io.Reader) (Manifest, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("open bundle payload: %w", err))
	}
	defer dec.Close()
	tr := tar.NewReader(dec)
	var m Manifest
	lists := map[string][]byte{}
	bins := map[string]bool{}
	seen := map[string]bool{}
	var listBytes int64
	var total int64
	entries := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("read bundle payload: %w", err))
		}
		entries++
		total += hdr.Size
		if entries > 10000 || total > MaxPayloadSize {
			return Manifest{}, cerr.New(cerr.Integrity, "bundle payload is too large")
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return Manifest{}, cerr.New(cerr.Integrity, "bundle payload contains unsupported entry %q", hdr.Name)
		}
		rel, err := archive.SafeRel(hdr.Name)
		if err != nil {
			return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("bundle payload: %w", err))
		}
		rel = filepath.ToSlash(rel)
		if seen[rel] {
			return Manifest{}, cerr.New(cerr.Integrity, "bundle payload contains %q twice", rel)
		}
		seen[rel] = true
		switch {
		case rel == "bundle.json":
			if hdr.Size > MaxManifestSize {
				return Manifest{}, cerr.New(cerr.Integrity, "bundle.json is too large")
			}
			data, err := io.ReadAll(io.LimitReader(tr, MaxManifestSize+1))
			if err != nil {
				return Manifest{}, cerr.Wrap(cerr.Integrity, err)
			}
			if err := json.Unmarshal(data, &m); err != nil {
				return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("parse bundle.json: %w", err))
			}
		case strings.HasPrefix(rel, "lists/") && strings.HasSuffix(rel, ".json"):
			if hdr.Size > MaxManifestSize {
				return Manifest{}, cerr.New(cerr.Integrity, "%s: list is too large", rel)
			}
			data, err := io.ReadAll(io.LimitReader(tr, MaxManifestSize+1))
			if err != nil {
				return Manifest{}, cerr.Wrap(cerr.Integrity, err)
			}
			listBytes += int64(len(data))
			if listBytes > 64<<20 {
				return Manifest{}, cerr.New(cerr.Integrity, "bundle lists exceed the inspection limit")
			}
			lists[strings.TrimSuffix(strings.TrimPrefix(rel, "lists/"), ".json")] = data
		case strings.HasPrefix(rel, "bin/"):
			bins[strings.TrimPrefix(rel, "bin/")] = true
			if _, err := io.Copy(io.Discard, io.LimitReader(tr, hdr.Size)); err != nil {
				return Manifest{}, cerr.Wrap(cerr.Integrity, err)
			}
		default:
			if _, err := io.Copy(io.Discard, io.LimitReader(tr, hdr.Size)); err != nil {
				return Manifest{}, cerr.Wrap(cerr.Integrity, err)
			}
		}
	}
	if _, err := io.Copy(io.Discard, dec); err != nil {
		return Manifest{}, cerr.Wrap(cerr.Integrity, fmt.Errorf("read bundle payload: %w", err))
	}
	if m.Name == "" {
		return Manifest{}, cerr.New(cerr.Integrity, "bundle payload has no bundle.json")
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, cerr.Wrap(cerr.Integrity, err)
	}
	for name, entry := range m.Lists {
		data, ok := lists[name]
		if !ok {
			return Manifest{}, cerr.New(cerr.Integrity, "bundle is missing list %s", name)
		}
		if entry.SHA256 != "" && hashHex(data) != entry.SHA256 {
			return Manifest{}, cerr.New(cerr.Integrity, "bundle list %s does not match its manifest hash", name)
		}
	}
	for _, p := range m.Packages {
		for _, bin := range p.Bins {
			if !bins[bin] {
				return Manifest{}, cerr.New(cerr.Integrity, "bundle is missing binary %s for %s", bin, p.Name)
			}
		}
	}
	return m, nil
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
