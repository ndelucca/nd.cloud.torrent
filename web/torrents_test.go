package web

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// collect drains everything the hub broadcast during fn, as event names, with
// " [EMPTY]" appended for content-free events.
//
// The subscriber is drained concurrently rather than after fn returns: the
// hub's buffer is subBuffer deep and broadcast *disconnects* a subscriber that
// fills it, so a test with more than a handful of torrents would silently lose
// events instead of failing.
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
				line, rest, _ := bytes.Cut(frame, []byte("\n"))
				name := strings.TrimPrefix(string(line), "event: ")
				body := strings.TrimSpace(strings.ReplaceAll(string(rest), "data:", ""))
				mu.Lock()
				if body == "" {
					out = append(out, name+" [EMPTY]")
				} else {
					out = append(out, name)
				}
				mu.Unlock()
			case <-sub.done:
				return
			}
		}
	}()

	fn()
	// Let the drainer catch up; broadcast is synchronous into the channel, so a
	// short settle is enough.
	time.Sleep(50 * time.Millisecond)
	u.hub.unsubscribe(sub)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(out) == 0 {
		return nil
	}
	return append([]string(nil), out...)
}

func torrent(hash, name string, pct float32) *engine.Torrent {
	return &engine.Torrent{
		InfoHash: hash, Name: name, Loaded: true, Started: true,
		Percent: pct, Size: 1000, Downloaded: int64(pct * 10),
	}
}

// TestTorrentListIsNotTreatedAsATorrent pins a bug that would have been
// invisible server-side and destructive in the browser.
//
// Removals were originally found by scanning cached region names for the
// "torrent-" prefix — which also matches the membership region "torrent-list".
// Every tick the skeleton was forgotten, re-rendered, and shipped alongside an
// *empty* torrent-list event, which htmx would have swapped in, wiping the
// entire torrent list once per second.
func TestTorrentListIsNotTreatedAsATorrent(t *testing.T) {
	u := newTestUI(t)
	torrents := map[string]*engine.Torrent{
		"aaa": torrent("aaa", "Alpha", 10),
	}

	first := collect(t, u, func() { u.RenderTorrents(torrents) })
	if len(first) == 0 {
		t.Fatal("first render produced nothing")
	}
	// Second render emits the row, which the first deliberately deferred.
	collect(t, u, func() { u.RenderTorrents(torrents) })

	// By now nothing at all has changed, so this must be completely silent.
	third := collect(t, u, func() { u.RenderTorrents(torrents) })
	for _, ev := range third {
		if strings.HasPrefix(ev, torrentListEvent) {
			t.Errorf("torrent-list re-sent with no membership change (%q); "+
				"an empty one would wipe the list in the browser", ev)
		}
	}
	if len(third) != 0 {
		t.Errorf("unchanged state produced %v, want no events", third)
	}
}

// TestSkeletonIsGatedOnMembershipNotBytes covers the second half of the same
// design. The skeleton embeds each row's current content so a new row is never
// briefly blank — which means its bytes change whenever any progress does.
// Gating its emission on rendered bytes would therefore re-send the entire
// skeleton every second and collapse the two tiers back into one.
func TestSkeletonIsGatedOnMembershipNotBytes(t *testing.T) {
	u := newTestUI(t)
	m := map[string]*engine.Torrent{"aaa": torrent("aaa", "Alpha", 10)}
	collect(t, u, func() { u.RenderTorrents(m) })
	collect(t, u, func() { u.RenderTorrents(m) })

	// Progress moves on every tick, which changes the skeleton's bytes.
	for i, p := range []float32{20, 30, 40} {
		m["aaa"] = torrent("aaa", "Alpha", p)
		got := collect(t, u, func() { u.RenderTorrents(m) })
		if contains(got, torrentListEvent) {
			t.Fatalf("tick %d: skeleton re-sent for a progress change (%v); "+
				"the whole list would be re-shipped every second", i, got)
		}
		if !contains(got, "torrent-aaa") {
			t.Errorf("tick %d: expected the row, got %v", i, got)
		}
	}
}

// TestTorrentTwoTierEvents covers the membership/volatile split.
func TestTorrentTwoTierEvents(t *testing.T) {
	u := newTestUI(t)

	// 1. A new torrent emits the skeleton. Its row is NOT emitted in the same
	//    flush: emitting an item event alongside the event that creates its
	//    element races the extension's registration (observed in-browser as a
	//    missed update at 300 ms).
	one := map[string]*engine.Torrent{"aaa": torrent("aaa", "Alpha", 10)}
	got := collect(t, u, func() { u.RenderTorrents(one) })
	if !contains(got, torrentListEvent) {
		t.Errorf("new torrent: got %v, want torrent-list", got)
	}
	if contains(got, "torrent-aaa") {
		t.Errorf("row emitted in the same flush as the skeleton that creates it: %v", got)
	}

	// 2. Progress changes emit only the row, never the skeleton.
	one["aaa"] = torrent("aaa", "Alpha", 42)
	got = collect(t, u, func() { u.RenderTorrents(one) })
	if !contains(got, "torrent-aaa") {
		t.Errorf("progress change: got %v, want torrent-aaa", got)
	}
	if contains(got, torrentListEvent) {
		t.Errorf("skeleton re-sent for a progress change: %v", got)
	}

	// 3. Adding a torrent changes membership, so the skeleton returns.
	one["bbb"] = torrent("bbb", "Beta", 0)
	got = collect(t, u, func() { u.RenderTorrents(one) })
	if !contains(got, torrentListEvent) {
		t.Errorf("added torrent: got %v, want torrent-list", got)
	}

	// 4. Removing one must emit a final EMPTY event for its name, or htmx never
	//    unregisters the listener and leaks the detached subtree.
	delete(one, "bbb")
	got = collect(t, u, func() { u.RenderTorrents(one) })
	if !contains(got, "torrent-bbb [EMPTY]") {
		t.Errorf("removed torrent: got %v, want an empty torrent-bbb event", got)
	}
	if !contains(got, torrentListEvent) {
		t.Errorf("removal must also update the skeleton: %v", got)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// newTestUI builds a UI with no engine, no server and no ports.
//
// These tests assert on SSE event names, which is a property of the renderer
// and the hub alone. Before the split they went through newTestServer, which
// meant a real torrent client, a temp config file and two bound ports to check
// that a string starts with "torrent-".
func newTestUI(t *testing.T) *UI {
	t.Helper()
	u, err := New(Deps{Title: "test"})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return u
}

// TestForgetSurvivesARenderFailure covers regions that were never forgotten.
//
// seen was advanced from a defer, so it ran even on the early return taken when
// the skeleton fails to render — a tick where nothing was sent at all. The
// forget events computed for that tick were skipped, the next tick saw an empty
// removal set, and the deleted torrents' regions stayed in the renderer
// forever: unbounded growth, and rows resurrected for every new subscriber via
// the connect-time snapshot.
//
// seen means "what the browsers have been told", so it may only advance once
// they have actually been told.
func TestForgetSurvivesARenderFailure(t *testing.T) {
	u := newTestUI(t)
	good := u.renderer.tmpl

	// Cloned from a fresh parse: html/template refuses to Clone a template that
	// has already been executed.
	bad, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	// Fails at execution, not parse, so the failure lands where a real template
	// bug would: inside renderer.render.
	if _, err := bad.Parse(`{{define "torrent-list"}}<div>{{index . 99}}</div>{{end}}`); err != nil {
		t.Fatalf("override torrent-list: %v", err)
	}

	a := torrent("aa", "A", 10)
	b := torrent("bb", "B", 20)
	both := map[string]*engine.Torrent{"aa": a, "bb": b}
	onlyA := map[string]*engine.Torrent{"aa": a}

	// Establish both torrents.
	collect(t, u, func() { u.RenderTorrents(both) })

	// b disappears on a tick whose skeleton cannot render: nothing is sent.
	u.renderer.tmpl = bad
	got := collect(t, u, func() { u.RenderTorrents(onlyA) })
	for _, ev := range got {
		if strings.HasPrefix(ev, torrentEventPrefix+"bb") {
			t.Fatalf("a failed render still emitted %q; sent: %v", ev, got)
		}
	}

	// The next healthy tick must still report b as gone.
	u.renderer.tmpl = good
	got = collect(t, u, func() { u.RenderTorrents(onlyA) })
	want := torrentEventPrefix + "bb [EMPTY]"
	for _, ev := range got {
		if ev == want {
			return
		}
	}
	t.Fatalf("torrent bb was never forgotten; sent: %v", got)
}
