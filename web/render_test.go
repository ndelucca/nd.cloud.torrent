package web

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
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
		"placeholder":      "Loading…",
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

// TestSnapshotIsOneBuffer pins what a connecting client receives: every stored
// region, in a single buffer, so the first write puts the page in a complete
// state rather than assembling it over several frames.
//
// One buffer is the whole contract. With three fixed region names there is no
// listener lifecycle to manage and no ordering constraint to satisfy, so
// nothing else about snapshot is worth asserting.
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

// TestFragmentFallbacksWhenTheTemplateSetFails covers writeMessage, the last
// resort when rendering itself is what broke.
//
// It is only reachable when a template is missing or errors, which never
// happens with the real set — so it sat at zero coverage while being the path
// that decides what a browser sees when the UI is broken. htmx swaps whatever
// comes back, so returning nothing, or an error page, would put a blank panel
// or a stack trace into the document.
func TestFragmentFallbacksWhenTheTemplateSetFails(t *testing.T) {
	// Rendering a fragment whose template is absent must fall back to
	// fragment-message rather than to an empty body.
	// ServeDownloads reads Deps.Tree, so it has to be supplied; the default test
	// UI carries only a Title.
	withTree := func(t *testing.T) *UI {
		t.Helper()
		u, err := New(Deps{Tree: func() *files.Node {
			return &files.Node{Name: "root", IsDir: true}
		}})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	t.Run("falls back to the message template", func(t *testing.T) {
		u := withTree(t)
		// Only fragment-message survives, so "downloads" cannot render.
		u.renderer.tmpl = template.Must(template.New("t").
			Parse(`{{define "fragment-message"}}<p class="muted">{{.}}</p>{{end}}`))

		w := httptest.NewRecorder()
		u.ServeDownloads(w, httptest.NewRequest(http.MethodGet, "/fragments/downloads", nil))

		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html — htmx swaps this into the page", ct)
		}
		if body := w.Body.String(); !strings.Contains(body, "Could not render downloads.") {
			t.Errorf("body = %q, want the caller's fallback message", body)
		}
	})

	// And when even that template is gone there is nothing left to render with,
	// so the one HTML literal in the package takes over. It must still be an
	// element: a bare-text payload swapped by idiomorph lands as an EMPTY
	// target, which is the failure checkFragment exists to prevent.
	t.Run("falls back to the literal when nothing can render", func(t *testing.T) {
		u := withTree(t)
		u.renderer.tmpl = template.Must(template.New("t").Parse(``))

		w := httptest.NewRecorder()
		u.ServeDownloads(w, httptest.NewRequest(http.MethodGet, "/fragments/downloads", nil))

		body := strings.TrimSpace(w.Body.String())
		if body == "" {
			t.Fatal("an empty body leaves the panel blank with no explanation")
		}
		if body[0] != '<' {
			t.Errorf("body = %q, want an element — bare text swaps as empty", body)
		}
	})
}

// framed returns the cached framing for a region.
//
// It lives in a _test.go file because only tests read it — the render path gets
// the frame back from store. Same package, so the call sites are unchanged, and
// it stays out of the shipped binary.
func (r *renderer) framed(event string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.framedBody[event]
}

// TestHumanBytesUsesBase1000 pins the half of the byte formatting that must
// agree with internal/reqlog.byteSize.
//
// The two render differently on purpose — "1.5 GB" here, "1.5GB" in a log line —
// and neither can call the other: both are unexported, in packages with no edge
// between them. What must not diverge is the base. When they disagreed, the same
// download was logged as 954MB and displayed as 1.0 GB, and a log that does not
// match the screen is worse than either convention.
//
// The boundary cases are what pin it: 1024 bytes is "1.0 KB" in base 1000 and
// would be "1.0 KB" at 1024 too, so the discriminating values are 1000 and 999.
// internal/reqlog.TestByteSize covers the same boundaries for the other side.
func TestHumanBytesUsesBase1000(t *testing.T) {
	for n, want := range map[int64]string{
		0:       "0 B",
		-1:      "0 B",
		999:     "999 B",
		1000:    "1.0 KB", // base 1024 would still say "1000 B" here
		1024:    "1.0 KB",
		1500:    "1.5 KB",
		1000000: "1.0 MB",
		// Clamps to the last unit rather than indexing past it. Note the two
		// formatters diverge here: reqlog.byteSize's "KMGTPE" runs one unit
		// further and would say EB. Harmless — a torrent is not an exabyte — but
		// it is a real difference, recorded rather than pretended away.
		1 << 60: "1152.9 PB",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
