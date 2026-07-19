package engine

import (
	"errors"
	"testing"
)

// StartTorrent and DeleteTorrent were both at 0% coverage, and
// TestStartAfterStopReAdds only drove startLocked against an engine with no
// client — so it asserted the failure path and left the behaviour the contract
// is actually about untested. engine/CLAUDE.md states it plainly: "StartTorrent
// re-adds from the retained spec, so start-after-stop works." Nothing ran that.

// configuredEngine returns an engine with a real client and one loaded torrent.
func configuredEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	e := New()
	t.Cleanup(func() { e.Close() })
	mustConfigure(t, e, Config{DownloadDirectory: t.TempDir(), IncomingPort: 0})
	if err := e.NewTorrentFile(testTorrentFile(t)); err != nil {
		t.Fatalf("NewTorrentFile: %v", err)
	}
	return e, onlyTorrent(t, e).InfoHash
}

// TestStartAfterStopReAddsForReal is the success path. Stopping drops the
// underlying torrent — there is no pause in anacrolix/torrent — so starting
// again has to re-add from the retained spec. Before this, only the
// no-client failure was covered, so a regression that left t.t nil would have
// shipped: the UI would show the torrent running while nothing moved.
func TestStartAfterStopReAddsForReal(t *testing.T) {
	e, hash := configuredEngine(t)

	if err := e.StartTorrent(hash); err != nil {
		t.Fatalf("StartTorrent: %v", err)
	}
	if !liveTorrent(t, e, hash).Started {
		t.Fatal("Started must be set after a successful start")
	}

	if err := e.StopTorrent(hash); err != nil {
		t.Fatalf("StopTorrent: %v", err)
	}
	stopped := liveTorrent(t, e, hash)
	if stopped.Started {
		t.Error("Started must be cleared by stop")
	}
	if stopped.t != nil {
		t.Error("stop must drop the underlying torrent handle")
	}
	if stopped.spec == nil {
		t.Fatal("stop must retain the spec, or the torrent can never restart")
	}

	// The assertion that was missing: this re-adds rather than flipping a flag.
	if err := e.StartTorrent(hash); err != nil {
		t.Fatalf("restart: %v", err)
	}
	restarted := liveTorrent(t, e, hash)
	if !restarted.Started {
		t.Error("Started must be set after the restart")
	}
	if restarted.t == nil {
		t.Fatal("restart flipped Started without re-adding the torrent; " +
			"the UI would show it running while nothing downloads")
	}
}

func TestStartTorrentRejectsDoubleStart(t *testing.T) {
	e, hash := configuredEngine(t)
	if err := e.StartTorrent(hash); err != nil {
		t.Fatalf("StartTorrent: %v", err)
	}
	if err := e.StartTorrent(hash); !errors.Is(err, ErrAlreadyStarted) {
		t.Errorf("double start = %v, want ErrAlreadyStarted", err)
	}
}

// TestDeleteTorrentRemovesItEverywhere covers the other 0% method. A delete that
// dropped the handle but left the cache entry would keep the row on the page
// forever, against a torrent the client no longer has.
func TestDeleteTorrentRemovesItEverywhere(t *testing.T) {
	e, hash := configuredEngine(t)
	if err := e.StartTorrent(hash); err != nil {
		t.Fatalf("StartTorrent: %v", err)
	}
	if err := e.DeleteTorrent(hash); err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	if got := e.GetTorrents(); len(got) != 0 {
		t.Fatalf("GetTorrents = %d torrents after delete, want 0", len(got))
	}
	if err := e.DeleteTorrent(hash); !errors.Is(err, ErrMissingTorrent) {
		t.Errorf("deleting twice = %v, want ErrMissingTorrent", err)
	}
}

// TestVerbsRejectUnknownHashes pins that the three verbs report a missing
// torrent rather than acting on nothing. A valid-looking hash that is not
// present must 404, not succeed silently.
func TestVerbsRejectUnknownHashes(t *testing.T) {
	e, _ := configuredEngine(t)
	absent := "abababababababababababababababababababab"

	for name, call := range map[string]func(string) error{
		"start":  e.StartTorrent,
		"stop":   e.StopTorrent,
		"delete": e.DeleteTorrent,
	} {
		if err := call(absent); !errors.Is(err, ErrMissingTorrent) {
			t.Errorf("%s on an absent hash = %v, want ErrMissingTorrent", name, err)
		}
	}

	// And a malformed one is caller error, not a missing torrent.
	for name, call := range map[string]func(string) error{
		"start":  e.StartTorrent,
		"stop":   e.StopTorrent,
		"delete": e.DeleteTorrent,
	} {
		if err := call("nothex"); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s on a malformed hash = %v, want ErrInvalidInput", name, err)
		}
	}
}

// liveTorrent reaches into ts for the real entry. GetTorrents hands out clones
// with the internal handles stripped, and t.t is exactly what these tests need
// to see.
func liveTorrent(t *testing.T, e *Engine, hash string) *torrentState {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	tor, ok := e.ts[hash]
	if !ok {
		t.Fatalf("torrent %s is not cached", hash)
	}
	return tor
}
