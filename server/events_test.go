package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

	// Names are collected with a timestamp so the settling period can be
	// discarded after the fact. Measuring steady state directly, rather than
	// subtracting an expected snapshot size, means the bound does not depend on
	// how many regions happen to exist.
	type stamped struct {
		name string
		at   time.Time
	}
	events := make(chan []stamped, 1)
	go func() {
		r := bufio.NewReader(resp.Body)
		var got []stamped
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				events <- got
				return
			}
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				got = append(got, stamped{strings.TrimSpace(name), time.Now()})
			}
		}
	}()

	// The initial snapshot and the kick that follows a new connection both land
	// immediately; everything after this instant is steady-state traffic.
	const settle = 1 * time.Second
	window := 3 * time.Second
	start := time.Now()
	time.Sleep(settle + window)
	cancel()
	resp.Body.Close()

	all := <-events
	var steadyNames []string
	for _, e := range all {
		if e.at.After(start.Add(settle)) {
			steadyNames = append(steadyNames, e.name)
		}
	}
	steady := len(steadyNames)

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
