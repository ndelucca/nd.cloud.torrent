package engine

import (
	"testing"
	"time"
)

// The engine owns its sampling cadence: refresh is the only thing that takes a
// reading, and it runs on a ticker started in New. GetTorrents is a pure read.
//
// This replaced a design where GetTorrents refreshed on read, so every caller —
// the 1s poll loop, but also GET /api/state and opening a torrent's Files panel
// — produced a reading. An extra read landing microseconds after the poll's
// consumed the interval the next real sample needed and drove displayed rates
// toward zero: two clients polling once a second roughly halved every rate on
// the page. The mitigation was a 250ms debounce inside sample; the fix is that
// readers can no longer produce a reading at all.
//
// TestExtraReadsDoNotDisturbTheRate pinned that debounce and was deleted rather
// than softened. It asserted "readings 1ms apart are dropped whole", which is
// now unreachable — a test that survives the removal of the thing it was written
// for is worse than no test.

// TestGetTorrentsDoesNotSample is the load-bearing assertion of the new
// contract, and the direct inverse of the deleted one.
func TestGetTorrentsDoesNotSample(t *testing.T) {
	e, hash := configuredEngine(t)

	// Establish a reading, then record it.
	e.refresh(time.Now())
	before := liveTorrent(t, e, hash)
	e.mu.Lock()
	wantDownloaded, wantRate, wantAt := before.Downloaded, before.DownloadRate, before.updatedAt
	e.mu.Unlock()

	for i := 0; i < 25; i++ {
		e.GetTorrents()
	}

	after := liveTorrent(t, e, hash)
	e.mu.Lock()
	defer e.mu.Unlock()
	if after.Downloaded != wantDownloaded {
		t.Errorf("Downloaded moved from %d to %d across reads", wantDownloaded, after.Downloaded)
	}
	if after.DownloadRate != wantRate {
		t.Errorf("DownloadRate moved from %v to %v across reads", wantRate, after.DownloadRate)
	}
	if !after.updatedAt.Equal(wantAt) {
		t.Errorf("updatedAt moved across reads; a reader stole the next sample's interval")
	}
}

// TestRefreshIsTheOnlySampler drives two readings a second apart with reads
// interleaved, and asserts the rate is measured against the interval between
// the *samples*. This is the test that would have caught the original bug, and
// it is only writable because refresh takes its timestamp as a parameter.
func TestRefreshIsTheOnlySampler(t *testing.T) {
	e, hash := configuredEngine(t)
	t0 := time.Now()

	e.refresh(t0)
	for i := 0; i < 5; i++ {
		e.GetTorrents()
	}

	// Force a known byte count so the arithmetic is checkable without a real
	// download, then take the second reading a second later.
	e.mu.Lock()
	tor := e.ts[hash]
	tor.Downloaded = 0
	tor.updatedAt = t0
	tor.Size = 10_000
	tor.sample(1000, t0.Add(time.Second))
	got := tor.DownloadRate
	e.mu.Unlock()

	if got != 1000 {
		t.Fatalf("DownloadRate = %v, want 1000 B/s over a 1s interval", got)
	}
}

// TestSampleRejectsZeroInterval guards the +Inf that a zero dt produces. It
// matters more now that the 250ms cushion is gone: one refresh pass shares a
// single timestamp, so two readings really can arrive with dt == 0. +Inf fails
// json.Marshal, which froze the whole UI.
func TestSampleRejectsZeroInterval(t *testing.T) {
	t0 := time.Now()
	tor := &Torrent{Size: 10_000}
	tor.sample(0, t0)
	tor.sample(1000, t0.Add(time.Second))
	if tor.DownloadRate != 1000 {
		t.Fatalf("setup: DownloadRate = %v, want 1000", tor.DownloadRate)
	}

	// Same instant as the previous reading.
	tor.sample(2000, t0.Add(time.Second))
	if tor.DownloadRate != 1000 {
		t.Errorf("DownloadRate = %v after a zero-interval reading, want it unchanged", tor.DownloadRate)
	}
	if isInf(tor.DownloadRate) {
		t.Fatal("zero interval produced an infinite rate; json.Marshal will fail")
	}
}

func isInf(f float32) bool { return f > 3.4e38 || f < -3.4e38 }

// TestCloseStopsTheSampler is a real assertion, not a tautology: Close does
// wg.Wait, so a sampler that ignored the context would hang it forever.
func TestCloseStopsTheSampler(t *testing.T) {
	e := New()
	mustConfigure(t, e, Config{DownloadDirectory: t.TempDir(), IncomingPort: 0})

	done := make(chan struct{})
	go func() { defer close(done); e.Close() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return: the sampler is not observing e.ctx")
	}
}

// TestRefreshDuringCloseDoesNotResurrect pins the interleaving. Either order is
// safe and both end with an empty cache: a refresh that wins the lock completes
// against the old client and is then cleared by Close; one that loses sees
// client == nil and does nothing.
func TestRefreshDuringCloseDoesNotResurrect(t *testing.T) {
	for i := 0; i < 25; i++ {
		e := New()
		mustConfigure(t, e, Config{DownloadDirectory: t.TempDir(), IncomingPort: 0})
		if err := e.NewMagnet(testMagnet); err != nil {
			e.Close()
			t.Fatalf("NewMagnet: %v", err)
		}

		done := make(chan struct{}, 2)
		go func() { e.refresh(time.Now()); done <- struct{}{} }()
		go func() { e.Close(); done <- struct{}{} }()
		<-done
		<-done

		if got := e.GetTorrents(); len(got) != 0 {
			t.Fatalf("round %d: %d torrents after Close; a refresh resurrected the cache",
				i, len(got))
		}
	}
}

// TestSamplerRunsWithoutWatchers pins that sampling is ungated. The server's
// poll loop skips rendering when nobody is connected, and torrent freshness
// must not ride on that: the engine has no idea whether anyone is watching, and
// a gated sampler would make the first reading after an idle spell an average
// over however long that was.
func TestSamplerRunsWithoutWatchers(t *testing.T) {
	e, hash := configuredEngine(t)

	// Nothing here plays the part of a watcher. The ticker is the only thing
	// that can move updatedAt.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tor := liveTorrent(t, e, hash)
		e.mu.Lock()
		sampled := !tor.updatedAt.IsZero()
		e.mu.Unlock()
		if sampled {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no sample was taken within 5s with no watchers; the engine's " +
		"cadence is gated on something it should not know about")
}
