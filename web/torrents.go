package web

import (
	"path"
	"sort"
	"strings"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

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
	Rate int64
	Idle bool
	// Complete is decided here for the same reason fileView.Complete is: a
	// torrent at 99.999% renders as "100.00%" and must not be marked done.
	Complete bool
	Files    []fileView
}

// fileView is one file's progress row.
//
// Complete and InProgress are decided here rather than by comparing Percent to
// 0.0 and 100.0 in the template: float equality against a truncated percentage
// is exactly what the "arithmetic lives in the view model" rule exists for. A
// file at 99.999% renders as "100.00%" yet must not be marked done.
type fileView struct {
	Name       string
	Size       int64
	Percent    float32
	Complete   bool
	InProgress bool
}

func newFileView(f engine.File) fileView {
	return fileView{
		Name:       path.Base(f.Path),
		Size:       f.Size,
		Percent:    f.Percent,
		Complete:   f.Percent >= 100,
		InProgress: f.Percent > 0 && f.Percent < 100,
	}
}

func newTorrentView(t *engine.Torrent) torrentView {
	return torrentView{
		InfoHash:   t.InfoHash,
		Name:       displayName(t),
		Loaded:     t.Loaded,
		Started:    t.Started,
		Percent:    t.Percent,
		Downloaded: t.Downloaded,
		Size:       t.Size,
		Rate:       int64(t.DownloadRate),
		Idle:       t.DownloadRate == 0,
		Complete:   t.Percent >= 100,
	}
}

// newTorrentViewWithFiles is the variant used by the /fragments file table.
func newTorrentViewWithFiles(t *engine.TorrentWithFiles) torrentView {
	v := newTorrentView(&t.Torrent)
	v.Files = make([]fileView, 0, len(t.Files))
	for _, f := range t.Files {
		v.Files = append(v.Files, newFileView(f))
	}
	// Sorted by the constructor, not the handler, so an unsorted view is
	// unrepresentable. Sorted here rather than in the browser: it costs nothing
	// on this side and the client never has to re-sort on every update.
	sort.Slice(v.Files, func(i, j int) bool { return v.Files[i].Name < v.Files[j].Name })
	return v
}

// displayName falls back to a truncated infohash: a magnet has no name until
// its metadata arrives, and an empty <h3> reads as a broken row.
//
// It carries no "fetching" wording. The template already says that, on the same
// condition, and having both meant a metadata-less row announced it twice —
// once in the heading and once below it.
func displayName(t *engine.Torrent) string {
	if strings.TrimSpace(t.Name) != "" {
		return t.Name
	}
	return t.InfoHash[:min(8, len(t.InfoHash))] + "…"
}

// RenderTorrents renders the whole list as one region and broadcasts it if the
// bytes changed.
//
// One region rather than a per-torrent region each: a dynamic region name means
// the browser must create an element before its frames arrive, and htmx's SSE
// extension unregisters per-element listeners lazily, from inside the listener.
// With three fixed names, all present from the first frame, none of that
// bookkeeping is part of this program's correctness argument.
//
// The price is the full list on every tick that changes — ~2 KB per torrent. An
// idle server still sends nothing: the region is byte-gated by renderer.store.
func (u *UI) RenderTorrents(torrents map[string]*engine.Torrent) {
	u.mu.Lock()
	defer u.mu.Unlock()

	views := make([]torrentView, 0, len(torrents))
	for _, t := range torrents {
		views = append(views, newTorrentView(t))
	}
	// Stable order, or the list churns on every map iteration. Sorting by name
	// also means a magnet moves from its placeholder position to its real one
	// as soon as the name lands.
	sort.Slice(views, func(i, j int) bool {
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return views[i].InfoHash < views[j].InfoHash
	})

	u.emit(torrentListEvent, "torrent-list", views)
}
