package web

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
	// writeTimeout bounds a single frame write to one subscriber.
	writeTimeout = 15 * time.Second
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
	// closed latches on shutdown. Without it a request arriving between
	// hub.close and srv.Shutdown subscribes to a hub nobody will ever release,
	// pinning Shutdown open for its whole budget.
	closed bool
}

func newHub() *hub { return &hub{subs: map[*subscriber]struct{}{}} }

func (h *hub) subscribe() *subscriber {
	s := &subscriber{ch: make(chan []byte, subBuffer), done: make(chan struct{})}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		s.stall() // serveEvents returns immediately
		return s
	}
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// close releases every subscriber and latches the hub shut. Idempotent and
// one-way: a Server is not restartable.
//
// It reuses the stall mechanism but means something different. A stall expects
// the subscriber back — EventSource reconnects and replays the snapshot, which
// is what makes dropping one self-correcting. Here the server is going away, so
// nothing self-corrects, and this also *evicts*: the reader may already be gone,
// leaving nobody to call unsubscribe.
//
// No farewell event telling the page to stop retrying: htmx owns the
// EventSource, so closing it from page code means driving unexported internals
// (see static/CLAUDE.md), whose failure mode is a permanently dead UI.
func (h *hub) close() {
	h.mu.Lock()
	h.closed = true
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	clear(h.subs)
	h.mu.Unlock()

	// Outside the lock: stall only closes a channel, but keeping the critical
	// section narrow keeps broadcast's "never blocks" contract obviously intact.
	for _, s := range subs {
		s.stall()
	}
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

// ServeEvents streams rendered fragments as named SSE events.
func (u *UI) ServeEvents(w http.ResponseWriter, r *http.Request) {
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

	// A per-write deadline, because request-context cancellation does not unblock
	// a blocked Write: without it this goroutine can park forever on a client
	// whose TCP send buffer is full, leaving the subscriber unreleased and
	// watchers() above zero — so the poll loop keeps walking the download
	// directory once a second for a browser that is gone.
	//
	// Set per write rather than server-wide: this stream and large downloads are
	// both legitimately long-lived.
	rc := http.NewResponseController(w)

	write := func(b []byte) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			// Not supported by this ResponseWriter; proceed without one.
			_ = err
		}
		if _, err := w.Write(b); err != nil {
			return false
		}
		return true
	}

	sub := u.hub.subscribe()
	defer u.hub.unsubscribe(sub)

	// A client connecting mid-tick must see current state immediately rather
	// than wait for something to change. One write: the membership skeleton
	// leads, so the elements exist before the per-torrent regions that target
	// them arrive.
	if snap := u.renderer.snapshot(torrentListEvent); len(snap) > 0 {
		if !write(snap) {
			return
		}
	}
	flusher.Flush()

	// Waking the render loop makes the very first connection populate the
	// regions, and refreshes ConnectedUsers for everyone already watching.
	u.kick()

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
			if !write(frame) {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			// A comment line: valid SSE, ignored by EventSource.
			if !write([]byte(": keepalive\n\n")) {
				return
			}
			flusher.Flush()
		}
	}
}
