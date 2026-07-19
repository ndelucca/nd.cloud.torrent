package web

import (
	"fmt"
	"sync"
	"testing"

	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/files"
	"github.com/ndelucca/nd.cloud.torrent/sysstat"
)

// UI.mu covers concurrent rendering: the server's poll loop and stats loop both
// call in. Nothing exercised that — server's TestConcurrentEventStreams only
// pairs stats.set with renderStats, never RenderTorrents against RenderStats.
//
// What UI.mu actually protects, established by removing it and watching:
//
//   - `seen`, a plain map read and written by RenderTorrents. Two concurrent
//     RenderTorrents calls race on it, and -race catches that. This test drives
//     exactly that pairing.
//   - Broadcast *ordering* between the poll and stats loops. That is a logical
//     race, not a memory one: every other piece of shared state (the renderer
//     cache) has its own mutex, so -race stays silent no matter how the two
//     interleave. It is not testable here, and pretending otherwise would be
//     the kind of coverage that reads as protection and is not.
//
// Removing UI.mu from RenderStats alone produces no failure anywhere in the
// suite. That is worth knowing before anyone "simplifies" it away.

// TestConcurrentRenderTorrentsRaceOnSeen pins the memory race. `seen` is
// cross-tick state — "what the browsers have been told" — and it is the only
// unguarded-by-default structure the Render* methods share.
func TestConcurrentRenderTorrentsRaceOnSeen(t *testing.T) {
	u := newTestUI(t)

	const rounds = 60
	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				torrents := map[string]*engine.Torrent{}
				// Different goroutines publish different sets, so `seen` is
				// genuinely written from several directions.
				for j := 0; j <= (i+g)%5; j++ {
					hash := fmt.Sprintf("%040x", j)
					torrents[hash] = torrent(hash, fmt.Sprintf("t%d", j), float32(i%100))
				}
				u.RenderTorrents(torrents)
			}
		}(g)
	}
	wg.Wait()
}

// TestConcurrentRendersAreSerialized runs the Render* methods against each other
// in the pattern the server actually produces: RenderTorrents and
// RenderDownloads from the poll loop, RenderStats from both loops, and a reader
// walking the cache the way a mid-tick subscriber does.
func TestConcurrentRendersAreSerialized(t *testing.T) {
	u := newTestUI(t)

	torrents := map[string]*engine.Torrent{}
	for i := 0; i < 8; i++ {
		hash := fmt.Sprintf("%040x", i)
		torrents[hash] = torrent(hash, fmt.Sprintf("t%d", i), float32(i))
	}

	const rounds = 60
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			// Vary the payload so change detection keeps producing frames
			// rather than suppressing everything after the first pass.
			for _, tor := range torrents {
				tor.Percent = float32(i % 100)
			}
			u.RenderTorrents(torrents)
		}
	}()

	// Two stats renderers, because the server has two: statsLoop samples and
	// renders, and pollLoop calls renderStats on every tick.
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				u.RenderStats(StatsData{
					System:         sysstat.Stats{Set: true, CPU: float64((i + g) % 100)},
					ConnectedUsers: i % 5,
				})
			}
		}(g)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			u.RenderDownloads(&files.Node{Name: fmt.Sprintf("d%d", i), IsDir: true})
		}
	}()

	// A reader, because a client connecting mid-tick walks the same cache the
	// renderers are writing.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = u.renderer.snapshot(torrentListEvent)
			_ = u.Watchers()
		}
	}()

	wg.Wait()
}

// TestConcurrentRendersWithChurningMembership is the same race with the torrent
// set changing underneath, which is what moves `seen` and drives the removal
// bookkeeping. Membership churn plus a concurrent stats render is the exact
// pairing the poll and stats loops produce.
func TestConcurrentRendersWithChurningMembership(t *testing.T) {
	u := newTestUI(t)

	const rounds = 60
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			torrents := map[string]*engine.Torrent{}
			// The set grows and shrinks, so torrents appear and disappear and
			// the forget path runs.
			for j := 0; j <= i%6; j++ {
				hash := fmt.Sprintf("%040x", j)
				torrents[hash] = torrent(hash, fmt.Sprintf("t%d", j), float32(j))
			}
			u.RenderTorrents(torrents)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			u.RenderStats(StatsData{
				System:         sysstat.Stats{Set: true, MemoryUsed: int64(i), MemoryTotal: 100},
				ConnectedUsers: i,
			})
		}
	}()

	wg.Wait()

	// After the churn the renderer must not be holding regions for torrents that
	// no longer exist. The last round rendered i%6+1 of them; anything beyond
	// the live set plus the two fixed regions means forget did not keep up.
	live := (rounds-1)%6 + 1
	u.renderer.mu.Lock()
	cached := len(u.renderer.framedBody)
	u.renderer.mu.Unlock()
	if cached > live+2 {
		t.Fatalf("renderer holds %d regions after churn, want at most %d "+
			"(%d live torrents plus torrent-list and downloads-changed)",
			cached, live+2, live)
	}
}
