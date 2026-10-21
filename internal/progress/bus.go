// Package progress defines the event stream shared by the engine, the CLI and
// the interactive TUI.
package progress

import "sync"

// Kind is the type of progress event.
type Kind string

const (
	// KindQueued means a package has been accepted for an operation.
	KindQueued Kind = "queued"
	// KindResolving means cpt is resolving a version or asset.
	KindResolving Kind = "resolving"
	// KindDownloading carries byte progress for an asset.
	KindDownloading Kind = "downloading"
	// KindVerifying means checksums are being fetched and compared.
	KindVerifying Kind = "verifying"
	// KindExtracting means the archive is being unpacked and scanned.
	KindExtracting Kind = "extracting"
	// KindInstalling means binaries are being placed into the bin directory.
	KindInstalling Kind = "installing"
	// KindRemoving means a package's binaries are being deleted.
	KindRemoving Kind = "removing"
	// KindDone marks a package operation that completed successfully.
	KindDone Kind = "done"
	// KindFailed marks a package operation that failed.
	KindFailed Kind = "failed"
	// KindInfo carries a free-form notice, usually a warning meant for the
	// interactive log.
	KindInfo Kind = "info"
)

// Event is one progress update. Package is empty for free-form messages.
type Event struct {
	Kind    Kind
	Package string
	Version string
	Action  string
	Message string
	Done    int64
	Total   int64
}

// Bus fans events out to any number of subscribers. Publishing never blocks:
// a subscriber that cannot keep up loses intermediate events, while terminal
// events (done and failed) evict an older queued event so they are not lost.
type Bus struct {
	mu     sync.Mutex
	subs   map[chan Event]struct{}
	closed bool
}

// New returns an empty bus.
func New() *Bus { return &Bus{subs: map[chan Event]struct{}{}} }

// Subscribe returns a channel of events and a function that unsubscribes. The
// channel is never closed; callers should stop reading after unsubscribing.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	if b == nil {
		return nil, func() {}
	}
	ch := make(chan Event, 512)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Publish sends e to every subscriber.
func (b *Bus) Publish(e Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	subs := make([]chan Event, 0, len(b.subs))
	for ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()
	for _, ch := range subs {
		deliver(ch, e)
	}
}

// Close releases every subscriber. The bus must not be reused afterwards.
func (b *Bus) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.closed = true
	b.subs = map[chan Event]struct{}{}
	b.mu.Unlock()
}

func deliver(ch chan Event, e Event) {
	select {
	case ch <- e:
		return
	default:
	}
	switch e.Kind {
	case KindDone, KindFailed:
		// Make room by dropping the oldest intermediate event.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- e:
		default:
		}
	}
}
