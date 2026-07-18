package server

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

// TestTreePathsAreServerComputed covers the replacement for the AngularJS
// $parent.$parent scope walk, which derived each node's path from the exact
// nesting the directives produced rather than from the data — so changing a
// directive silently broke every path.
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

// TestTreeSignatureDetectsChange covers the ping's change detection. The tree is
// pulled, not streamed, so this signature is the only thing that tells a browser
// to re-fetch.
func TestTreeSignatureDetectsChange(t *testing.T) {
	a := dir("d", file("a.txt", 10))
	same := dir("d", file("a.txt", 10))
	same.Modified = a.Modified
	same.Children[0].Modified = a.Children[0].Modified
	if treeSignature(a) != treeSignature(same) {
		t.Error("signature is not stable for equal input")
	}

	grown := dir("d", file("a.txt", 20)) // same name, new size
	grown.Modified = a.Modified
	grown.Children[0].Modified = a.Children[0].Modified
	if treeSignature(a) == treeSignature(grown) {
		t.Error("a file growing must change the signature — this is what makes " +
			"a downloading file's size update in the browser")
	}

	added := dir("d", file("a.txt", 10), file("b.txt", 1))
	added.Modified = a.Modified
	added.Children[0].Modified = a.Children[0].Modified
	if treeSignature(a) == treeSignature(added) {
		t.Error("a new file must change the signature")
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
