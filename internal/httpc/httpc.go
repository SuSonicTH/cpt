// Package httpc builds HTTP clients and performs small retrying requests.
package httpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SuSonicTH/cpt/internal/cerr"
	"github.com/SuSonicTH/cpt/internal/config"
)

// ErrNotFound reports a 404 response.
var ErrNotFound = errors.New("not found")

// userAgentTransport stamps a User-Agent on requests that do not set one.
type userAgentTransport struct {
	base http.RoundTripper
	ua   string
}

func (t userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" && t.ua != "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", t.ua)
	}
	return t.base.RoundTrip(req)
}

func transport(cfg config.HTTP) http.RoundTripper {
	base := http.DefaultTransport
	return userAgentTransport{base: base, ua: cfg.UserAgent}
}

// New returns a client that follows redirects, with the configured timeout.
func New(cfg config.HTTP) *http.Client {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport(cfg),
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

// Get fetches url and returns the body, limited to limit bytes. Missing
// resources return ErrNotFound. Transient failures are retried attempts times,
// honouring Retry-After.
func Get(ctx context.Context, client *http.Client, url string, limit int64, header http.Header, attempts int) ([]byte, http.Header, error) {
	attempts = max(1, attempts)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = cerr.Wrap(cerr.Network, fmt.Errorf("%s: %w", url, err))
			if attempt == attempts || ctx.Err() != nil {
				return nil, nil, lastErr
			}
			if err := sleepCtx(ctx, backoff(nil, attempt)); err != nil {
				return nil, nil, lastErr
			}
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
			data, err := readLimited(resp.Body, limit)
			resp.Body.Close()
			if err != nil {
				return nil, nil, err
			}
			return data, resp.Header, nil
		case resp.StatusCode == http.StatusNotModified:
			resp.Body.Close()
			return nil, resp.Header, nil
		case resp.StatusCode == http.StatusNotFound:
			drain(resp.Body)
			return nil, resp.Header, fmt.Errorf("%s: %w", url, ErrNotFound)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			delay := backoff(resp, attempt)
			code := resp.StatusCode
			drain(resp.Body)
			if attempt == attempts {
				return nil, nil, cerr.New(cerr.Network, "%s: HTTP %d after %d attempts", url, code, attempts)
			}
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, nil, cerr.New(cerr.Network, "%s: HTTP %d", url, code)
			}
			continue
		default:
			body := readSmall(resp.Body)
			return nil, nil, cerr.New(cerr.Network, "%s: HTTP %d: %s", url, resp.StatusCode, body)
		}
	}
	return nil, nil, lastErr
}

// Head issues a HEAD request without following redirects.
func Head(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	return HeadRetry(ctx, client, url, 1)
}

// HeadRetry issues a HEAD request, retrying transient failures.
func HeadRetry(ctx context.Context, client *http.Client, url string, attempts int) (*http.Response, error) {
	attempts = max(1, attempts)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = cerr.Wrap(cerr.Network, fmt.Errorf("%s: %w", url, err))
			if attempt == attempts || ctx.Err() != nil {
				return nil, lastErr
			}
			if err := sleepCtx(ctx, backoff(nil, attempt)); err != nil {
				return nil, lastErr
			}
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			delay := backoff(resp, attempt)
			code := resp.StatusCode
			resp.Body.Close()
			lastErr = cerr.New(cerr.Network, "%s: HTTP %d", url, code)
			if attempt == attempts {
				return nil, lastErr
			}
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, lastErr
			}
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response larger than %d bytes", limit)
	}
	return data, nil
}

func readSmall(r io.Reader) string {
	body, _ := io.ReadAll(io.LimitReader(r, 512))
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func drain(r io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 1<<16))
}

// backoff returns how long to wait before the next attempt. A Retry-After
// header (seconds or HTTP date) wins over exponential-ish backoff.
func backoff(resp *http.Response, attempt int) time.Duration {
	if resp != nil {
		if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
				return min(time.Duration(secs)*time.Second, 30*time.Second)
			}
			if t, err := http.ParseTime(v); err == nil {
				if d := time.Until(t); d > 0 {
					return min(d, 30*time.Second)
				}
			}
		}
	}
	d := time.Duration(attempt) * 400 * time.Millisecond
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
