package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/files"
)

// TestEventsArriveImmediately is the regression test for the gzip trap.
// gzhttp buffers until DefaultMinSize (1 KiB) before deciding whether to
// compress; an SSE frame is typically smaller, so without an explicit
// text/event-stream exception the first event sits in the buffer and never
// reaches the browser. The stream looks connected and delivers nothing.
func TestEventsArriveImmediately(t *testing.T) {
	s := newTestServer(t)
	// Populate a region so there is a snapshot to deliver on connect.
	s.renderStats()

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

// TestIdleServerIsQuiet covers the change detection end to end. Suppressing
// unchanged regions is what keeps an idle server quiet; losing it means
// streaming to every connected browser forever, once per poll tick.
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
	// legitimate one-time event.
	s.renderStats()
	s.ui.RenderTorrents(s.engine.GetTorrents())
	s.ui.RenderDownloads(files.List(s.downloadDir()))

	go s.pollLoop(ctx)
	go s.statsLoop(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Names accumulate under a lock so the main goroutine can watch the stream
	// settle rather than guess how long settling takes.
	var mu sync.Mutex
	var names []string
	go func() {
		r := bufio.NewReader(resp.Body)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				mu.Lock()
				names = append(names, strings.TrimSpace(name))
				mu.Unlock()
			}
		}
	}()

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(names)
	}

	// Wait for the stream to go quiet, instead of assuming it does so within a
	// fixed settle. The initial snapshot and the kick a new connection triggers
	// both land promptly, but "promptly" on a loaded CI runner is not bounded by
	// any constant — and the previous version's 1s guess meant a slow startup
	// frame was counted as steady traffic, failing the test with a message about
	// change detection being broken. Observing quiescence removes the guess.
	const quietFor = 750 * time.Millisecond
	settleDeadline := time.Now().Add(15 * time.Second)
	last := count()
	quietSince := time.Now()
	for {
		time.Sleep(50 * time.Millisecond)
		if n := count(); n != last {
			last, quietSince = n, time.Now()
		}
		if time.Since(quietSince) >= quietFor {
			break
		}
		if time.Now().After(settleDeadline) {
			t.Fatalf("stream never went quiet: %d events and still arriving; "+
				"change detection is not suppressing unchanged regions", count())
		}
	}

	// Steady state starts here, by observation rather than by clock arithmetic.
	before := count()
	window := 3 * time.Second
	time.Sleep(window)
	steady := count() - before

	mu.Lock()
	steadyNames := append([]string(nil), names[min(before, len(names)):]...)
	all := append([]string(nil), names...)
	mu.Unlock()

	cancel()
	resp.Body.Close()

	// What remains should only be the stats sample, whose heap and goroutine
	// numbers legitimately move every statsInterval. One frame per poll tick
	// would mean suppression is broken.
	maxSteady := int(window/statsInterval) + 1
	if steady > maxSteady {
		t.Errorf("%d events in the %s after settling (max %d): "+
			"change detection is not suppressing unchanged regions", steady, window, maxSteady)
	}
	pollTicks := int(window / pollInterval)
	if steady >= pollTicks {
		t.Errorf("steady traffic (%d) reached the poll tick count (%d): "+
			"regions are being pushed every tick regardless of change", steady, pollTicks)
	}
	t.Logf("idle traffic: %d total, %d steady in %s — steady sequence: %v",
		len(all), steady, window, steadyNames)
}
