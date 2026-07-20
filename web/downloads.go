package web

import (
	"fmt"
	"hash/fnv"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/files"
)

// downloadsView is the download tree as the template wants it.
type downloadsView struct {
	Root      fsView
	Truncated bool
	Limit     int
}

func newDownloadsView(root *files.Node) downloadsView {
	return downloadsView{
		Root:      newRootView(root),
		Truncated: root.Truncated,
		Limit:     files.Limit,
	}
}

// fsView is one node of the download tree, prepared for rendering.
//
// Path is computed here, from the data, rather than derived in the browser from
// how deeply the markup happens to be nested. A path that depends on DOM
// structure breaks silently the moment the structure changes.
type fsView struct {
	Name     string
	Path     string // slash-separated, relative to the download root
	ID       string // stable, safe for an HTML id and a localStorage key
	Size     int64
	Modified string
	IsDir    bool
	Children []fsView
	// Preview is "", "video", "audio" or "image".
	Preview string
	// TopLevel marks a direct child of the download root. It decides the
	// default collapse state, and it is computed here rather than derived in
	// the browser from how deeply the markup is nested — structure-derived
	// state breaks silently when the structure changes, which is the same
	// reason Path is computed here.
	TopLevel bool
}

// newRootView adapts the walk's root. The root node carries the download
// directory's own name, but every path the UI emits must be relative to it, so
// its children start from an empty parent.
func newRootView(root *files.Node) fsView {
	v := fsView{Name: root.Name, IsDir: true}
	for _, c := range root.Children {
		if c == nil {
			continue
		}
		child := newFSView(c, "")
		child.TopLevel = true
		v.Children = append(v.Children, child)
	}
	sortChildren(&v)
	return v
}

func newFSView(n *files.Node, parent string) fsView {
	p := n.Name
	if parent != "" {
		p = parent + "/" + n.Name
	}
	v := fsView{
		Name:     n.Name,
		Path:     p,
		ID:       pathID(p),
		Size:     n.Size,
		Modified: humanAgo(n.Modified),
		IsDir:    n.IsDir,
		Preview:  previewKind(n.Name),
	}
	if n.IsDir {
		// A directory is never previewable, whatever its name looks like.
		v.Preview = ""
	}
	for _, c := range n.Children {
		if c == nil {
			continue
		}
		v.Children = append(v.Children, newFSView(c, p))
	}
	sortChildren(&v)
	return v
}

// sortChildren puts directories first, then orders by name — what a file
// browser is expected to do, and stable so the tree does not reshuffle between
// fetches.
func sortChildren(v *fsView) {
	sort.SliceStable(v.Children, func(i, j int) bool {
		a, b := v.Children[i], v.Children[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return a.Name < b.Name
	})
}

// pathID hashes a path into something usable as an HTML id and a localStorage
// key. Paths contain slashes, spaces and quotes; ids must not.
func pathID(p string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(p))
	return strconv.FormatUint(h.Sum64(), 36)
}

func previewKind(name string) string {
	switch ext(name) {
	case ".mp4", ".webm", ".ogv", ".mkv", ".mov":
		return "video"
	case ".mp3", ".ogg", ".wav", ".flac", ".m4a":
		return "audio"
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".svg":
		return "image"
	}
	return ""
}

func ext(name string) string { return strings.ToLower(path.Ext(name)) }

// The tree is fingerprinted in two parts because the two halves change on
// completely different timescales, and one signature over both made the ping
// fire every tick during any download — re-fetching the whole tree once a
// second against a regions.go comment promising the opposite.
//
// No pure function of the tree can fix that. Bucketing mtime to the minute
// loses a write that lands late in a bucket already reported; bucketing size
// loses a file that grows by less than a bucket and stops. The distinction that
// matters — "still changing" versus "settled" — is a property of the tree over
// *time*, not of the tree in hand, so it needs state. That lives in UI, under
// u.mu, which RenderDownloads already holds.

// downloadsSettle is how long in-progress size and mtime churn may accumulate
// before the tree is refreshed for it. A shape change bypasses it entirely.
//
// The value is a comfort setting, not a correctness one: the guarantee below
// holds for any positive duration.
const downloadsSettle = 30 * time.Second

// shapeSignature fingerprints what the tree *is*: names, directory-ness, and
// whether the walk was truncated. An entry appearing, disappearing or being
// renamed changes it, and those must reach the browser at once.
//
// Size and Modified are deliberately excluded — they change every second for
// every file a torrent is writing.
func shapeSignature(n *files.Node) uint64 {
	h := fnv.New64a()
	var walk func(*files.Node)
	walk = func(x *files.Node) {
		if x == nil {
			return
		}
		fmt.Fprintf(h, "%s|%t|%t;", x.Name, x.IsDir, x.Truncated)
		for _, c := range x.Children {
			walk(c)
		}
	}
	walk(n)
	return h.Sum64()
}

// contentSignature fingerprints what the tree *contains*: sizes and mtimes.
//
// A directory's Size is the recursive sum of its children (see files.list), so
// a single leaf write moves every ancestor — which is why this cannot be
// compared per tick.
func contentSignature(n *files.Node) uint64 {
	h := fnv.New64a()
	var walk func(*files.Node)
	walk = func(x *files.Node) {
		if x == nil {
			return
		}
		fmt.Fprintf(h, "%d|%d;", x.Size, x.Modified.UnixNano())
		for _, c := range x.Children {
			walk(c)
		}
	}
	walk(n)
	return h.Sum64()
}

// RenderDownloads emits the ping when the tree changed. It renders no HTML: the
// browser pulls the fragment.
//
// The content half is admitted at most once per downloadsSettle. This is not a
// plain throttle, and the difference is what closes the hole: once the tree
// settles, the stored signature keeps differing from the current one until the
// next admission, so the final size is always published within downloadsSettle
// of the last write. An idle tree that suddenly changes fires at once, because
// the last admission is long past.
func (u *UI) RenderDownloads(root *files.Node) {
	u.mu.Lock()
	defer u.mu.Unlock()

	now := u.now()
	if c := contentSignature(root); c != u.dlContentSig &&
		now.Sub(u.dlContentAt) >= downloadsSettle {
		u.dlContentSig = c
		u.dlContentAt = now
	}
	// Built here rather than in a template, and it has to be: html/template
	// *elides* HTML comments during escaping, so `{{define}}<!--{{.}}-->` renders
	// to nothing at all. The signature would vanish, store would see an
	// unchanged empty body after the first tick, and the browser would stop
	// re-fetching the tree — a silent stall, not an error.
	//
	// A comment is the right shape anyway: nothing swaps this event, it only
	// fires an hx-trigger, and a comment is element-shaped enough for
	// checkFragment's rule while rendering as nothing if it ever were swapped.
	// It starts with '<' by construction, which is that rule.
	body := []byte("<!--" +
		strconv.FormatUint(shapeSignature(root), 36) + "." +
		strconv.FormatUint(u.dlContentSig, 36) + "-->")
	if frame := u.renderer.store(downloadsChangedEvent, body); frame != nil {
		u.hub.broadcast(frame)
	}
}
