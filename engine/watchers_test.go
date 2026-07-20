package engine

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// The metadata watchers had three defects that only show up on the stop, start
// and delete paths — the ones engine/CLAUDE.md describes but nothing ran:
//
//   - a restarted magnet got no fresh watcher, so it showed as running forever
//     while downloading nothing;
//   - a delete left its watcher parked until the process ended, because Drop
//     does not close GotInfo;
//   - a duplicate add parked a second watcher on a handle already covered.
//
// All three are about a goroutine's existence, which NumGoroutine cannot speak
// to here: a configured engine runs a torrent client with dozens of its own.
// Counting stacks by function name is exact instead, and it is what lets these
// assert the invariant rather than a proxy for it.

// watcherFunc is the watcher goroutine's entry in a stack dump.
const watcherFunc = "engine.(*Engine).watchInfoLocked.func1"

func liveWatchers() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), watcherFunc)
}

// waitWatchers waits for the watcher count to reach base+want.
//
// Relative to a baseline rather than absolute: a watcher belonging to an engine
// another test has not yet cleaned up would otherwise be counted as ours.
//
// It polls because a watcher exits asynchronously — cancelling its context and
// observing the goroutine gone are two different instants.
func waitWatchers(t *testing.T, base, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := liveWatchers() - base
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("metadata watchers: got %d, want %d", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// magnetEngine returns a configured engine holding one magnet whose metadata
// will never arrive, so every watcher below stays parked until something
// releases it deliberately.
func magnetEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	e := New()
	t.Cleanup(func() { e.Close() })
	mustConfigure(t, e, Config{DownloadDirectory: t.TempDir(), IncomingPort: 0, AutoStart: false})
	if err := e.NewMagnet(testMagnet); err != nil {
		t.Fatalf("NewMagnet: %v", err)
	}
	return e, onlyTorrent(t, e).InfoHash
}

// TestRestartRewatchesForMetadata covers a magnet that showed as running
// forever while downloading nothing.
//
// Stopping drops the torrent, and a magnet's retained spec carries no
// InfoBytes, so the re-added handle has no metadata and startLocked cannot call
// DownloadAll. The watcher from the original add is parked on the *previous*
// handle, where infoArrived's t.t != tt guard makes it a no-op — so unless the
// restart registers a fresh one, nothing is left to call DownloadAll when the
// metadata finally lands.
//
// Scope, since it is easy to over-read: this asserts a live watcher on the
// current handle. The DownloadAll it eventually performs is not observable —
// anacrolix exposes no effect of it, which the sibling test for the add path
// records for the same reason.
func TestRestartRewatchesForMetadata(t *testing.T) {
	base := liveWatchers()
	e, hash := magnetEngine(t)
	waitWatchers(t, base, 1)

	if err := e.StartTorrent(hash); err != nil {
		t.Fatalf("StartTorrent: %v", err)
	}
	if err := e.StopTorrent(hash); err != nil {
		t.Fatalf("StopTorrent: %v", err)
	}
	// Stopping releases the watcher: its handle is gone and Drop will never
	// close GotInfo.
	waitWatchers(t, base, 0)

	if err := e.StartTorrent(hash); err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitWatchers(t, base, 1)

	e.mu.Lock()
	defer e.mu.Unlock()
	tor := e.ts[hash]
	if tor.t == nil {
		t.Fatal("restart must re-add the torrent")
	}
	if tor.watching != tor.t {
		t.Fatal("restart left the watcher on the dropped handle; the magnet " +
			"would stay flagged as running and never download")
	}
}

// TestDeleteReleasesTheWatcher pins the leak. Drop does not close GotInfo, so
// before the per-torrent cancel a delete left its watcher parked for the
// lifetime of the process — an unbounded goroutine count driven by a user
// action.
func TestDeleteReleasesTheWatcher(t *testing.T) {
	base := liveWatchers()
	e, hash := magnetEngine(t)
	waitWatchers(t, base, 1)

	if err := e.DeleteTorrent(hash); err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	waitWatchers(t, base, 0)
}

// TestReAddDoesNotStackWatchers pins the "exactly one watcher" invariant.
// AddTorrentSpec returns the existing handle for a duplicate add, so
// registering unconditionally parked a second watcher on a handle already
// covered — harmless in its decision, but it accumulates under a retrying
// client.
//
// What this can and cannot fail against, since the two are easy to conflate:
// it fails against the unconditional register (two watchers), which is the
// defect. It does *not* fail against dropping the `watching == tt` short
// circuit alone, because the stopWatch behind it cancels the first watcher
// before the second starts and the count still settles at one. That short
// circuit is an optimisation, not the invariant.
func TestReAddDoesNotStackWatchers(t *testing.T) {
	base := liveWatchers()
	e, _ := magnetEngine(t)
	waitWatchers(t, base, 1)

	if err := e.NewMagnet(testMagnet); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	// Held long enough that a second watcher would have been observed.
	waitWatchers(t, base, 1)
	time.Sleep(20 * time.Millisecond)
	if got := liveWatchers() - base; got != 1 {
		t.Fatalf("a duplicate add parked another watcher: got %d, want 1", got)
	}
}
