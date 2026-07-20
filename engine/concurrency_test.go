package engine

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The suite races Configure against Configure and against Close, but nothing ran
// the path that mutates the ts map against the path that reads it. The root
// CLAUDE.md records that the bugs this codebase actually shipped were
// unsynchronized map access, twice, so this is the gap that mattered most.
//
// It also matters ahead of any change to how sampling works. GetTorrents is a
// pure read of the last sample, so the concurrent *writes* these tests race it
// against come from two directions: the caller's adds and deletes, and the
// sampler goroutine, which is writing every torrent's progress on its own
// cadence throughout. The sampler is the half that is easy to forget, and the
// one that makes a configured engine the right fixture here.

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
	// Counted, because a bare sleep is not a workload. On a loaded runner the
	// goroutines below can be descheduled for most of the window and the test
	// passes having raced almost nothing — green, and covering nothing.
	var reads, mutations atomic.Int64

	// Readers. GetTorrents clones under the lock and touches nothing else; the
	// concurrent writes come from the mutators below and from the sampler.
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
				reads.Add(1)
				for ih, tor := range e.GetTorrents() {
					_ = tor.Name
					_ = tor.Percent
					// The file table is a separate call now, and it has to be
					// made here: Torrent carries no Files, so a reader that only
					// ranged over GetTorrents would range over nothing and this
					// test would silently stop covering the copy at all.
					if wf, err := e.TorrentWithFiles(ih); err == nil {
						for _, f := range wf.Files {
							_ = f.Path
						}
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
			mutations.Add(1)
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

	// The thresholds are deliberately low — this asserts that the race was
	// exercised at all, not how fast the machine is.
	if r, m := reads.Load(), mutations.Load(); r < 50 || m < 5 {
		t.Fatalf("the race barely ran: %d reads, %d mutation rounds. Whatever "+
			"-race reported, it did not report on much", r, m)
	}
}

// TestConcurrentStateReadsAreConsistent pins that a snapshot handed to a caller
// does not change underneath them while the engine keeps mutating, and exercises
// that path under -race.
//
// What it does NOT do, despite the obvious reading, is prove the copy is deep.
// Nothing observational can, because updateLoaded rebuilds Files with make() on
// every pass rather than patching in place — so even an aliased slice points at
// an array the engine has stopped touching. Verified: replacing slices.Clone
// with a plain alias leaves this test green.
//
// TestViewWithFilesIsDeep is what actually covers that, by mutating the copy and
// checking the original. The deep copy here defends against updateLoaded ever
// patching in place, which engine/CLAUDE.md forbids for a separate reason: index
// i stops being the same file once a torrent is re-added.
func TestConcurrentStateReadsAreConsistent(t *testing.T) {
	e := New()
	defer e.Close()
	mustConfigure(t, e, Config{DownloadDirectory: t.TempDir(), IncomingPort: 0})
	if err := e.NewTorrentFile(testTorrentFile(t)); err != nil {
		t.Fatalf("NewTorrentFile: %v", err)
	}
	// Files are populated from the live torrent by a sample.
	e.refresh(time.Now())

	hash := onlyTorrent(t, e).InfoHash
	held, err := e.TorrentWithFiles(hash)
	if err != nil {
		t.Fatalf("TorrentWithFiles: %v", err)
	}
	if len(held.Files) == 0 {
		t.Fatal("setup: the torrent has no files, so a shared backing array " +
			"would be undetectable here")
	}
	before := append([]File(nil), held.Files...)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.refresh(time.Now())
			e.GetTorrents()
			_, _ = e.TorrentWithFiles(hash)
		}()
	}
	wg.Wait()

	for i := range before {
		if held.Files[i] != before[i] {
			t.Fatalf("a held snapshot's file %d changed under the caller "+
				"(%+v -> %+v); the copy shares its backing array",
				i, before[i], held.Files[i])
		}
	}
	// No check that the internal handles did not escape: Torrent has no field
	// that could carry them, so it is unrepresentable rather than asserted.
}
