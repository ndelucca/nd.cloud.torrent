package web

// The SSE region names and the fragment routes, in one place.
//
// These strings are the wire protocol between this package's templates, its
// renderer and the browser: a region name appears in a {{define}}, in an
// sse-swap attribute and in a Render* call, and a fragment path appears in an
// hx-get attribute and in the server's route table. They were previously two
// consts, one bare literal used twice, and several inline strings — so a rename
// could be applied to three of the five and still compile.
const (
	// torrentEventPrefix namespaces the per-torrent SSE regions.
	torrentEventPrefix = "torrent-"
	// torrentListEvent is the membership skeleton's region name.
	torrentListEvent = "torrent-list"
	// statsEvent carries the host stats region.
	statsEvent = "stats"
	// downloadsChangedEvent is a content-free ping. The tree itself is fetched
	// with hx-get rather than streamed: it changes on the order of minutes while
	// torrent progress changes every second, and coupling them to the same 1 Hz
	// push would re-ship the whole tree — and put every collapse state at risk —
	// for a change that did not happen.
	downloadsChangedEvent = "downloads-changed"
)

// KnownRoutes are the paths the templates ask this package's handlers for. The
// server asserts that each one resolves on its mux, which is the only thing
// tying an hx-get attribute to a route that answers it.
var KnownRoutes = []string{
	"/events",
	"/fragments/downloads",
	"/fragments/torrent/{hash}/files",
}
