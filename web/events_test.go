package web

import (
	"sync"
	"testing"
	"time"
)

// TestHubDropsStalledSubscriber covers the backpressure requirement: one
// stalled TCP client must not block the render loop and freeze every other
// browser's UI.
//
// A full buffer disconnects rather than dropping the frame. Frames carry only
// what changed, so a dropped frame would leave that browser permanently stale
// with no way to notice; disconnecting is self-correcting because EventSource
// reconnects and replays the snapshot.
func TestHubDropsStalledSubscriber(t *testing.T) {
	h := newHub()
	sub := h.subscribe()

	for i := 0; i < subBuffer+5; i++ {
		done := make(chan struct{})
		go func() { h.broadcast([]byte("event: x\ndata: y\n\n")); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("broadcast blocked on a subscriber that never reads")
		}
	}

	select {
	case <-sub.done:
	default:
		t.Error("a subscriber that never drains must be disconnected, not silently starved")
	}

	if h.count() != 1 {
		t.Errorf("count = %d, want 1 (unsubscribe is the reader's job)", h.count())
	}
}

// TestHubCloseReleasesSubscribers pins the mechanism the shutdown path relies
// on. TestRunShutsDownPromptlyWithSSEClients covers the same bug end to end;
// this one is deterministic and names each invariant separately, so a
// regression says which part broke.
func TestHubCloseReleasesSubscribers(t *testing.T) {
	h := newHub()
	sub := h.subscribe()

	h.close()

	select {
	case <-sub.done:
	default:
		t.Error("close must release every subscriber, or Shutdown waits them out")
	}

	// Unlike a stall, shutdown evicts: the reader may already be gone, so
	// nobody is left to call unsubscribe.
	if h.count() != 0 {
		t.Errorf("count = %d, want 0 after close", h.count())
	}

	// The latch. Without it a request arriving between hub.close and
	// srv.Shutdown subscribes to a hub nobody will ever release, reopening the
	// bug in a narrower window.
	late := h.subscribe()
	select {
	case <-late.done:
	default:
		t.Error("subscribe after close must return an already-released subscriber")
	}

	// And the hub must stay safe to broadcast into: the render loop can still
	// be mid-tick when shutdown lands.
	done := make(chan struct{})
	go func() { h.broadcast([]byte("event: x\ndata: y\n\n")); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked after close")
	}

	h.close() // idempotent: Run closes on both the cancellation path and a defer
}

// TestHubConcurrentSubscribers is where the remote-DoS shape now lives.
//
// The original bug was a per-connection map written from each HTTP goroutine
// without the mutex, which produced "fatal error: concurrent map writes" with
// two simultaneous clients — unrecoverable in Go. The roster is gone, but the
// hub keeps a map keyed by subscriber, so the same shape is possible here.
// Run under -race.
func TestHubConcurrentSubscribers(t *testing.T) {
	h := newHub()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			sub := h.subscribe()
			h.unsubscribe(sub)
		}()
		go func() {
			defer wg.Done()
			h.broadcast([]byte("event: x\ndata: <i>y</i>\n\n"))
		}()
		go func() {
			defer wg.Done()
			_ = h.count()
		}()
	}
	wg.Wait()
}
