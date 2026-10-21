package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/SuSonicTH/cpt/internal/platform"
)

func testManifest(name string, frozen bool) Manifest {
	return Manifest{
		Schema:     ManifestSchema,
		Name:       name,
		Target:     platform.Current().String(),
		CptVersion: "test",
		Frozen:     frozen,
		Lists:      map[string]ManifestList{},
	}
}

func buildTestFile(t *testing.T, dir, base string, p Payload) string {
	t.Helper()
	var payload bytes.Buffer
	if err := WritePayload(&payload, p); err != nil {
		t.Fatalf("WritePayload: %v", err)
	}
	var out bytes.Buffer
	out.WriteString(base)
	sum := sha256.Sum256(payload.Bytes())
	footer := Footer{
		PayloadOffset: uint64(len(base)),
		PayloadLen:    uint64(payload.Len()),
		CRC32C:        crc32c(payload.Bytes()),
		Version:       FormatVersion,
	}
	copy(footer.SHA256[:], sum[:])
	if p.Manifest.Frozen {
		footer.Flags |= FlagFrozen
	}
	out.Write(payload.Bytes())
	out.Write(footer.Marshal())
	path := filepath.Join(dir, "test-bundle")
	if err := os.WriteFile(path, out.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFooterRoundTrip(t *testing.T) {
	f := Footer{PayloadOffset: 1234, PayloadLen: 5678, Version: FormatVersion, Flags: FlagFrozen}
	data := f.Marshal()
	if len(data) != FooterSize {
		t.Fatalf("footer size = %d", len(data))
	}
	got, err := ParseFooter(append([]byte("prefix"), data...))
	if err != nil {
		t.Fatal(err)
	}
	if got.PayloadOffset != f.PayloadOffset || got.PayloadLen != f.PayloadLen || !got.Frozen() {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestParseFooterRejectsBadInput(t *testing.T) {
	f := Footer{PayloadOffset: 10, PayloadLen: 20, Version: FormatVersion}
	data := f.Marshal()

	if _, err := ParseFooter(data[:FooterSize-1]); err == nil {
		t.Fatal("truncated footer accepted")
	}
	bad := append([]byte(nil), data...)
	bad[0] = 'X'
	if _, err := ParseFooter(bad); err == nil {
		t.Fatal("bad magic accepted")
	}
	newer := append([]byte(nil), data...)
	newer[60] = FormatVersion + 1
	if _, err := ParseFooter(newer); err == nil {
		t.Fatal("newer format version accepted")
	}
}

func TestReadFooterValidatesFileSize(t *testing.T) {
	f := Footer{PayloadOffset: 4, PayloadLen: 10, Version: FormatVersion}
	data := append([]byte("base"), make([]byte, 10)...)
	data = append(data, f.Marshal()...)
	path := filepath.Join(t.TempDir(), "bundle")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c.Close()

	// A footer whose payload length does not match the file must be rejected.
	data[len(data)-FooterSize+16] = 99
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("mismatched payload length accepted")
	}
}

func TestContainerVerifyDetectsCorruption(t *testing.T) {
	m := testManifest("corrupt", false)
	m.Packages = []ManifestPackage{{Name: "x", Tag: "v1", Version: "1", Asset: "a", Bins: []string{"x"}}}
	path := buildTestFile(t, t.TempDir(), "base", Payload{
		Manifest: m,
		Lists:    map[string][]byte{},
		Bins:     nil,
	})
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Verify(); err != nil {
		t.Fatalf("fresh bundle failed verification: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[int(c.Footer.PayloadOffset)] ^= 0xff
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Verify(); err == nil {
		t.Fatal("corrupted payload passed verification")
	}
}

func crc32c(data []byte) uint32 {
	return crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
}

func hashFor(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
