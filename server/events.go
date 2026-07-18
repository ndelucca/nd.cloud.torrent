package server

import (
	"net/http"
	"sync"
	"time"
)

const (
	// subBuffer is how many frames may queue for one browser before it is
	// considered stalled. Small on purpose: see hub.broadcast.
	subBuffer = 8
	// keepaliveInterval keeps intermediaries from closing an idle stream. An
	// idle server emits no events at all, so without this a proxy may drop the
	// connection after its read timeout.
	keepaliveInterval = 25 * time.Second
)

// subscriber is one connected browser.
type subscriber struct {
	ch   chan []byte
	done chan struct{}
	once sync.Once
}

// stall marks the subscriber unservable. It is idempotent because both the
// broadcaster and the request goroutine can reach it.
func (s *subscriber) stall() { s.once.Do(func() { close(s.done) }) }

// hub fans rendered frames out to every connected browser.
type hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

func newHub() *hub { return &hub{subs: map[*subscriber]struct{}{}} }

func (h *hub) subscribe() *subscriber {
	s := &subscriber{ch: make(chan []byte, subBuffer), done: make(chan struct{})}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
	s.stall()
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// broadcast queues a frame for every subscriber. It never blocks: one stalled
// TCP client must not freeze the render loop and with it every other browser's
// UI.
//
// A subscriber whose buffer is full is disconnected rather than having the
// frame dropped. Frames carry only what *changed*, so a dropped frame would
// leave that browser permanently stale with no way to notice. Disconnecting is
// self-correcting: EventSource reconnects on its own and replays the full
// snapshot.
func (h *hub) broadcast(frame []byte) {
	if len(frame) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.ch <- frame:
		default:
			s.stall()
		}
	}
}

// serveEvents streams rendered fragments as named SSE events.
func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	head := w.Header()
	head.Set("Content-Type", "text/event-stream")
	head.Set("Cache-Control", "no-cache")
	head.Set("Connection", "keep-alive")
	// Ask reverse proxies not to buffer; nginx in particular will otherwise
	// hold the stream until its buffer fills.
	head.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub := s.hub.subscribe()
	defer s.hub.unsubscribe(sub)

	// A client connecting mid-tick must see current state immediately rather
	// than wait for something to change.
	for _, frame := range s.renderer.snapshot() {
		if _, err := w.Write(frame); err != nil {
			return
		}
	}
	flusher.Flush()

	// Waking the render loop makes the very first connection populate the
	// regions, and refreshes ConnectedUsers for everyone already watching.
	s.kick()

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.done:
			return
		case frame := <-sub.ch:
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			// A comment line: valid SSE, ignored by EventSource.
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
