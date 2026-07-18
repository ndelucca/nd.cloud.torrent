package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/files"
)

// TestFrameSSE pins the framing. Rendered HTML is full of newlines, and every
// line of an SSE payload needs its own "data: " prefix — writing a fragment as
// a single data line silently truncates it at the first newline.
func TestFrameSSE(t *testing.T) {
	got := string(frameSSE("stats", []byte("<div>\n  <span>hi</span>\n</div>")))
	want := "event: stats\ndata: <div>\ndata:   <span>hi</span>\ndata: </div>\n\n"
	if got != want {
		t.Errorf("frameSSE:\ngot  %q\nwant %q", got, want)
	}

	// CRLF must normalise, or the stray \r ends up inside the payload.
	if got := string(frameSSE("x", []byte("a\r\nb"))); got != "event: x\ndata: a\ndata: b\n\n" {
		t.Errorf("CRLF not normalised: %q", got)
	}

	// An empty body must still deliver the event — that is what lets htmx's
	// SSE extension collect a listener for a removed element.
	if got := string(frameSSE("torrent-abc", nil)); got != "event: torrent-abc\ndata:\n\n" {
		t.Errorf("empty frame = %q", got)
	}
}

// TestRendererChangeDetection covers the suppression that replaces velox's
// merge-patch diffing. Without it an idle server streams to every browser
// forever.
func TestRendererChangeDetection(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	r := newRenderer(tmpl)

	view := statsView{Version: "1.0", System: SystemStats{Set: true, GoRoutines: 7}}

	first, err := r.render("stats", "stats", view)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("first render must produce a frame")
	}
	if !bytes.Contains(first, []byte("event: stats")) {
		t.Errorf("frame missing event name: %s", first)
	}

	again, err := r.render("stats", "stats", view)
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Errorf("identical render must be suppressed, got %s", again)
	}

	view.System.GoRoutines = 8
	changed, err := r.render("stats", "stats", view)
	if err != nil {
		t.Fatal(err)
	}
	if changed == nil {
		t.Error("changed data must produce a frame")
	}
}

// TestFragmentsAreWrappedInElements guards a silent failure verified in
// Chromium 150: idiomorph swaps a bare-text payload as EMPTY. No error is
// raised anywhere — the data arrives and the DOM goes blank — so the check has
// to happen at render time.
//
// This also runs over every shipped template, so adding one that emits leading
// text fails here rather than in a browser.
func TestFragmentsAreWrappedInElements(t *testing.T) {
	if err := checkFragment("x", []byte("  <div>ok</div>")); err != nil {
		t.Errorf("element fragment rejected: %v", err)
	}
	if err := checkFragment("x", nil); err != nil {
		t.Errorf("empty fragment rejected: %v", err)
	}
	err := checkFragment("x", []byte("bare text"))
	if !errors.Is(err, errBareText) {
		t.Errorf("bare text = %v, want errBareText", err)
	}

	// Every fragment, with data it can actually render. The previous version
	// executed them all with statsView{} and continue'd on error — which was
	// almost all of them — so it really only covered the two that happen to
	// tolerate a wrong type, while claiming to cover every template.
	tmpl, perr := parseTemplates()
	if perr != nil {
		t.Fatal(perr)
	}
	fragments := []struct {
		name string
		data any
	}{
		{"stats", statsView{System: SystemStats{Set: true}}},
		{"api-ok", "Done."},
		{"api-error", "Nope."},
		{"torrent-list", []torrentView{{InfoHash: "abc", Name: "N", Loaded: true}}},
		{"torrent-row", torrentView{InfoHash: "abc", Name: "N", Loaded: true, Started: true}},
		{"torrent-files", torrentView{InfoHash: "abc", Files: []*engine.File{{Path: "a/b.mkv", Size: 1}}}},
		{"omni", nil},
		{"config", engine.Config{DownloadDirectory: "/d", IncomingPort: 1}},
		{"downloads", struct {
			Root      fsView
			Truncated bool
			Limit     int
		}{Root: newRootView(&files.Node{Name: "d", IsDir: true}), Limit: 10}},
	}
	for _, f := range fragments {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, f.name, f.data); err != nil {
			t.Errorf("%s: render failed: %v", f.name, err)
			continue
		}
		if err := checkFragment(f.name, buf.Bytes()); err != nil {
			t.Errorf("%v", err)
		}
		if strings.Contains(buf.String(), "ZgotmplZ") {
			t.Errorf("%s: ZgotmplZ in output — a value reached a URL or CSS "+
				"context the autoescaper could not prove safe", f.name)
		}
	}
}

// TestRendererForget covers the htmx SSE listener leak. The extension
// unregisters a per-element listener lazily, from inside the listener itself.
// If the server just stops emitting an event name after a torrent is deleted,
// that listener never runs again and retains the detached DOM subtree forever.
func TestRendererForget(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	r := newRenderer(tmpl)
	r.store("torrent-abc", []byte("<div>x</div>"))

	if len(r.snapshot()) != 1 {
		t.Fatal("expected the region in the snapshot")
	}
	frame := r.forget("torrent-abc")
	if string(frame) != "event: torrent-abc\ndata:\n\n" {
		t.Errorf("forget frame = %q, want an empty data event", frame)
	}
	if len(r.snapshot()) != 0 {
		t.Error("forgotten region must leave the snapshot")
	}
}

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

// TestEventsArriveImmediately is the regression test for the gzip trap.
// gzhttp buffers until DefaultMinSize (1 KiB) before deciding whether to
// compress; an SSE frame is typically smaller, so without an explicit
// text/event-stream exception the first event sits in the buffer and never
// reaches the browser. The stream looks connected and delivers nothing.
func TestEventsArriveImmediately(t *testing.T) {
	s := newTestServer(t)
	// Populate a region so there is a snapshot to deliver on connect.
	s.renderRegions()

	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A real EventSource advertises gzip, which is exactly what triggers the bug.
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q: the SSE stream must not be compressed", enc)
	}

	type read struct {
		line string
		err  error
	}
	lines := make(chan read, 1)
	go func() {
		r := bufio.NewReader(resp.Body)
		l, err := r.ReadString('\n')
		lines <- read{l, err}
	}()

	select {
	case got := <-lines:
		if got.err != nil {
			t.Fatalf("reading first line: %v", got.err)
		}
		if !strings.HasPrefix(got.line, "event: ") {
			t.Errorf("first line = %q, want an event line", got.line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no bytes within 2s: the SSE stream is being buffered " +
			"(check that gzip excepts text/event-stream)")
	}
}

// TestIdleServerIsQuiet covers the change detection end to end. velox suppressed
// pushes whose JSON merge patch was empty; that suppression is now ours to
// maintain, and losing it means streaming to every connected browser forever.
//
// The bound is not zero: the stats sample legitimately changes every
// statsInterval because heap size and goroutine count move. What must not
// happen is a frame every pollInterval.
func TestIdleServerIsQuiet(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-based")
	}
	s := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	// Warm the regions before measuring. Each region's very first render is a
	// legitimate one-time event; counting those as steady traffic would make
	// the bound depend on how many regions exist rather than on whether
	// suppression works.
	s.renderRegions()
	s.renderTorrents(s.engine.GetTorrents())
	s.renderDownloads(files.List(s.downloadDir()))

	go s.pollLoop(ctx)
	go s.statsLoop(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Measured before connecting: regions created later are live events, not
	// part of this client's snapshot.
	snapshot := len(s.renderer.snapshot())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Drain the initial snapshot and whatever the first kick produces.
	window := 3 * time.Second
	events := make(chan []string, 1)
	go func() {
		r := bufio.NewReader(resp.Body)
		var names []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				events <- names
				return
			}
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				names = append(names, strings.TrimSpace(name))
			}
		}
	}()

	time.Sleep(window)
	cancel()
	resp.Body.Close()

	names := <-events
	n := len(names)
	// The initial snapshot delivers one event per region and is not idle
	// traffic, so discount it. Deriving it from the renderer keeps the bound
	// correct as regions are added rather than needing a new magic number each
	// time.
	steady := n - snapshot

	// What remains should only be the stats sample, whose heap and goroutine
	// numbers legitimately move every statsInterval. One frame per poll tick
	// would mean suppression is broken.
	maxSteady := int(window/statsInterval) + 1
	if steady > maxSteady {
		t.Errorf("%d events in %s after a %d-event snapshot (max %d steady): "+
			"change detection is not suppressing unchanged regions",
			n, window, snapshot, maxSteady)
	}
	pollTicks := int(window / pollInterval)
	if steady >= pollTicks {
		t.Errorf("steady traffic (%d) reached the poll tick count (%d): "+
			"regions are being pushed every tick regardless of change", steady, pollTicks)
	}
	t.Logf("idle traffic in %s: %d total, %d snapshot, %d steady — sequence: %v", window, n, snapshot, steady, names)
}
