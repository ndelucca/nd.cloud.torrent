package web

import (
	"bytes"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/files"
	"github.com/ndelucca/nd.cloud.torrent/sysstat"
)

// TestFrameSSE pins the framing. Rendered HTML is full of newlines, and every
// line of an SSE payload needs its own "data: " prefix — writing a fragment as
// a single data line silently truncates it at the first newline.
func TestFrameSSE(t *testing.T) {
	got := string(frameSSE("stats", []byte("<div>\n  <span>hi</span>\n</div>")))
	want := "event: stats\ndata: <div>\ndata:   <span>hi</span>\ndata: </div>\n\n"
	if got != want {
		t.Errorf("frameSSE:\ngot  %q\nwant %q", got, want)
	}

	// CRLF must normalise, or the stray \r ends up inside the payload.
	if got := string(frameSSE("x", []byte("a\r\nb"))); got != "event: x\ndata: a\ndata: b\n\n" {
		t.Errorf("CRLF not normalised: %q", got)
	}

	// An empty body must still deliver the event — that is what lets htmx's
	// SSE extension collect a listener for a removed element.
	if got := string(frameSSE("torrent-abc", nil)); got != "event: torrent-abc\ndata:\n\n" {
		t.Errorf("empty frame = %q", got)
	}
}

// TestRendererChangeDetection covers the per-region suppression. Without it an
// idle server streams to every browser forever.
func TestRendererChangeDetection(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	r := newRenderer(tmpl)

	view := statsView{Version: "1.0", Stats: sysstat.Stats{Set: true, GoRoutines: 7}}

	first, err := r.render("stats", "stats", view)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("first render must produce a frame")
	}
	if !bytes.Contains(first, []byte("event: stats")) {
		t.Errorf("frame missing event name: %s", first)
	}

	again, err := r.render("stats", "stats", view)
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Errorf("identical render must be suppressed, got %s", again)
	}

	view.GoRoutines = 8
	changed, err := r.render("stats", "stats", view)
	if err != nil {
		t.Fatal(err)
	}
	if changed == nil {
		t.Error("changed data must produce a frame")
	}
}

// TestFragmentsAreWrappedInElements guards a silent failure verified in
// Chromium 150: idiomorph swaps a bare-text payload as EMPTY. No error is
// raised anywhere — the data arrives and the DOM goes blank — so the check has
// to happen at render time.
//
// This also runs over every shipped template, so adding one that emits leading
// text fails here rather than in a browser.
func TestFragmentsAreWrappedInElements(t *testing.T) {
	if err := checkFragment("x", []byte("  <div>ok</div>")); err != nil {
		t.Errorf("element fragment rejected: %v", err)
	}
	if err := checkFragment("x", nil); err != nil {
		t.Errorf("empty fragment rejected: %v", err)
	}
	err := checkFragment("x", []byte("bare text"))
	if !errors.Is(err, errBareText) {
		t.Errorf("bare text = %v, want errBareText", err)
	}

	// Every fragment, with data it can actually render. The previous version
	// executed them all with statsView{} and continue'd on error — which was
	// almost all of them — so it really only covered the two that happen to
	// tolerate a wrong type, while claiming to cover every template.
	tmpl, perr := parseTemplates()
	if perr != nil {
		t.Fatal(perr)
	}
	// Sample data per fragment. The set of *names* is not written here — it is
	// enumerated from the parsed templates below, so adding a {{define}} without
	// a fixture fails this test instead of being silently skipped. The list used
	// to be the source of truth and covered 9 of 11; `page` and `fsnode` were
	// absent while web/CLAUDE.md claimed the test ran over every shipped
	// template.
	fixtures := map[string]any{
		"stats":         statsView{Stats: sysstat.Stats{Set: true}},
		"api-ok":        "Done.",
		"api-error":     "Nope.",
		"torrent-list":  []torrentView{{InfoHash: "abc", Name: "N", Loaded: true}},
		"torrent-row":   torrentView{InfoHash: "abc", Name: "N", Loaded: true, Started: true},
		"torrent-files": torrentView{InfoHash: "abc", Files: []fileView{{Name: "b.mkv", Size: 1, Percent: 50, InProgress: true}}},
		"omni":          nil,
		"config":        engine.Config{DownloadDirectory: "/d", IncomingPort: 1},
		"downloads":     newDownloadsView(&files.Node{Name: "d", IsDir: true}),
		"fsnode":        newFSView(&files.Node{Name: "f.mkv", Size: 2}, ""),
		// page is a full document, not a fragment: it opens with <!doctype html>,
		// so checkFragment's "starts with <" holds but the element-wrapping rule
		// is not what governs it. It is still executed here for the render and
		// ZgotmplZ checks.
		"page": pageView{Title: "T", Config: engine.Config{DownloadDirectory: "/d", IncomingPort: 1}},
	}

	// Enumerate what actually shipped rather than what someone remembered to
	// list.
	//
	// Two kinds of name are skipped. The root ("cloud-torrent") defines nothing.
	// The "*.html" entries are ParseFS's doing: it registers every file under its
	// base name as well as registering each {{define}} inside it, so the set
	// contains "torrents.html" alongside "torrent-list" and "torrent-row". Those
	// file-level templates are the whitespace between the defines and are never
	// executed. This is the same base-name behaviour web/CLAUDE.md warns about
	// for collisions — worth seeing here, since it is why templates are addressed
	// only by {{define}} name.
	var names []string
	for _, tm := range tmpl.Templates() {
		n := tm.Name()
		if n == "" || n == "cloud-torrent" || strings.HasSuffix(n, ".html") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no templates found; the enumeration is broken, not the templates")
	}

	for _, name := range names {
		data, ok := fixtures[name]
		if !ok {
			t.Errorf("template %q has no fixture in this test; add one so it is "+
				"checked rather than skipped", name)
			continue
		}
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
			t.Errorf("%s: render failed: %v", name, err)
			continue
		}
		if err := checkFragment(name, buf.Bytes()); err != nil {
			t.Errorf("%v", err)
		}
		if strings.Contains(buf.String(), "ZgotmplZ") {
			t.Errorf("%s: ZgotmplZ in output — a value reached a URL or CSS "+
				"context the autoescaper could not prove safe", name)
		}
	}
}

// TestRendererForget covers the htmx SSE listener leak. The extension
// unregisters a per-element listener lazily, from inside the listener itself.
// If the server just stops emitting an event name after a torrent is deleted,
// that listener never runs again and retains the detached DOM subtree forever.
func TestRendererForget(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	r := newRenderer(tmpl)
	r.store("torrent-abc", []byte("<div>x</div>"))

	if !bytes.Contains(r.snapshot(torrentListEvent), []byte("torrent-abc")) {
		t.Fatal("expected the region in the snapshot")
	}
	frame := r.forget("torrent-abc")
	if string(frame) != "event: torrent-abc\ndata:\n\n" {
		t.Errorf("forget frame = %q, want an empty data event", frame)
	}
	if len(r.snapshot(torrentListEvent)) != 0 {
		t.Error("forgotten region must leave the snapshot")
	}
}

// TestSnapshotLeadsWithMembership pins an ordering that used to be accidental.
//
// A new subscriber receives every region's current body at once. The membership
// skeleton has to come first: an element cannot listen for torrent-<hash>
// before it exists, so a row frame that arrives ahead of the skeleton is
// silently discarded. That happened to hold only because infohashes are
// lowercase hex and 'l' sorts after 'f' — it inverts the day hashes are encoded
// any other way.
func TestSnapshotLeadsWithMembership(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	r := newRenderer(tmpl)
	// Deliberately stored out of order, and with a hash that sorts *before*
	// "torrent-list" so alphabetical order alone would put the row first.
	r.store(torrentEventPrefix+"aaaa", []byte("<div>row</div>"))
	r.store(torrentListEvent, []byte("<ul>list</ul>"))
	r.store(statsEvent, []byte("<div>stats</div>"))

	snap := r.snapshot(torrentListEvent)
	if !bytes.HasPrefix(snap, []byte("event: "+torrentListEvent+"\n")) {
		t.Fatalf("snapshot must lead with %q, got:\n%s", torrentListEvent, snap)
	}
	// And it is one buffer, not one frame: every region is present.
	for _, want := range []string{torrentEventPrefix + "aaaa", statsEvent} {
		if !bytes.Contains(snap, []byte("event: "+want+"\n")) {
			t.Errorf("snapshot is missing region %q", want)
		}
	}
}
