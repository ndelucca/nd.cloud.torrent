package web

import (
	"github.com/ndelucca/nd.cloud.torrent/files"
	"strings"
	"testing"
	"time"
)

func dir(name string, children ...*files.Node) *files.Node {
	return &files.Node{Name: name, IsDir: true, Children: children, Modified: time.Now()}
}
func file(name string, size int64) *files.Node {
	return &files.Node{Name: name, Size: size, Modified: time.Now()}
}

// TestTreePathsAreServerComputed pins that each node's path comes from the data
// and is relative to the download root.
//
// Deriving it in the browser instead means deriving it from how deeply the
// markup is nested, which breaks silently the moment the markup changes — and
// these paths are what /download/ URLs are built from.
func TestTreePathsAreServerComputed(t *testing.T) {
	root := dir("downloads",
		dir("Show", dir("S01", file("ep01.mkv", 100), file("ep02.mkv", 200))),
		file("readme.txt", 10),
	)
	v := newRootView(root)

	// The download directory's own name must not appear in any path: paths are
	// relative to the download root and go straight into /download/<path>.
	var paths []string
	var walk func(fsView)
	walk = func(n fsView) {
		if n.Path != "" {
			paths = append(paths, n.Path)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(v)

	want := map[string]bool{
		"Show":              true,
		"Show/S01":          true,
		"Show/S01/ep01.mkv": true,
		"Show/S01/ep02.mkv": true,
		"readme.txt":        true,
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "downloads") {
			t.Errorf("path %q includes the download directory name", p)
		}
		if !want[p] {
			t.Errorf("unexpected path %q", p)
		}
		delete(want, p)
	}
	for p := range want {
		t.Errorf("missing path %q", p)
	}
}

// TestTreeDirectoriesFirst pins the ordering, which must be stable so the tree
// does not reshuffle between fetches.
func TestTreeDirectoriesFirst(t *testing.T) {
	v := newRootView(dir("d",
		file("zzz.txt", 1),
		file("aaa.txt", 1),
		dir("Beta"),
		dir("Alpha"),
	))
	var got []string
	for _, c := range v.Children {
		got = append(got, c.Name)
	}
	want := []string{"Alpha", "Beta", "aaa.txt", "zzz.txt"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// TestEmptyDirectoryIsStillADirectory guards the inference that used to be made
// from len(Children): an empty folder would have rendered as a file link.
func TestEmptyDirectoryIsStillADirectory(t *testing.T) {
	v := newRootView(dir("d", dir("empty")))
	if len(v.Children) != 1 {
		t.Fatal("expected one child")
	}
	if !v.Children[0].IsDir {
		t.Error("an empty directory must still be a directory")
	}
	if v.Children[0].Preview != "" {
		t.Error("a directory is never previewable")
	}
}

// alignTimes copies a's timestamps onto b so a comparison isolates whatever the
// caller actually varied. dir/file stamp time.Now.
func alignTimes(a, b *files.Node) {
	b.Modified = a.Modified
	for i := range b.Children {
		if i < len(a.Children) {
			alignTimes(a.Children[i], b.Children[i])
		}
	}
}

// TestTreeShapeSignature covers the half that must fire immediately: an entry
// appearing, disappearing or being renamed.
//
// It must NOT move when a file merely grows, and that is the whole point. One
// signature over shape and content together changed on every tick of every
// download, so the browser re-fetched the entire tree once a second — against a
// comment in regions.go promising it changed on the order of minutes.
func TestTreeShapeSignature(t *testing.T) {
	a := dir("d", file("a.txt", 10))

	same := dir("d", file("a.txt", 10))
	alignTimes(a, same)
	if shapeSignature(a) != shapeSignature(same) {
		t.Error("shape signature is not stable for equal input")
	}

	grown := dir("d", file("a.txt", 20))
	if shapeSignature(a) != shapeSignature(grown) {
		t.Error("a file growing must NOT change the shape signature; that is " +
			"what stops the ping firing every tick during a download")
	}

	added := dir("d", file("a.txt", 10), file("b.txt", 1))
	alignTimes(a, added)
	if shapeSignature(a) == shapeSignature(added) {
		t.Error("a new file must change the shape signature")
	}

	renamed := dir("d", file("renamed.txt", 10))
	alignTimes(a, renamed)
	if shapeSignature(a) == shapeSignature(renamed) {
		t.Error("a rename must change the shape signature")
	}

	truncated := dir("d", file("a.txt", 10))
	alignTimes(a, truncated)
	truncated.Truncated = true
	if shapeSignature(a) == shapeSignature(truncated) {
		t.Error("hitting files.Limit must change the shape signature; the UI " +
			"says so and the browser has to be told to re-fetch")
	}
}

// TestTreeContentSignature covers the other half: sizes and mtimes, which is
// what the settle window rate-limits.
func TestTreeContentSignature(t *testing.T) {
	a := dir("d", file("a.txt", 10))

	same := dir("d", file("a.txt", 10))
	alignTimes(a, same)
	if contentSignature(a) != contentSignature(same) {
		t.Error("content signature is not stable for equal input")
	}

	grown := dir("d", file("a.txt", 20))
	alignTimes(a, grown)
	if contentSignature(a) == contentSignature(grown) {
		t.Error("a file growing must change the content signature, or the final " +
			"size would never reach the browser")
	}
}

// TestDownloadsPingIsRateLimitedDuringChurn is why the signature was split.
//
// The old signature hashed Size and Modified for every node, so any active
// download changed it on every poll tick and the browser re-fetched the whole
// tree once a second. This pins three things at once: churn is rate-limited, a
// shape change is not, and the last write is still published once things settle.
func TestDownloadsPingIsRateLimitedDuringChurn(t *testing.T) {
	u := newTestUI(t)
	clock := time.Now()
	u.now = func() time.Time { return clock }

	pings := 0
	last := ""
	tick := func(n *files.Node) {
		u.RenderDownloads(n)
		if got := string(u.renderer.framed(downloadsChangedEvent)); got != last {
			last = got
			pings++
		}
		clock = clock.Add(time.Second)
	}

	// A file growing once a second for a minute, the way a download does.
	base := dir("d", file("a.txt", 0))
	for i := 1; i <= 60; i++ {
		grown := dir("d", file("a.txt", int64(i)))
		alignTimes(base, grown)
		tick(grown)
	}
	// 60 seconds of churn at downloadsSettle=30s admits the first reading and
	// then one per window. Anything near 60 means the rate limit is not working.
	if want := int(60*time.Second/downloadsSettle) + 1; pings > want {
		t.Errorf("%d pings over 60s of churn, want at most %d — the tree is "+
			"being re-fetched on churn again", pings, want)
	}

	// A shape change must not wait for the window.
	before := pings
	added := dir("d", file("a.txt", 60), file("new.txt", 1))
	alignTimes(base, added)
	tick(added)
	if pings != before+1 {
		t.Errorf("a new file did not ping immediately (%d -> %d); shape changes "+
			"must bypass the settle window", before, pings)
	}

	// A last write lands inside the window, so it is not admitted and pings
	// nothing.
	final := dir("d", file("a.txt", 999), file("new.txt", 1))
	alignTimes(added, final)
	before = pings
	tick(final)
	if pings != before {
		t.Fatalf("a write inside the settle window pinged (%d -> %d); the rate "+
			"limit is not holding", before, pings)
	}

	// Then writing stops. Nothing about the tree changes from here, but the
	// stored signature still differs from it, so the next admission publishes
	// the final size. This is the difference between this and a plain throttle,
	// which would drop that write and leave the browser permanently stale.
	before = pings
	clock = clock.Add(downloadsSettle)
	tick(final)
	if pings != before+1 {
		t.Errorf("the final size was never published (%d -> %d); the settle "+
			"window must not swallow the last write", before, pings)
	}
}

// TestPreviewKind covers the extension sniffing, including case.
func TestPreviewKind(t *testing.T) {
	cases := map[string]string{
		"a.mkv": "video", "a.MP4": "video",
		"a.mp3": "audio", "a.FLAC": "audio",
		"a.png": "image", "a.JPEG": "image",
		"a.txt": "", "a": "", "a.exe": "",
	}
	for name, want := range cases {
		if got := previewKind(name); got != want {
			t.Errorf("previewKind(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestTreeRecursionRenders is the parse-time guard for the recursive template.
// html/template's contextual autoescaper must reach a fixed point over a
// self-referential template; if fsnode ever recursed from inside an attribute
// it would fail with ErrOutputContext, and it fails at PARSE time, so nothing
// would start.
func TestTreeRecursionRenders(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("templates do not parse (recursive fsnode may have broken the "+
			"autoescaper's fixed point): %v", err)
	}
	view := struct {
		Root      fsView
		Truncated bool
		Limit     int
	}{Root: newRootView(dir("d", dir("deep", dir("deeper", file("x.mkv", 1))))), Limit: 1000}

	var sb strings.Builder
	if err := tmpl.ExecuteTemplate(&sb, "downloads", view); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	for _, want := range []string{"deep", "deeper", "x.mkv", `href="/download/deep/deeper/x.mkv"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered tree missing %q", want)
		}
	}
	if strings.Contains(out, "ZgotmplZ") {
		t.Error("ZgotmplZ in output: a value reached a URL or CSS context the " +
			"autoescaper could not prove safe")
	}
}
