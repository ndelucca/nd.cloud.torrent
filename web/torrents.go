package web

import (
	"log"
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
	// reports a float32 of bytes per second. There used to be a DownloadRate
	// alongside it holding the same number as a float, purely so the template
	// could compare it to 0.0.
	Rate  int64
	Idle  bool
	Files []fileView
}

// fileView is one file's progress row.
//
// Complete and InProgress are decided here rather than by comparing Percent to
// 0.0 and 100.0 in the template. web/CLAUDE.md already required arithmetic to
// live in the view model, and float equality against a truncated percentage is
// exactly the sort of thing that rule exists for: a file at 99.999% renders as
// "100.00%" yet must not be marked done.
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
	}
}

// newTorrentViewWithFiles is the variant used by the /fragments file table.
func newTorrentViewWithFiles(t *engine.Torrent) torrentView {
	v := newTorrentView(t)
	v.Files = make([]fileView, 0, len(t.Files))
	for _, f := range t.Files {
		v.Files = append(v.Files, newFileView(f))
	}
	// Sorted by the constructor, not by the handler. It was the handler's job
	// once and was silently dropped when that handler was collapsed into a
	// helper — nothing failed, because nothing asserted the order. Building an
	// unsorted view is now unrepresentable.
	//
	// Sorted here rather than in the browser: it costs nothing on this side and
	// the client never has to re-sort on every update.
	sortFilesByName(v.Files)
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

// RenderTorrents renders the whole list as one region and broadcasts it if the
// bytes changed.
//
// One region, not two tiers. The previous scheme emitted a membership skeleton
// plus a per-torrent region, which needed cross-tick state (`seen`), a rule for
// not emitting an item event in the same flush as the element that hosts it, a
// final empty event per disappearing region so htmx's SSE extension would
// collect its listener, and a snapshot ordering that put membership first.
// About 110 lines, all of it compensating for the lifecycle of *dynamic region
// names*.
//
// The argument for that complexity was frame size, and it does not hold: the
// SSE stream is excluded from gzip outright (an SSE frame is below gzhttp's
// 1 KiB threshold, so compressing buffers the first event forever), so there was
// never a persistent deflate window for small frames to fit inside. What it cost
// was real — three live bugs and a permanent coupling to one library's
// listener bookkeeping.
//
// With three fixed region names, all present from the first frame, that
// bookkeeping stops being part of this program's correctness argument. The price
// is the full list on every tick that changes: ~2 KB per torrent, so ~42 KB/s
// with 20 active torrents versus ~21 KB/s before. An idle server still sends
// nothing, because the whole region is byte-gated by renderer.store.
func (u *UI) RenderTorrents(torrents map[string]*engine.Torrent) {
	u.mu.Lock()
	defer u.mu.Unlock()

	views := make([]torrentView, 0, len(torrents))
	for _, t := range torrents {
		views = append(views, newTorrentView(t))
	}
	// Stable order, or the list churns on every map iteration. Sorting by name
	// also means a magnet moves from its "Fetching metadata…" position to its
	// real one as soon as the name lands — the old scheme could not, because it
	// gated the skeleton on the infohash *set*, which does not change when a
	// name arrives.
	sort.Slice(views, func(i, j int) bool {
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return views[i].InfoHash < views[j].InfoHash
	})

	frame, err := u.renderer.render(torrentListEvent, "torrent-list", views)
	if err != nil {
		log.Printf("render torrent-list: %s", err)
		return
	}
	u.hub.broadcast(frame)
}

// sortFilesByName orders a torrent's files for display.
func sortFilesByName(files []fileView) {
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
}
