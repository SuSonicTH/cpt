package progress

import (
	"testing"
	"time"
)

func TestBusPublishesToSubscribers(t *testing.T) {
	bus := New()
	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()

	bus.Publish(Event{Kind: KindQueued, Package: "fzf"})
	bus.Publish(Event{Kind: KindDone, Package: "fzf", Version: "1.0.0", Action: "installed"})

	for _, want := range []Kind{KindQueued, KindDone} {
		select {
		case got := <-ch:
			if got.Kind != want {
				t.Fatalf("got %q, want %q", got.Kind, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

func TestBusUnsubscribeStopsDelivery(t *testing.T) {
	bus := New()
	ch, unsubscribe := bus.Subscribe()
	unsubscribe()
	bus.Publish(Event{Kind: KindQueued, Package: "fzf"})
	select {
	case got := <-ch:
		t.Fatalf("received event after unsubscribe: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestBusKeepsTerminalEventsWhenFull(t *testing.T) {
	bus := New()
	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()

	for i := 0; i < cap(ch)+5; i++ {
		bus.Publish(Event{Kind: KindDownloading, Package: "big", Done: int64(i), Total: 1000})
	}
	bus.Publish(Event{Kind: KindDone, Package: "big", Action: "installed"})

	deadline := time.After(time.Second)
	for {
		select {
		case got := <-ch:
			if got.Kind == KindDone {
				return
			}
		case <-deadline:
			t.Fatal("terminal event was dropped")
		}
	}
}

func TestNilBusIsSafe(t *testing.T) {
	var bus *Bus
	bus.Publish(Event{Kind: KindQueued})
	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()
	bus.Close()
	if ch != nil {
		t.Fatalf("nil bus returned a channel")
	}
}
