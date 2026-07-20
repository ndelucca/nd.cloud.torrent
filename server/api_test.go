package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFailedActionStillKicks covers a partially applied action being invisible.
//
// The render loop was woken only when the API call returned nil. But an action
// can apply partially and still report an error — uploading five torrents where
// two are malformed adds three and returns 400 — so those three stayed off the
// page until the next tick. kick is coalesced and floored, so waking it
// unconditionally costs at most one extra render.
func TestFailedActionStillKicks(t *testing.T) {
	s := newTestServer(t)
	// Drain anything startup left pending, so the assertion is about this call.
	select {
	case <-s.render.kickCh:
	default:
	}

	req := httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader("not-a-magnet"))
	req.Header.Set("Origin", "http://"+req.Host)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code < 400 {
		t.Fatalf("setup: expected the action to fail, got %d", rec.Code)
	}
	select {
	case <-s.render.kickCh:
	default:
		t.Fatal("a failed action left the render loop asleep; a partial success " +
			"would stay invisible until the next tick")
	}
}

// TestTorrentVerbsRoundTrip covers POST /api/torrents/{hash}/stop, which was the
// only torrent verb with no end-to-end test — handleStart and handleDelete were
// both fully covered while handleStop sat at zero.
//
// It matters more than the other two, not less. Stopping is destructive:
// StopTorrent drops the underlying torrent and keeps only the spec, and
// StartTorrent has to re-add from that spec. So this drives the whole cycle
// rather than just the verb — a route table that pointed /stop at the wrong
// handler, or a start-after-stop that flipped the flag without re-adding, both
// ship green without it.
func TestTorrentVerbsRoundTrip(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	// AutoStart off *before* adding, so the torrent's state is driven only by
	// the verbs below. With it on, the metadata watcher starts the torrent
	// asynchronously — GotInfo is already closed for a .torrent file — and can
	// land between a check and the next request, answering 409 to a start this
	// test just established was needed.
	//
	// It is also the one setting that applies without a restart, which is why
	// this works at all.
	cfg := s.engine.Config()
	cfg.AutoStart = false
	if err := s.engine.Configure(cfg); err != nil {
		t.Fatalf("disabling AutoStart: %v", err)
	}

	if err := s.engine.NewTorrentFile(testTorrentFile(t, "payload.bin")); err != nil {
		t.Fatalf("NewTorrentFile: %v", err)
	}
	var hash string
	for ih := range s.engine.GetTorrents() {
		hash = ih
	}
	if hash == "" {
		t.Fatal("setup: no torrent was added")
	}

	post := func(t *testing.T, verb string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/torrents/"+hash+"/"+verb, nil)
		r.Header.Set("Origin", "http://"+r.Host)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	started := func(t *testing.T) bool {
		t.Helper()
		tor, ok := s.engine.GetTorrents()[hash]
		if !ok {
			t.Fatal("the torrent vanished from the engine")
		}
		return tor.Started
	}

	if started(t) {
		t.Fatal("setup: the torrent is running with AutoStart off")
	}

	if w := post(t, "start"); w.Code != http.StatusOK {
		t.Fatalf("start: status %d (%q)", w.Code, w.Body.String())
	}
	if !started(t) {
		t.Fatal("start returned 200 but the torrent is not running")
	}

	if w := post(t, "stop"); w.Code != http.StatusOK {
		t.Fatalf("stop: status %d (%q)", w.Code, w.Body.String())
	}
	if started(t) {
		t.Fatal("stop returned 200 but the torrent is still running")
	}

	// Stopping dropped the underlying torrent, so this is the path that has to
	// re-add it from the retained spec. Without that, Started flips back to true
	// and nothing downloads — visible only here.
	if w := post(t, "start"); w.Code != http.StatusOK {
		t.Fatalf("start after stop: status %d (%q)", w.Code, w.Body.String())
	}
	if !started(t) {
		t.Fatal("start-after-stop returned 200 but the torrent is not running")
	}

	// Each verb is a state transition, so repeating one is a conflict rather
	// than a no-op.
	if w := post(t, "start"); w.Code != http.StatusConflict {
		t.Errorf("starting an already-started torrent = %d, want 409", w.Code)
	}
}
