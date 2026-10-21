// Package httpc builds HTTP clients and performs small retrying requests.
package httpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/SuSonicTH/cpt/internal/config"
)

// ErrNotFound reports a 404 response.
var ErrNotFound = errors.New("not found")

// New returns a client that follows redirects, with the configured timeout.
func New(cfg config.HTTP) *http.Client {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
}

// NewNoRedirect returns a client that reports redirects to the caller.
func NewNoRedirect(cfg config.HTTP) *http.Client {
	c := New(cfg)
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return c
}

// Get fetches url and returns the body, limited to limit bytes.
// Missing resources return ErrNotFound.
func Get(ctx context.Context, client *http.Client, url string, limit int64, header http.Header) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return nil, resp.Header, nil
	case http.StatusNotFound:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, resp.Header, fmt.Errorf("%s: %w", url, ErrNotFound)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, resp.Header, fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, trimBody(body))
	}
	if limit > 0 {
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, nil, err
		}
		if int64(len(data)) > limit {
			return nil, nil, fmt.Errorf("%s: response larger than %d bytes", url, limit)
		}
		return data, resp.Header, nil
	}
	data, err := io.ReadAll(resp.Body)
	return data, resp.Header, err
}

// Head issues a HEAD request without following redirects.
func Head(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func trimBody(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
