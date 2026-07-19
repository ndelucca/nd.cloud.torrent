package web

// The SSE region names and the fragment routes, in one place.
//
// These strings are the wire protocol between this package's templates, its
// renderer and the browser: a region name appears in a {{define}}, in an
// sse-swap attribute and in a Render* call; a fragment path appears in an
// hx-get attribute and in the server's route table. Declaring them once is what
// makes a rename either complete or a compile error.
const (
	// torrentListEvent carries the whole torrent list. There is deliberately no
	// per-torrent region: a dynamic region name means the browser must create an
	// element before its frames arrive, and htmx's SSE extension unregisters
	// per-element listeners lazily, from inside the listener. Both are the
	// library's bookkeeping, and neither is this program's problem while every
	// region name is fixed and present from the first frame.
	torrentListEvent = "torrent-list"
	// statsEvent carries the host stats region.
	statsEvent = "stats"
	// downloadsChangedEvent is a content-free ping; the tree itself is fetched
	// with hx-get. Streaming it would re-ship the whole tree once a second for a
	// change nobody can see, since torrent progress moves every tick while the
	// tree's shape moves on the order of minutes.
	//
	// The payload is a shape signature and a rate-limited content signature —
	// see web.RenderDownloads. Hashing both together fires this every tick of
	// every download, which is the thing the paragraph above claims it does not.
	downloadsChangedEvent = "downloads-changed"
)

// KnownRoutes are the paths the templates ask this package's handlers for. The
// server asserts that each one resolves on its mux, which is what ties an
// hx-get attribute to a route that answers it.
var KnownRoutes = []string{
	"/events",
	"/fragments/downloads",
	"/fragments/torrent/{hash}/files",
}

// StaticAssets are the concrete files page.html loads from the embedded asset
// FS.
//
// They are separate from KnownRoutes because they are asserted differently. A
// KnownRoutes entry is a pattern, checked by resolving it on the mux; these are
// real files, checked by fetching them. Resolution alone proves nothing here —
// the server mounts "GET /css/" and "GET /js/" as prefixes, which match any
// path beneath them, so a renamed stylesheet or a vendor upgrade that drops a
// file would resolve happily and 404 in the browser.
var StaticAssets = []string{
	"/cloud-favicon.png",
	"/css/ct.css",
	"/js/ct.js",
	"/js/vendor/alpine.min.js",
	"/js/vendor/htmx.min.js",
	"/js/vendor/idiomorph-ext.min.js",
	"/js/vendor/sse.js",
}
