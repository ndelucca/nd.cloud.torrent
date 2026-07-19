package engine

import (
	"sync"
	"testing"
	"time"
)

// The suite races Configure against Configure and against Close, but nothing ran
// the path that mutates the ts map against the path that reads it. The root
// CLAUDE.md records that the bugs this codebase actually shipped were
// unsynchronized map access, twice, so this is the gap that mattered most.
//
// It also matters ahead of any change to how sampling works: GetTorrents is
// currently a *mutating* read — it refreshes the cache from the client and takes
// a progress sample — so it contends with adds and deletes for e.mu. These tests
// are the ones that say whether that contract still holds after a refactor.

// TestConcurrentReadsAndMutations runs GetTorrents against adds and deletes.
// Under -race, an unsynchronized map access here is a hard failure rather than
// an intermittent one.
func TestConcurrentReadsAndMutations(t *testing.T) {
	e := New()
	defer e.Close()
	mustConfigure(t, e, Config{DownloadDirectory: t.TempDir(), IncomingPort: 0})
	if err := e.NewTorrentFile(testTorrentFile(t)); err != nil {
		t.Fatalf("NewTorrentFile: %v", err)
	}
	hash := onlyTorrent(t, e).InfoHash

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers. GetTorrents refreshes and clones, so these are writers to the
	// engine's internal state even though they read from the caller's view.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, tor := range e.GetTorrents() {
					// Touch the copied fields: a shallow clone would show up
					// here as a race on the Files backing array.
					_ = tor.Name
					_ = tor.Percent
					for _, f := range tor.Files {
						_ = f.Path
					}
				}
			}
		}()
	}

	// Mutators: add and remove the same torrent, plus start/stop churn.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = e.NewTorrentFile(testTorrentFile(t))
			_ = e.NewMagnet(testMagnet)
			_ = e.StartTorrent(hash)
			_ = e.StopTorrent(hash)
			_ = e.DeleteTorrent(hash)
		}
	}()

	time.Sleep(250 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestConcurrentStateReadsAreConsistent pins that the deep copy actually is one.
// The internal ts map and its *Torrent values must never escape, because callers
// marshal what they get back while the engine keeps mutating the originals.
func TestConcurrentStateReadsAreConsistent(t *testing.T) {
	e := New()
	defer e.Close()
	mustConfigure(t, e, Config{DownloadDirectory: t.TempDir(), IncomingPort: 0})
	if err := e.NewTorrentFile(testTorrentFile(t)); err != nil {
		t.Fatalf("NewTorrentFile: %v", err)
	}

	// A snapshot held across further engine activity must not change underneath
	// its holder.
	snapshot := e.GetTorrents()
	before := map[string]int64{}
	for ih, tor := range snapshot {
		before[ih] = tor.Downloaded
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.GetTorrents()
		}()
	}
	wg.Wait()

	for ih, tor := range snapshot {
		if tor.Downloaded != before[ih] {
			t.Fatalf("torrent %s: a held snapshot changed under the caller "+
				"(%d -> %d); GetTorrents is not returning a deep copy",
				ih, before[ih], tor.Downloaded)
		}
		if tor.t != nil || tor.spec != nil {
			t.Fatalf("torrent %s: internal handles escaped the engine", ih)
		}
	}
}
