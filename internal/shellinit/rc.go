package shellinit

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf16"

	"github.com/SuSonicTH/cpt/internal/fsutil"
)

// EditRCFile adds or removes the cpt marker block in a shell startup file. It
// preserves the file mode and encoding and follows a symlinked dotfile instead
// of replacing the link. It reports whether the file changed.
func EditRCFile(path string, install bool, block string) (bool, error) {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	current, encoding, err := decodeRC(data)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	next := RemoveBlock(current)
	if install {
		next = AddBlock(current, block)
	}
	if next == current {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := fsutil.WriteFileAtomic(path, encodeRC(next, encoding), mode); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

type rcEncoding int

const (
	rcUTF8 rcEncoding = iota
	rcUTF8BOM
	rcUTF16LE
	rcUTF16BE
)

// decodeRC understands the UTF-16 files Windows PowerShell frequently uses for
// profiles, so editing one does not corrupt it.
func decodeRC(data []byte) (string, rcEncoding, error) {
	switch {
	case len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF:
		return string(data[3:]), rcUTF8BOM, nil
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE:
		s, err := decodeUTF16(data[2:], binary.LittleEndian)
		return s, rcUTF16LE, err
	case len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF:
		s, err := decodeUTF16(data[2:], binary.BigEndian)
		return s, rcUTF16BE, err
	default:
		return string(data), rcUTF8, nil
	}
}

func decodeUTF16(data []byte, order binary.ByteOrder) (string, error) {
	if len(data)%2 != 0 {
		return "", fmt.Errorf("truncated UTF-16 data")
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = order.Uint16(data[i*2:])
	}
	return string(utf16.Decode(units)), nil
}

func encodeRC(s string, encoding rcEncoding) []byte {
	switch encoding {
	case rcUTF8BOM:
		return append([]byte{0xEF, 0xBB, 0xBF}, []byte(s)...)
	case rcUTF16LE:
		return append([]byte{0xFF, 0xFE}, encodeUTF16(s, binary.LittleEndian)...)
	case rcUTF16BE:
		return append([]byte{0xFE, 0xFF}, encodeUTF16(s, binary.BigEndian)...)
	default:
		return []byte(s)
	}
}

func encodeUTF16(s string, order binary.ByteOrder) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, len(units)*2)
	for i, u := range units {
		order.PutUint16(out[i*2:], u)
	}
	return out
}
