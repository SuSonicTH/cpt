package httpc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type trackingBody struct {
	io.Reader
	closed *bool
}

func (b *trackingBody) Close() error {
	*b.closed = true
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGetClosesBodiesOnEveryPath(t *testing.T) {
	statuses := []int{
		http.StatusNotFound,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	}
	for _, status := range statuses {
		closed := false
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Retry-After": {"0"}},
				Body:       &trackingBody{Reader: strings.NewReader("x"), closed: &closed},
			}, nil
		})}
		_, _, _ = Get(context.Background(), client, "https://example.test/x", 16, nil, 1)
		if !closed {
			t.Errorf("status %d: response body was not closed", status)
		}
	}
}

func TestProbeAssetAcceptsAny2xx(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodHead {
			return &http.Response{StatusCode: http.StatusMethodNotAllowed, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("hello")), Header: make(http.Header)}, nil
	})}
	code, err := ProbeAsset(context.Background(), client, "https://example.test/x", 1)
	if err != nil || code != http.StatusOK {
		t.Fatalf("ProbeAsset = %d, %v", code, err)
	}
}
