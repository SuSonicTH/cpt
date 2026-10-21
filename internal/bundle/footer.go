// Package bundle implements cpt's self-extracting bundle container.
package bundle

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/SuSonicTH/cpt/internal/cerr"
)

const (
	// Magic identifies a bundle footer.
	Magic = "CPTBDL01"
	// FormatVersion is the container version written by this build.
	FormatVersion = 1
	// FooterSize is the fixed size of the trailer.
	FooterSize = 64

	// FlagFrozen marks a bundle whose lists are read-only.
	FlagFrozen = 1 << 0
)

// Footer is the 64-byte trailer that follows the compressed payload.
type Footer struct {
	PayloadOffset uint64
	PayloadLen    uint64
	SHA256        [32]byte
	CRC32C        uint32
	Version       uint16
	Flags         uint16
}

// Frozen reports whether the bundle marks its lists read-only.
func (f Footer) Frozen() bool { return f.Flags&FlagFrozen != 0 }

// Marshal encodes the footer in its little-endian on-disk form.
func (f Footer) Marshal() []byte {
	out := make([]byte, FooterSize)
	copy(out[0:8], Magic)
	binary.LittleEndian.PutUint64(out[8:16], f.PayloadOffset)
	binary.LittleEndian.PutUint64(out[16:24], f.PayloadLen)
	copy(out[24:56], f.SHA256[:])
	binary.LittleEndian.PutUint32(out[56:60], f.CRC32C)
	binary.LittleEndian.PutUint16(out[60:62], f.Version)
	binary.LittleEndian.PutUint16(out[62:64], f.Flags)
	return out
}

// ParseFooter decodes the last 64 bytes of a file.
func ParseFooter(data []byte) (Footer, error) {
	if len(data) < FooterSize {
		return Footer{}, cerr.New(cerr.Integrity, "bundle footer is truncated")
	}
	data = data[len(data)-FooterSize:]
	if string(data[0:8]) != Magic {
		return Footer{}, cerr.New(cerr.Integrity, "not a cpt bundle (bad footer magic)")
	}
	f := Footer{
		PayloadOffset: binary.LittleEndian.Uint64(data[8:16]),
		PayloadLen:    binary.LittleEndian.Uint64(data[16:24]),
		CRC32C:        binary.LittleEndian.Uint32(data[56:60]),
		Version:       binary.LittleEndian.Uint16(data[60:62]),
		Flags:         binary.LittleEndian.Uint16(data[62:64]),
	}
	copy(f.SHA256[:], data[24:56])
	if f.Version == 0 || f.Version > FormatVersion {
		return Footer{}, cerr.New(cerr.Integrity,
			"bundle format version %d is not supported (this build understands up to %d)",
			f.Version, FormatVersion)
	}
	return f, nil
}

// ReadFooter reads and validates the trailer of a bundle file.
func ReadFooter(f *os.File) (Footer, error) {
	info, err := f.Stat()
	if err != nil {
		return Footer{}, err
	}
	if info.Size() < FooterSize {
		return Footer{}, cerr.New(cerr.Integrity, "not a cpt bundle (file is too small)")
	}
	buf := make([]byte, FooterSize)
	if _, err := f.ReadAt(buf, info.Size()-FooterSize); err != nil {
		return Footer{}, err
	}
	footer, err := ParseFooter(buf)
	if err != nil {
		return Footer{}, err
	}
	if footer.PayloadOffset == 0 || footer.PayloadLen == 0 {
		return Footer{}, cerr.New(cerr.Integrity, "bundle footer has an empty payload")
	}
	size := uint64(info.Size())
	if footer.PayloadOffset > size ||
		footer.PayloadLen > size-footer.PayloadOffset ||
		size-footer.PayloadOffset-footer.PayloadLen != FooterSize {
		return Footer{}, cerr.New(cerr.Integrity,
			"bundle footer does not match the file size (offset %d, length %d, file %d)",
			footer.PayloadOffset, footer.PayloadLen, info.Size())
	}
	return footer, nil
}

// Container is an opened bundle file.
type Container struct {
	Path   string
	Footer Footer
	file   *os.File
}

// Open opens path and reads its footer. It does not verify the payload hash;
// call Verify before trusting the contents.
func Open(path string) (*Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	footer, err := ReadFooter(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Container{Path: path, Footer: footer, file: f}, nil
}

// Close releases the underlying file.
func (c *Container) Close() error { return c.file.Close() }

// Payload returns a reader over the compressed payload.
func (c *Container) Payload() io.Reader {
	return io.NewSectionReader(c.file, int64(c.Footer.PayloadOffset), int64(c.Footer.PayloadLen))
}

// Base returns a reader over the executable bytes before the payload.
func (c *Container) Base() io.Reader {
	return io.NewSectionReader(c.file, 0, int64(c.Footer.PayloadOffset))
}

// VerifyingPayload returns a reader that hashes the compressed bytes as they
// are consumed. The returned function compares the hashes with the footer; call
// it only after the reader has been drained to EOF.
func (c *Container) VerifyingPayload() (io.Reader, func() error) {
	h := sha256.New()
	crc := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	check := func() error {
		if sum := h.Sum(nil); !equalBytes(sum, c.Footer.SHA256[:]) {
			return cerr.New(cerr.Integrity,
				"bundle payload checksum mismatch: expected %x, got %x", c.Footer.SHA256, sum)
		}
		if got := crc.Sum32(); got != c.Footer.CRC32C {
			return cerr.New(cerr.Integrity,
				"bundle payload CRC mismatch: expected %08x, got %08x", c.Footer.CRC32C, got)
		}
		return nil
	}
	return io.TeeReader(c.Payload(), io.MultiWriter(h, crc)), check
}

// Verify streams the compressed payload and checks its SHA256 and CRC32C.
func (c *Container) Verify() error {
	h := sha256.New()
	crc := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	if _, err := io.Copy(io.MultiWriter(h, crc), c.Payload()); err != nil {
		return fmt.Errorf("read bundle payload: %w", err)
	}
	if sum := h.Sum(nil); !equalBytes(sum, c.Footer.SHA256[:]) {
		return cerr.New(cerr.Integrity,
			"bundle payload checksum mismatch: expected %x, got %x", c.Footer.SHA256, sum)
	}
	if got := crc.Sum32(); got != c.Footer.CRC32C {
		return cerr.New(cerr.Integrity,
			"bundle payload CRC mismatch: expected %08x, got %08x", c.Footer.CRC32C, got)
	}
	return nil
}

// IsBundle reports whether path ends with a syntactically valid footer.
func IsBundle(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = ReadFooter(f)
	return err == nil
}

// SelfPath returns the executable path of the running process.
func SelfPath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}
	return path, nil
}

// DetectSelf reports whether the running executable is a bundle. A file without
// the footer magic is simply a normal cpt binary (isBundle false, nil). A file
// with the magic but an invalid footer returns isBundle true and the error, so
// the caller can report corruption instead of dispatching installer arguments
// as a cpt subcommand.
func DetectSelf() (string, bool, error) {
	path, err := SelfPath()
	if err != nil {
		return "", false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return path, false, nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() < FooterSize {
		return path, false, nil
	}
	magic := make([]byte, len(Magic))
	if _, err := f.ReadAt(magic, info.Size()-int64(FooterSize)); err != nil {
		return path, false, nil
	}
	if string(magic) != Magic {
		return path, false, nil
	}
	if _, err := ReadFooter(f); err != nil {
		return path, true, err
	}
	return path, true, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
