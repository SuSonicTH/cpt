// Package fetch downloads assets and verifies their checksums.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/SuSonicTH/cpt/internal/httpc"
)

// MaxAssetSize bounds a single download.
const MaxAssetSize = 1 << 30 // 1 GiB

// Downloader fetches assets to disk with retries.
type Downloader struct {
	Client   *http.Client
	Attempts int
	// Progress is called with the number of bytes received so far and the
	// expected total (-1 when unknown).
	Progress func(label string, done, total int64)
}

// To downloads url into dest and returns the SHA256 of the content.
func (d *Downloader) To(ctx context.Context, url, dest, label string) (string, int64, error) {
	attempts := d.Attempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		hash, size, err := d.once(ctx, url, dest, label)
		if err == nil {
			return hash, size, nil
		}
		if errors.Is(err, httpc.ErrNotFound) || ctx.Err() != nil {
			return "", 0, err
		}
		lastErr = err
		if attempt < attempts {
			select {
			case <-ctx.Done():
				return "", 0, ctx.Err()
			case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
			}
		}
	}
	return "", 0, lastErr
}

func (d *Downloader) once(ctx context.Context, url, dest, label string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", 0, fmt.Errorf("%s: %w", url, httpc.ErrNotFound)
	default:
		return "", 0, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	if resp.ContentLength > MaxAssetSize {
		return "", 0, fmt.Errorf("%s: asset too large (%d bytes)", url, resp.ContentLength)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", 0, err
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		f.Close()
		os.Remove(tmp)
	}()

	h := sha256.New()
	written := int64(0)
	buf := make([]byte, 128*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return "", 0, err
			}
			h.Write(buf[:n])
			written += int64(n)
			if written > MaxAssetSize {
				return "", 0, fmt.Errorf("%s: asset exceeded %d bytes", url, MaxAssetSize)
			}
			if d.Progress != nil {
				d.Progress(label, written, resp.ContentLength)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", 0, readErr
		}
	}
	if err := f.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), written, nil
}
