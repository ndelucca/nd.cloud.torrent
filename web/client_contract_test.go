package web

import (
	"strings"
	"testing"

	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/files"
)

// Attributes the templates emit for ct.js to act on are a contract between two
// languages with nothing but convention holding them together. The repo already
// pins that class of thing for routes and region names (contract_test.go); these
// cover the ones the idiomorph guards depend on, where a silent removal has no
// console error and no test failure — just a UI that misbehaves once per tick.

// TestFilePanelOptsOutOfMorphing pins data-preserve on the per-torrent file
// panel. Without it a morph reverts the fetched file table to the "Loading
// files…" placeholder and re-adds the x-cloak Alpine stripped at init, which
// hides the panel permanently behind [x-cloak]{display:none}.
func TestFilePanelOptsOutOfMorphing(t *testing.T) {
	u := newTestUI(t)
	body, err := u.renderer.execute("torrent-list", []torrentView{
		{InfoHash: "abc", Name: "N", Loaded: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)

	panel := `id="tf-abc"`
	idx := strings.Index(out, panel)
	if idx < 0 {
		t.Fatalf("file panel not rendered:\n%s", out)
	}
	// The attribute must be on the panel element itself, not merely somewhere in
	// the document: beforeNodeMorphed checks the node it is about to morph.
	end := strings.Index(out[idx:], ">")
	if end < 0 {
		t.Fatalf("unterminated panel element:\n%s", out[idx:])
	}
	tag := out[idx : idx+end]
	if !strings.Contains(tag, "data-preserve") {
		t.Errorf("file panel is missing data-preserve; a morph will revert its "+
			"fetched content and re-add x-cloak. Element: <%s>", tag)
	}
}

// TestTreeMarksTopLevelNodes pins data-top, which ct.js reads to decide a
// directory's default collapse state. It replaced a DOM-structure walk in the
// browser, so if the attribute stops being emitted the default silently becomes
// "closed" for everything.
func TestTreeMarksTopLevelNodes(t *testing.T) {
	root := &files.Node{Name: "downloads", IsDir: true, Children: []*files.Node{
		{Name: "movies", IsDir: true, Children: []*files.Node{
			{Name: "nested", IsDir: true},
		}},
	}}
	view := newDownloadsView(root)

	if len(view.Root.Children) != 1 {
		t.Fatalf("children = %d, want 1", len(view.Root.Children))
	}
	top := view.Root.Children[0]
	if !top.TopLevel {
		t.Error("a direct child of the download root must be TopLevel")
	}
	if len(top.Children) != 1 {
		t.Fatalf("nested children = %d, want 1", len(top.Children))
	}
	if top.Children[0].TopLevel {
		t.Error("a nested directory must not be TopLevel")
	}

	u := newTestUI(t)
	body, err := u.renderer.execute("downloads", view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `data-top="1"`) {
		t.Errorf("rendered tree carries no data-top; ct.js cannot tell top-level "+
			"nodes apart:\n%s", body)
	}
}

// TestEmptyDirectoryHasNoDanglingAriaControls pins that the toggle only names a
// list that exists. The <ul> is behind {{if .Children}}, so an empty directory
// used to point aria-controls at an absent element.
func TestEmptyDirectoryHasNoDanglingAriaControls(t *testing.T) {
	root := &files.Node{Name: "downloads", IsDir: true, Children: []*files.Node{
		{Name: "empty", IsDir: true},
	}}
	u := newTestUI(t)
	body, err := u.renderer.execute("downloads", newDownloadsView(root))
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	if strings.Contains(out, "aria-controls") {
		t.Errorf("empty directory emitted aria-controls with no list to point at:\n%s", out)
	}
}

// TestTorrentVerbsReportTheirOutcome pins that the action buttons have somewhere
// to put the server's reply. With hx-swap="none" the api-error fragment is
// discarded, so a failed Stop or Remove is completely silent while the whole
// error path exists and works.
//
// The target sits on the .torrent-actions wrapper and htmx inherits it down, so
// this matches an ancestor attribute rather than one per button. Asserting the
// inheritance properly would need an HTML parser, which is not worth a
// dependency; what matters is that the rendered row carries the target at all.
func TestTorrentVerbsReportTheirOutcome(t *testing.T) {
	u := newTestUI(t)
	body, err := u.renderer.execute("torrent-row", torrentView{
		InfoHash: "abc", Name: "N", Loaded: true, Started: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `hx-swap="none"`) {
		t.Errorf("a torrent verb still discards the server's response:\n%s", body)
	}
	if !strings.Contains(string(body), `hx-target="#omni-status"`) {
		t.Errorf("torrent verbs must target the status region:\n%s", body)
	}
}

// TestTreeDeleteReportsItsOutcome is the same contract for the download tree,
// which did not have it.
//
// The delete targeted #downloads with hx-swap="innerHTML" against a handler
// that answered 200 with NO body on success and 500 plain text on failure. So a
// successful delete blanked the panel until the next ping, and a failed one was
// completely silent — htmx does not swap a non-2xx response.
func TestTreeDeleteReportsItsOutcome(t *testing.T) {
	u := newTestUI(t)
	body, err := u.renderer.execute("downloads", newDownloadsView(&files.Node{
		Name: "root", IsDir: true,
		Children: []*files.Node{{Name: "a.txt", Size: 1}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)

	// Scoped to the element carrying hx-delete: #downloads appears elsewhere in
	// the tree markup, so a document-wide search would not say which element the
	// reply lands on.
	i := strings.Index(out, "hx-delete=")
	if i < 0 {
		t.Fatalf("no delete button rendered:\n%s", out)
	}
	tag := out[strings.LastIndex(out[:i], "<"):]
	tag = tag[:strings.Index(tag, ">")]

	if !strings.Contains(tag, `hx-target="#omni-status"`) {
		t.Errorf("the tree delete must report into the status region: <%s>", tag)
	}
	if strings.Contains(tag, `hx-target="#downloads"`) {
		t.Errorf("the tree delete swaps the server's reply into the tree panel, "+
			"which blanks it on success: <%s>", tag)
	}
}

// TestIconButtonsHaveAccessibleNames pins that no icon-only button ships without
// one. The glyphs are the whole text content, so a screen reader announces
// "▶ button" and "× button" — and one of them deletes files. title is a mouse
// tooltip and is not exposed as an accessible name.
func TestIconButtonsHaveAccessibleNames(t *testing.T) {
	u := newTestUI(t)
	body, err := u.renderer.execute("downloads", newDownloadsView(&files.Node{
		Name: "root", IsDir: true,
		Children: []*files.Node{{Name: "clip.mp4", Size: 1}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)

	var tags []string
	for i := 0; ; {
		j := strings.Index(out[i:], `class="icon-btn`)
		if j < 0 {
			break
		}
		j += i
		start := strings.LastIndex(out[:j], "<")
		end := j + strings.Index(out[j:], ">")
		tags = append(tags, out[start:end])
		i = end
	}

	// The fixture renders exactly one previewable file, so it must produce
	// exactly three icon buttons: preview, delete, confirm-delete. Asserting the
	// count is what stops this passing because the scan stopped matching, which
	// is otherwise indistinguishable from passing clean.
	if len(tags) != 3 {
		t.Fatalf("found %d icon-btn elements, want 3 — the scan has stopped "+
			"matching:\n%s", len(tags), out)
	}
	for _, tag := range tags {
		if !strings.Contains(tag, "aria-label=") {
			t.Errorf("icon-only button has no accessible name: <%s>", tag)
		}
	}
}

// TestStatusRegionIsOutsideSwapTargets is the reason the target above works.
// #omni-status lives in the Add panel; if it were inside #torrents the morph
// that follows an action would wipe the message the action just produced.
func TestStatusRegionIsOutsideSwapTargets(t *testing.T) {
	u := newTestUI(t)
	body, err := u.renderer.execute("page", pageView{
		Title:  "T",
		Config: engine.Config{DownloadDirectory: "/d", IncomingPort: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	status := strings.Index(out, `id="omni-status"`)
	torrents := strings.Index(out, `id="torrents"`)
	if status < 0 || torrents < 0 {
		t.Fatalf("expected both regions in the page (status=%d torrents=%d)", status, torrents)
	}
	if status > torrents {
		t.Error("#omni-status renders after #torrents opens; if it is inside that " +
			"swap target, every action erases its own result")
	}
}
