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
// What UI.mu actually protects:
//
//   - `dlContentSig` and `dlContentAt`, plain fields read and written by
//     RenderDownloads. They are the only cross-tick state the Render* methods
//     share, and -race catches two concurrent calls on them.
//   - Broadcast *ordering* between the poll and stats loops. That is a logical
//     race, not a memory one: every other piece of shared state (the renderer
//     cache) has its own mutex, so -race stays silent no matter how the two
//     interleave. It is not testable here, and pretending otherwise would be
//     the kind of coverage that reads as protection and is not.
//
// Removing UI.mu from RenderStats alone produces no failure anywhere in the
// suite. That is worth knowing before anyone "simplifies" it away.

// TestConcurrentRenderTorrentsAreSerialized drives the pairing the poll loop
// produces. It reaches the renderer cache, which has its own mutex, so what it
// proves is that the path is clean under -race rather than that UI.mu is
// load-bearing for it — the state UI.mu exists for lives on the downloads path,
// which TestConcurrentRendersAreSerialized below exercises.
func TestConcurrentRenderTorrentsAreSerialized(t *testing.T) {
	u := newTestUI(t)

	const rounds = 60
	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				torrents := map[string]*engine.Torrent{}
				// Different goroutines publish different sets, so the renderer
				// cache is genuinely written from several directions.
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
			_ = u.renderer.snapshot()
			_ = u.Watchers()
		}
	}()

	wg.Wait()
}

// TestConcurrentRendersWithChurningMembership is the same race with the torrent
// set changing underneath. Membership churn plus a concurrent stats render is
// the exact pairing the poll and stats loops produce.
func TestConcurrentRendersWithChurningMembership(t *testing.T) {
	u := newTestUI(t)

	const rounds = 60
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			torrents := map[string]*engine.Torrent{}
			// The set grows and shrinks, so torrents appear and disappear
			// between renders.
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

	// The region count is fixed regardless of churn: torrent-list, stats and
	// downloads-changed. A count above three means a region name is being
	// built at runtime again, which is what the fixed-name contract forbids.
	u.renderer.mu.Lock()
	cached := len(u.renderer.framedBody)
	u.renderer.mu.Unlock()
	if cached > 3 {
		t.Fatalf("renderer holds %d regions after churn, want at most 3 fixed ones; "+
			"a dynamic region name has come back", cached)
	}
}
