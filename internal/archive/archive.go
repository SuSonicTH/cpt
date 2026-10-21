// Package archive safely extracts release assets and selects binaries.
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const (
	// MaxFileSize bounds a single extracted file.
	MaxFileSize = 512 << 20
	// MaxTotalSize bounds an entire extraction.
	MaxTotalSize = 2 << 30
)

// File is one extracted regular file, relative to the extraction root.
type File struct {
	Rel  string
	Mode os.FileMode
}

// DetectFormat infers the archive format from a file name.
func DetectFormat(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"):
		return "tar.xz"
	case strings.HasSuffix(lower, ".tar.zst"), strings.HasSuffix(lower, ".tzst"):
		return "tar.zst"
	case strings.HasSuffix(lower, ".zip"):
		return "zip"
	default:
		return "raw"
	}
}

// Extract unpacks archivePath into dest and returns the regular files written.
// Symlinks, hardlinks and device nodes are skipped. Entries escaping dest are
// an error.
func Extract(archivePath, format, dest string) ([]File, error) {
	if format == "" || format == "auto" {
		format = DetectFormat(archivePath)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	switch format {
	case "raw":
		return extractRaw(archivePath, dest)
	case "zip":
		return extractZip(archivePath, dest)
	case "tar.gz", "tgz":
		r, err := openGzip(archivePath)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return extractTar(r, dest)
	case "tar.xz", "txz":
		r, err := openXZ(archivePath)
		if err != nil {
			return nil, err
		}
		return extractTar(r, dest)
	case "tar.zst", "tzst":
		r, err := openZstd(archivePath)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return extractTar(r, dest)
	default:
		return nil, fmt.Errorf("unsupported archive format %q", format)
	}
}

func openGzip(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("open gzip %s: %w", filepath.Base(p), err)
	}
	return &readCloser{Reader: zr, closers: []io.Closer{zr, f}}, nil
}

func openXZ(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	xr, err := xz.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("open xz %s: %w", filepath.Base(p), err)
	}
	return &readCloser{Reader: xr, closers: []io.Closer{f}}, nil
}

func openZstd(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	zr, err := zstd.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("open zstd %s: %w", filepath.Base(p), err)
	}
	return &readCloser{Reader: zr, closers: []io.Closer{zr.IOReadCloser(), f}}, nil
}

type readCloser struct {
	io.Reader
	closers []io.Closer
}

func (r *readCloser) Close() error {
	var err error
	for _, c := range r.closers {
		if cerr := c.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

func extractRaw(src, dest string) ([]File, error) {
	base := filepath.Base(src)
	base = strings.TrimSuffix(base, ".part")
	target := filepath.Join(dest, base)
	mode := os.FileMode(0o755)
	if err := copyFile(src, target, mode); err != nil {
		return nil, err
	}
	return []File{{Rel: base, Mode: mode}}, nil
}

func extractTar(r io.Reader, dest string) ([]File, error) {
	tr := tar.NewReader(r)
	var files []File
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue // symlinks, hardlinks and specials are deliberately skipped
		}
		rel, err := safeRel(hdr.Name)
		if err != nil {
			return nil, err
		}
		if hdr.Size > MaxFileSize {
			return nil, fmt.Errorf("%s: entry too large (%d bytes)", rel, hdr.Size)
		}
		total += hdr.Size
		if total > MaxTotalSize {
			return nil, fmt.Errorf("archive exceeds %d bytes", int64(MaxTotalSize))
		}
		mode := os.FileMode(hdr.Mode).Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := writeEntry(tr, dest, rel, mode, hdr.Size); err != nil {
			return nil, err
		}
		files = append(files, File{Rel: rel, Mode: mode})
	}
	return files, nil
}

func extractZip(src, dest string) ([]File, error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return nil, fmt.Errorf("open zip %s: %w", filepath.Base(src), err)
	}
	defer zr.Close()

	var files []File
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() {
			continue
		}
		rel, err := safeRel(f.Name)
		if err != nil {
			return nil, err
		}
		if f.UncompressedSize64 > MaxFileSize {
			return nil, fmt.Errorf("%s: entry too large", rel)
		}
		total += int64(f.UncompressedSize64)
		if total > MaxTotalSize {
			return nil, fmt.Errorf("archive exceeds %d bytes", int64(MaxTotalSize))
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		err = writeEntry(rc, dest, rel, mode, int64(f.UncompressedSize64))
		rc.Close()
		if err != nil {
			return nil, err
		}
		files = append(files, File{Rel: rel, Mode: mode})
	}
	return files, nil
}

func writeEntry(r io.Reader, dest, rel string, mode os.FileMode, size int64) error {
	target := filepath.Join(dest, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", rel, err)
	}
	defer f.Close()
	limit := size
	if limit < 0 {
		limit = MaxFileSize
	}
	written, err := io.Copy(f, io.LimitReader(r, limit))
	if err != nil {
		return fmt.Errorf("extract %s: %w", rel, err)
	}
	if written != size {
		return fmt.Errorf("extract %s: expected %d bytes, got %d", rel, size, written)
	}
	return nil
}

// safeRel rejects absolute paths and traversal outside the extraction root.
func safeRel(name string) (string, error) {
	name = filepath.ToSlash(strings.TrimSpace(name))
	if name == "" || name == "." {
		return "", fmt.Errorf("archive contains an empty path")
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", fmt.Errorf("archive entry %q has an absolute path", name)
	}
	if hasVolume(name) {
		return "", fmt.Errorf("archive entry %q has a drive letter", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}
	return filepath.FromSlash(clean), nil
}

// hasVolume reports a Windows-style drive or UNC prefix even on Unix builds.
func hasVolume(p string) bool {
	if len(p) >= 2 && p[1] == ':' {
		return true
	}
	return strings.HasPrefix(p, "//")
}

// Bin is a selected executable ready to be installed.
type Bin struct {
	Name string
	Path string
}

// Select chooses the executables to install. When spec is non-empty only those
// names are installed; otherwise every executable in the archive is considered,
// preferring a file named after the package.
func Select(files []File, dest string, spec []string, windows bool, pkgName string) ([]Bin, error) {
	if len(spec) > 0 {
		var out []Bin
		for _, want := range spec {
			found := ""
			for _, f := range files {
				if binMatches(filepath.Base(f.Rel), want, windows) {
					found = f.Rel
					break
				}
			}
			name := want
			if found == "" {
				// Distribution archives often name the binary after the asset,
				// for example yq_linux_amd64, while the list asks for "yq".
				var prefixed []File
				for _, f := range files {
					base := filepath.Base(f.Rel)
					if strings.HasPrefix(base, want+"-") || strings.HasPrefix(base, want+"_") || strings.HasPrefix(base, want+".") {
						if executable(f, windows) {
							prefixed = append(prefixed, f)
						}
					}
				}
				switch {
				case len(prefixed) == 1:
					found = prefixed[0].Rel
				case len(files) == 1:
					// A raw asset is a single file regardless of its name.
					found = files[0].Rel
				default:
					return nil, fmt.Errorf("binary %q not found in archive", want)
				}
			} else {
				name = filepath.Base(found)
			}
			if windows && !strings.EqualFold(filepath.Ext(name), ".exe") {
				name += ".exe"
			}
			out = append(out, Bin{Name: name, Path: filepath.Join(dest, found)})
		}
		return dedupe(out)
	}

	var candidates []File
	for _, f := range files {
		if depth(f.Rel) > 4 || excludedDoc(filepath.Base(f.Rel)) {
			continue
		}
		if !executable(f, windows) {
			continue
		}
		candidates = append(candidates, f)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no executables found in archive")
	}

	// Prefer the file named after the package when one exists.
	var preferred []File
	for _, f := range candidates {
		if binMatches(filepath.Base(f.Rel), pkgName, windows) {
			preferred = append(preferred, f)
		}
	}
	if len(preferred) > 0 {
		candidates = preferred
	}

	out := make([]Bin, 0, len(candidates))
	for _, f := range candidates {
		out = append(out, Bin{Name: filepath.Base(f.Rel), Path: filepath.Join(dest, f.Rel)})
	}
	return dedupe(out)
}

func dedupe(bins []Bin) ([]Bin, error) {
	seen := map[string]string{}
	var out []Bin
	for _, b := range bins {
		if prev, ok := seen[b.Name]; ok {
			if prev != b.Path {
				return nil, fmt.Errorf("archive contains two files named %q", b.Name)
			}
			continue
		}
		seen[b.Name] = b.Path
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func binMatches(base, want string, windows bool) bool {
	if base == want {
		return true
	}
	if windows {
		if strings.EqualFold(base, want) || strings.EqualFold(base, want+".exe") {
			return true
		}
	}
	return false
}

func executable(f File, windows bool) bool {
	if windows {
		return strings.EqualFold(filepath.Ext(f.Rel), ".exe")
	}
	return f.Mode&0o111 != 0
}

func depth(rel string) int {
	return strings.Count(filepath.ToSlash(rel), "/")
}

func excludedDoc(base string) bool {
	upper := strings.ToUpper(base)
	for _, prefix := range []string{"LICENSE", "README", "CHANGELOG", "COPYING", "NOTICE"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".md", ".txt", ".1", ".html", ".json", ".sha256", ".sha", ".asc", ".sig", ".pdf":
		return true
	}
	return false
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.LimitReader(in, MaxFileSize)); err != nil {
		return err
	}
	return nil
}
