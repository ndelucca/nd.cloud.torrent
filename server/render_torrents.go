package server

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
// authors are not coupled to engine.Torrent's field names, and so the file list
// can be dropped from the streamed row (it is fetched on demand instead).
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
	Files        []*engine.File
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
		Files:        t.Files,
	}
}

// displayName falls back to the infohash: a magnet has no name until its
// metadata arrives, and an empty <h3> reads as a broken row.
func displayName(t *engine.Torrent) string {
	if strings.TrimSpace(t.Name) != "" {
		return t.Name
	}
	return "Fetching metadata… " + t.InfoHash[:min(8, len(t.InfoHash))]
}

// renderTorrents implements the two-tier event scheme.
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
func (s *Server) renderTorrents(torrents map[string]*engine.Torrent) {
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
	for hash := range s.seenTorrents {
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
	membershipChanged := !sameHashes(current, s.seenTorrents)
	listFrame, err := s.renderer.render(torrentListEvent, "torrent-list", views)
	if err != nil {
		log.Printf("render torrent-list: %s", err)
		return
	}
	if membershipChanged {
		if listFrame == nil {
			// Bytes unchanged but membership moved: send the cached framing.
			listFrame = s.renderer.framed(torrentListEvent)
		}
		s.hub.broadcast(listFrame)
	}

	// Tier 2. Torrents whose row arrived with this tick's skeleton are skipped:
	// emitting an item event in the same flush as the event that creates its
	// element races the extension's registration (observed in-browser: missed
	// at 300 ms, delivered at 600 ms). They refresh on the next tick.
	for _, v := range views {
		if listFrame != nil && s.newThisTick(v.InfoHash) {
			continue
		}
		frame, err := s.renderer.render(torrentEventPrefix+v.InfoHash, "torrent-row", v)
		if err != nil {
			log.Printf("render torrent %s: %s", v.InfoHash, err)
			continue
		}
		s.hub.broadcast(frame)
	}

	for _, name := range removed {
		s.hub.broadcast(s.renderer.forget(name))
	}

	s.seenTorrents = current
}

// newThisTick reports whether this infohash had no region before this render,
// i.e. its row was created by the skeleton we just sent.
func (s *Server) newThisTick(hash string) bool { return !s.seenTorrents[hash] }

// sortFilesByPath orders a torrent's files for display. The AngularJS UI did
// this client-side with orderBy:'Path' on every digest.
func sortFilesByPath(files []*engine.File) {
	sort.Slice(files, func(i, j int) bool {
		if files[i] == nil || files[j] == nil {
			return files[j] == nil
		}
		return files[i].Path < files[j].Path
	})
}
