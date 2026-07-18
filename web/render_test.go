package web

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/files"
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

	view := statsView{Version: "1.0", StatsData: StatsData{Set: true, GoRoutines: 7}}

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
	fragments := []struct {
		name string
		data any
	}{
		{"stats", statsView{StatsData: StatsData{Set: true}}},
		{"api-ok", "Done."},
		{"api-error", "Nope."},
		{"torrent-list", []torrentView{{InfoHash: "abc", Name: "N", Loaded: true}}},
		{"torrent-row", torrentView{InfoHash: "abc", Name: "N", Loaded: true, Started: true}},
		{"torrent-files", torrentView{InfoHash: "abc", Files: []*engine.File{{Path: "a/b.mkv", Size: 1}}}},
		{"omni", nil},
		{"config", engine.Config{DownloadDirectory: "/d", IncomingPort: 1}},
		{"downloads", struct {
			Root      fsView
			Truncated bool
			Limit     int
		}{Root: newRootView(&files.Node{Name: "d", IsDir: true}), Limit: 10}},
	}
	for _, f := range fragments {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, f.name, f.data); err != nil {
			t.Errorf("%s: render failed: %v", f.name, err)
			continue
		}
		if err := checkFragment(f.name, buf.Bytes()); err != nil {
			t.Errorf("%v", err)
		}
		if strings.Contains(buf.String(), "ZgotmplZ") {
			t.Errorf("%s: ZgotmplZ in output — a value reached a URL or CSS "+
				"context the autoescaper could not prove safe", f.name)
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

	if len(r.snapshot()) != 1 {
		t.Fatal("expected the region in the snapshot")
	}
	frame := r.forget("torrent-abc")
	if string(frame) != "event: torrent-abc\ndata:\n\n" {
		t.Errorf("forget frame = %q, want an empty data event", frame)
	}
	if len(r.snapshot()) != 0 {
		t.Error("forgotten region must leave the snapshot")
	}
}
