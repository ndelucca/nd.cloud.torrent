package web

import (
	"fmt"
	"hash/fnv"
	"path"
	"sort"
	"strconv"

	"github.com/ndelucca/nd.cloud.torrent/files"
)

// downloadsChangedEvent is a content-free ping. The tree itself is fetched with
// hx-get rather than streamed: it changes on the order of minutes while torrent
// progress changes every second, and coupling them to the same 1 Hz push would
// re-ship the whole tree — and put every collapse state at risk — for a change
// that did not happen.
const downloadsChangedEvent = "downloads-changed"

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
		v.Children = append(v.Children, newFSView(c, ""))
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

func ext(name string) string {
	e := path.Ext(name)
	lower := make([]byte, len(e))
	for i := 0; i < len(e); i++ {
		c := e[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[i] = c
	}
	return string(lower)
}

// treeSignature is a cheap fingerprint of the tree's shape and contents. It is
// compared instead of rendering the tree every tick, because the fragment is
// only ever rendered on demand.
func treeSignature(n *files.Node) uint64 {
	h := fnv.New64a()
	var walk func(*files.Node)
	walk = func(x *files.Node) {
		if x == nil {
			return
		}
		fmt.Fprintf(h, "%s|%d|%d|%t;", x.Name, x.Size, x.Modified.UnixNano(), x.Truncated)
		for _, c := range x.Children {
			walk(c)
		}
	}
	walk(n)
	return h.Sum64()
}

// RenderDownloads emits the ping when the tree changed. It renders no HTML: the
// browser pulls the fragment.
func (u *UI) RenderDownloads(root *files.Node) {
	u.mu.Lock()
	defer u.mu.Unlock()

	sig := treeSignature(root)
	// Wrapped in a comment so the payload is still element-shaped; nothing
	// swaps this event, it only fires an hx-trigger.
	body := []byte("<!--" + strconv.FormatUint(sig, 36) + "-->")
	if frame := u.renderer.store(downloadsChangedEvent, body); frame != nil {
		u.hub.broadcast(frame)
	}
}
