package web

// The SSE region names and the fragment routes, in one place.
//
// These strings are the wire protocol between this package's templates, its
// renderer and the browser: a region name appears in a {{define}}, in an
// sse-swap attribute and in a Render* call; a fragment path appears in an
// hx-get attribute and in the server's route table. Declaring them once is what
// makes a rename either complete or a compile error.
const (
	// torrentEventPrefix namespaces the per-torrent SSE regions.
	torrentEventPrefix = "torrent-"
	// torrentListEvent is the membership skeleton's region name.
	torrentListEvent = "torrent-list"
	// statsEvent carries the host stats region.
	statsEvent = "stats"
	// downloadsChangedEvent is a content-free ping; the tree itself is fetched
	// with hx-get. It changes on the order of minutes while torrent progress
	// changes every second, so streaming it would re-ship the whole tree — and
	// risk every collapse state — for a change that did not happen.
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
