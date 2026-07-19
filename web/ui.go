// Package web renders the user interface and pushes it to browsers.
//
// It owns the templates, the view models, the SSE hub and every handler that
// produces HTML. It reads the world through the closures in Deps and never
// holds a reference to the server, the engine or any shared mutable state —
// that one-way arrow is what keeps rendering testable without standing up a
// torrent client.
package web

import (
	"sync"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/files"
)

// Deps is everything the UI needs from the rest of the program, as functions
// over value types. Closures rather than the objects behind them: it is what
// stops this package acquiring a dependency on the server, and it lets a test
// supply three literals instead of a running engine.
type Deps struct {
	Title   string
	Version string
	Runtime string
	Uptime  time.Time

	// Torrents and Tree are read once per render tick.
	Torrents func() map[string]*engine.Torrent
	Tree     func() *files.Node
	// TorrentFiles fetches one torrent with its file table, for the on-demand
	// file fragment. A keyed lookup rather than a scan of a full snapshot: the
	// row being expanded is one row.
	//
	// It returns a bool, not an error, because this package does not see errors
	// — both failure modes render the same fragment either way.
	TorrentFiles func(hash string) (*engine.TorrentWithFiles, bool)
	// Config backs the settings form.
	Config func() engine.Config
	// Kick asks the render loop to run before its next tick, so a browser
	// connecting mid-tick does not wait a full second for its first frame.
	Kick func()
}

// UI renders regions and fans them out to connected browsers.
type UI struct {
	renderer *renderer
	hub      *hub
	deps     Deps

	// mu serializes rendering. The server's poll and stats loops both call in,
	// and unsynchronized they can broadcast samples in the opposite order to the
	// one they were taken in, leaving browsers on the older one. It also covers
	// the two fields below.
	mu sync.Mutex

	// The last admitted content signature of the download tree and when it was
	// admitted. See RenderDownloads for why the tree needs state across ticks
	// while every other region is a pure function of its argument.
	dlContentSig uint64
	dlContentAt  time.Time

	// now is time.Now, replaced in tests. A seam rather than a Deps field: the
	// rate limit it drives is this package's own business, and Deps is the
	// contract with the server.
	now func() time.Time
}

// New parses the templates and returns a UI.
//
// The parse error is returned rather than panicked: the recursive tree template
// can fail html/template's contextual autoescaper at parse time, and a
// package-level Must would turn a template edit into a startup panic.
func New(d Deps) (*UI, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &UI{
		renderer: newRenderer(tmpl),
		hub:      newHub(),
		deps:     d,
		now:      time.Now,
	}, nil
}

// Watchers reports how many browsers are connected. The server's poll loop is
// gated on this: rendering for nobody is pure waste.
func (u *UI) Watchers() int { return u.hub.count() }

// Close releases every connected browser and latches the hub shut. See
// hub.close for why this is not the same as dropping a stalled subscriber.
func (u *UI) Close() { u.hub.close() }

func (u *UI) kick() {
	if u.deps.Kick != nil {
		u.deps.Kick()
	}
}
