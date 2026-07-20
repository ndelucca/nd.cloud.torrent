package web

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// splitFrames breaks one broadcast into its individual SSE events.
//
// A tick is delivered as a single concatenated buffer, so a channel receive
// carries many frames. SSE frames are self-delimiting — a blank line ends one —
// which is what makes the concatenation valid in the first place.
func splitFrames(buf []byte) []string {
	var out []string
	for _, raw := range strings.Split(string(buf), "\n\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		line, rest, _ := strings.Cut(raw, "\n")
		name := strings.TrimPrefix(line, "event: ")
		body := strings.TrimSpace(strings.ReplaceAll(rest, "data:", ""))
		if body == "" {
			out = append(out, name+" [EMPTY]")
		} else {
			out = append(out, name)
		}
	}
	return out
}

// sentinelEvent terminates a collect run. It is a real broadcast, so it arrives
// after everything fn produced and cannot overtake it.
const sentinelEvent = "__collect_sentinel__"

// collect drains everything the hub broadcast during fn, as event names, with
// " [EMPTY]" appended for content-free events.
//
// The subscriber is drained concurrently rather than after fn returns: the
// hub's buffer is subBuffer deep and broadcast *disconnects* a subscriber that
// fills it, so a test with more than a handful of torrents would silently lose
// events instead of failing.
//
// The end of the run is a sentinel broadcast, not a sleep. A fixed settle was a
// guess at how long the drainer needs, and under -race on a contended machine a
// broadcast could miss the window — which surfaced as an assertion about event
// contents rather than as a timeout, i.e. as a bug that is not there. The
// sentinel makes it exact: frames are delivered in order, so seeing it means
// everything before it has been collected.
func collect(t *testing.T, u *UI, fn func()) []string {
	t.Helper()
	sub := u.hub.subscribe()
	defer u.hub.unsubscribe(sub)

	var mu sync.Mutex
	var out []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case frame, ok := <-sub.ch:
				if !ok {
					return
				}
				names := splitFrames(frame)
				mu.Lock()
				for _, n := range names {
					if strings.HasPrefix(n, sentinelEvent) {
						mu.Unlock()
						return
					}
					out = append(out, n)
				}
				mu.Unlock()
			case <-sub.done:
				return
			}
		}
	}()

	fn()
	u.hub.broadcast(frameSSE(sentinelEvent, []byte("<i></i>")))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("collect: sentinel never arrived; the drainer is stuck or the hub dropped it")
	}
	u.hub.unsubscribe(sub)

	mu.Lock()
	defer mu.Unlock()
	if len(out) == 0 {
		return nil
	}
	return append([]string(nil), out...)
}

func newTestUI(t *testing.T) *UI {
	t.Helper()
	u, err := New(Deps{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func torrent(hash, name string, pct float32) *engine.Torrent {
	return &engine.Torrent{
		InfoHash: hash, Name: name, Loaded: true, Started: true,
		Percent: pct, Size: 1000, Downloaded: int64(pct * 10),
	}
}

// The tests below assert observable output — what a browser receives — rather
// than internal bookkeeping. With one fixed region for the whole list there is
// no per-torrent listener lifecycle left to pin.

// TestIdleServerIsSilent is the byte-gating property, and the reason a full
// list per tick is affordable: an unchanged list emits nothing at all. This is
// what the manual `curl -sN localhost:3000/events` check corresponds to.
func TestIdleServerIsSilent(t *testing.T) {
	u := newTestUI(t)
	torrents := map[string]*engine.Torrent{
		"aaa": torrent("aaa", "A", 10),
		"bbb": torrent("bbb", "B", 20),
	}

	if got := collect(t, u, func() { u.RenderTorrents(torrents) }); len(got) != 1 {
		t.Fatalf("first render emitted %v, want one torrent-list", got)
	}
	if got := collect(t, u, func() { u.RenderTorrents(torrents) }); len(got) != 0 {
		t.Errorf("an unchanged list emitted %v, want nothing", got)
	}
}

// TestProgressEmitsTheList is the inverse, and the behaviour the two-tier
// scheme deliberately avoided: progress on any torrent re-sends the whole list.
func TestProgressEmitsTheList(t *testing.T) {
	u := newTestUI(t)
	torrents := map[string]*engine.Torrent{"aaa": torrent("aaa", "A", 10)}
	collect(t, u, func() { u.RenderTorrents(torrents) })

	torrents["aaa"].Percent = 50
	got := collect(t, u, func() { u.RenderTorrents(torrents) })
	if len(got) != 1 || got[0] != torrentListEvent {
		t.Errorf("progress emitted %v, want one %s", got, torrentListEvent)
	}
}

// TestRemovedTorrentLeavesTheList asserts the hash is absent from the payload.
// That is strictly stronger than the old test, which only proved an empty
// event had been emitted for its region — i.e. that a listener was released,
// not that the row was gone.
func TestRemovedTorrentLeavesTheList(t *testing.T) {
	u := newTestUI(t)
	torrents := map[string]*engine.Torrent{
		"aaa": torrent("aaa", "A", 10),
		"bbb": torrent("bbb", "B", 20),
	}
	u.RenderTorrents(torrents)

	delete(torrents, "bbb")
	u.RenderTorrents(torrents)

	body := u.renderer.framed(torrentListEvent)
	if bytes.Contains(body, []byte("bbb")) {
		t.Errorf("the removed torrent is still in the list payload:\n%s", body)
	}
	if !bytes.Contains(body, []byte("aaa")) {
		t.Errorf("the surviving torrent is missing from the payload:\n%s", body)
	}
}

// TestSortOrderFollowsName pins a bug the two-tier scheme could not fix. The
// skeleton was gated on the infohash *set*, which does not change when a
// magnet's metadata arrives — so the row's contents updated but the list stayed
// in the position it held under "Fetching metadata…" until membership next
// moved for some other reason.
func TestSortOrderFollowsName(t *testing.T) {
	u := newTestUI(t)
	// zzz has no name yet, so it renders as "Fetching metadata… zzz" and sorts
	// after "Alpha".
	torrents := map[string]*engine.Torrent{
		"aaa": torrent("aaa", "Alpha", 10),
		"zzz": {InfoHash: "zzz", Loaded: false, Started: true},
	}
	u.RenderTorrents(torrents)
	before := string(u.renderer.framed(torrentListEvent))
	if strings.Index(before, "aaa") > strings.Index(before, "zzz") {
		t.Fatalf("setup: expected Alpha first\n%s", before)
	}

	// The name lands and should sort first.
	torrents["zzz"].Name = "AAAA"
	torrents["zzz"].Loaded = true
	u.RenderTorrents(torrents)
	after := string(u.renderer.framed(torrentListEvent))
	if strings.Index(after, "zzz") > strings.Index(after, "aaa") {
		t.Errorf("a torrent whose name arrived did not move in the list:\n%s", after)
	}
}

// TestOneBroadcastPerTick pins the invariant that makes subBuffer mean
// something.
//
// RenderTorrents used to broadcast once per torrent, so a subscriber's buffer
// measured *changed rows* rather than lag: with subBuffer at 8, eight rows
// moving in one tick could stall a perfectly healthy client. Each frame was its
// own Write and Flush, so a reader descheduled for a few milliseconds fell
// behind, and broadcast disconnects a subscriber it cannot deliver to — which
// reconnected, took the snapshot, kicked the render loop, and produced another
// burst. With one broadcast per tick the buffer measures ticks, which is a real
// stall.
func TestOneBroadcastPerTick(t *testing.T) {
	u := newTestUI(t)

	// Comfortably more torrents than subBuffer.
	torrents := map[string]*engine.Torrent{}
	for i := 0; i < subBuffer*3; i++ {
		h := string(rune('a'+i%26)) + string(rune('a'+i/26))
		torrents[h] = torrent(h, "T"+h, float32(i))
	}

	u.RenderTorrents(torrents)

	sub := u.hub.subscribe()
	defer u.hub.unsubscribe(sub)
	for _, tor := range torrents {
		tor.Percent += 1
		tor.Downloaded += 10
	}
	u.RenderTorrents(torrents)

	var receives, events int
	var payload []byte
	for draining := true; draining; {
		select {
		case frame := <-sub.ch:
			receives++
			events += len(splitFrames(frame))
			payload = append(payload, frame...)
		default:
			draining = false
		}
	}

	if receives != 1 {
		t.Fatalf("a tick produced %d broadcasts for %d torrents; want exactly 1",
			receives, len(torrents))
	}
	// One frame, not one per changed row. The single-region scheme makes this
	// structural rather than something the render loop has to be careful about.
	if events != 1 {
		t.Errorf("a tick carried %d events, want exactly 1 (%s)", events, torrentListEvent)
	}
	// And that one frame must actually contain every torrent, or "one broadcast"
	// would be satisfied by sending less.
	for hash := range torrents {
		if !bytes.Contains(payload, []byte(hash)) {
			t.Fatalf("torrent %s is missing from the coalesced frame", hash)
		}
	}
}

// TestFileViewIsSorted pins an ordering that was lost without anything
// noticing.
//
// The sort used to live in the handler and was dropped when that handler was
// collapsed into a shared helper. No test covered it, so the file table simply
// started rendering in map order. Building the view now sorts it, so an
// unsorted one is unrepresentable.
func TestFileViewIsSorted(t *testing.T) {
	tor := &engine.TorrentWithFiles{
		Torrent: engine.Torrent{InfoHash: "abc"},
		Files: []engine.File{
			{Path: "z/last.mkv"},
			{Path: "a/first.mkv"},
			{Path: "m/middle.mkv"},
		},
	}
	v := newTorrentViewWithFiles(tor)

	var got []string
	for _, f := range v.Files {
		got = append(got, f.Name)
	}
	want := []string{"first.mkv", "last.mkv", "middle.mkv"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("file order = %v, want %v", got, want)
		}
	}
}
