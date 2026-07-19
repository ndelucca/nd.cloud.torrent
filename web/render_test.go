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
		"stats":            statsView{Stats: sysstat.Stats{Set: true}},
		"api-ok":           "Done.",
		"api-error":        "Nope.",
		"torrent-list":     []torrentView{{InfoHash: "abc", Name: "N", Loaded: true}},
		"torrent-row":      torrentView{InfoHash: "abc", Name: "N", Loaded: true, Started: true},
		"torrent-files":    torrentView{InfoHash: "abc", Files: []fileView{{Name: "b.mkv", Size: 1, Percent: 50, InProgress: true}}},
		"omni":             nil,
		"fragment-message": "Nothing here.",
		"config":           engine.Config{DownloadDirectory: "/d", IncomingPort: 1},
		"downloads":        newDownloadsView(&files.Node{Name: "d", IsDir: true}),
		"fsnode":           newFSView(&files.Node{Name: "f.mkv", Size: 2}, ""),
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

// TestSnapshotIsOneBuffer replaces two tests that pinned machinery the
// single-region scheme removed: renderer.forget (the final empty event that let
// htmx's SSE extension collect a per-element listener) and snapshot's
// membership-first ordering. Both existed only because region names were
// created and destroyed at runtime. With three fixed names there is no listener
// lifecycle to manage and no ordering constraint to satisfy — what remains worth
// asserting is that a connecting client gets every region in one buffer.
func TestSnapshotIsOneBuffer(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	r := newRenderer(tmpl)
	r.store(torrentListEvent, []byte("<div>list</div>"))
	r.store(statsEvent, []byte("<div>stats</div>"))

	snap := r.snapshot()
	for _, want := range []string{"event: " + torrentListEvent, "event: " + statsEvent} {
		if !bytes.Contains(snap, []byte(want)) {
			t.Errorf("snapshot is missing %q:\n%s", want, snap)
		}
	}
	// Self-delimiting frames concatenated into one write, not one write each.
	if n := bytes.Count(snap, []byte("event: ")); n != 2 {
		t.Errorf("snapshot carries %d frames, want 2", n)
	}
}
