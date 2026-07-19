package web

import (
	"log"
	"sort"
	"strings"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

const (
	// torrentEventPrefix namespaces the per-torrent SSE regions.
	torrentEventPrefix = "torrent-"
	// torrentListEvent is the membership skeleton's region name.
	torrentListEvent = "torrent-list"
)

// sameHashes reports whether two infohash sets are equal.
func sameHashes(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// torrentView is one torrent as the templates want it. It exists so template
// authors are not coupled to engine.Torrent's field names.
//
// Files is populated only by newTorrentViewWithFiles, for the on-demand file
// table. The streamed row never renders it, and copying every file into every
// row once per second would be pure waste.
type torrentView struct {
	InfoHash   string
	Name       string
	Loaded     bool
	Started    bool
	Percent    float32
	Downloaded int64
	Size       int64
	// Rate is int64 so the `bytes` template func can format it; the engine
	// reports a float32 of bytes per second.
	Rate         int64
	DownloadRate float32
	Files        []engine.File
}

func newTorrentView(t *engine.Torrent) torrentView {
	return torrentView{
		InfoHash:     t.InfoHash,
		Name:         displayName(t),
		Loaded:       t.Loaded,
		Started:      t.Started,
		Percent:      t.Percent,
		Downloaded:   t.Downloaded,
		Size:         t.Size,
		Rate:         int64(t.DownloadRate),
		DownloadRate: t.DownloadRate,
	}
}

// newTorrentViewWithFiles is the variant used by the /fragments file table.
func newTorrentViewWithFiles(t *engine.Torrent) torrentView {
	v := newTorrentView(t)
	v.Files = t.Files
	return v
}

// displayName falls back to the infohash: a magnet has no name until its
// metadata arrives, and an empty <h3> reads as a broken row.
func displayName(t *engine.Torrent) string {
	if strings.TrimSpace(t.Name) != "" {
		return t.Name
	}
	return "Fetching metadata… " + t.InfoHash[:min(8, len(t.InfoHash))]
}

// RenderTorrents implements the two-tier event scheme.
//
// Tier 1, "torrent-list", is the membership skeleton. It is emitted only when
// the set of infohashes changes, because it is the only thing that can create
// or destroy a row: an element cannot listen for torrent-<hash> before it
// exists.
//
// Tier 2, "torrent-<hash>", carries the volatile row and is emitted only for
// torrents whose rendered output actually changed.
//
// Granularity is per torrent rather than per field on purpose. Per-field naming
// would mean ~1000 event names for 20 torrents of 50 files, whose SSE framing
// alone outweighs the payload, and 1000 morphs per second would jank the tab.
// Per torrent also keeps each frame well inside deflate's 32 KiB window, which
// is what makes a persistent gzip stream act as a cheap delta encoder.
func (u *UI) RenderTorrents(torrents map[string]*engine.Torrent) {
	u.mu.Lock()
	defer u.mu.Unlock()

	views := make([]torrentView, 0, len(torrents))
	for _, t := range torrents {
		views = append(views, newTorrentView(t))
	}
	// Stable order, or the skeleton churns on every map iteration.
	sort.Slice(views, func(i, j int) bool {
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return views[i].InfoHash < views[j].InfoHash
	})

	current := make(map[string]bool, len(views))
	for _, v := range views {
		current[v.InfoHash] = true
	}

	// Drop regions for torrents that are gone, and tell the browser explicitly.
	// htmx's SSE extension unregisters a per-element listener lazily, from
	// inside the listener itself; a name that simply stops being emitted leaks
	// both the listener and the detached DOM subtree it closes over.
	//
	// Removals are derived from the tracked infohash set, NOT by scanning region
	// names for the "torrent-" prefix: that prefix also matches the membership
	// region "torrent-list", so the scan forgot and re-sent the skeleton every
	// tick — and shipped an *empty* torrent-list event, which would have wiped
	// the whole list in the browser once per second.
	var removed []string
	for hash := range u.seen {
		if !current[hash] {
			removed = append(removed, torrentEventPrefix+hash)
		}
	}
	sort.Strings(removed)

	// Tier 1. The skeleton embeds each row's current content so a new row is
	// never briefly blank — which means its bytes change whenever any torrent's
	// progress does. So its emission is gated on the infohash *set*, not on the
	// rendered bytes: byte-gating would re-send the whole skeleton every second
	// and collapse the two tiers back into one.
	//
	// It is still re-rendered every tick, because the cache is what a late
	// subscriber receives on connect and a stale skeleton would show them stale
	// rows until each one next changed.
	membershipChanged := !sameHashes(current, u.seen)

	listFrame, err := u.renderer.render(torrentListEvent, "torrent-list", views)
	if err != nil {
		log.Printf("render torrent-list: %s", err)
		return
	}
	if membershipChanged {
		if listFrame == nil {
			// Bytes unchanged but membership moved: send the cached framing.
			listFrame = u.renderer.framed(torrentListEvent)
		}
		u.hub.broadcast(listFrame)
	}

	// Tier 2. Torrents whose row arrived with this tick's skeleton are skipped:
	// emitting an item event in the same flush as the event that creates its
	// element races the extension's registration (observed in-browser: missed
	// at 300 ms, delivered at 600 ms). They refresh on the next tick.
	for _, v := range views {
		if listFrame != nil && u.newThisTick(v.InfoHash) {
			continue
		}
		frame, err := u.renderer.render(torrentEventPrefix+v.InfoHash, "torrent-row", v)
		if err != nil {
			log.Printf("render torrent %s: %s", v.InfoHash, err)
			continue
		}
		u.hub.broadcast(frame)
	}

	for _, name := range removed {
		u.hub.broadcast(u.renderer.forget(name))
	}

	// Last, and deliberately not deferred. seen is "what the browsers have been
	// told", so it may only advance once they have been told. Advancing it from
	// a defer meant an early return on a render failure — when nothing was sent
	// at all — still marked the tick as delivered: the forget events computed
	// above were skipped, the next tick saw an empty removal set, and the
	// regions of deleted torrents were never forgotten. They grew without bound
	// in the renderer and were replayed to every new subscriber, resurrecting
	// rows for torrents that no longer exist.
	u.seen = current
}

// newThisTick reports whether this infohash had no region before this render,
// i.e. its row was created by the skeleton we just sent.
func (u *UI) newThisTick(hash string) bool { return !u.seen[hash] }

// sortFilesByPath orders a torrent's files for display.
func sortFilesByPath(files []engine.File) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
}
