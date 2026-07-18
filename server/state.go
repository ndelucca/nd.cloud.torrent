package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// sampledStats holds the one thing the server genuinely has to remember between
// ticks: the most recent host sample.
//
// Everything else the UI and /api/state show is derived — torrents from the
// engine, the tree from the filesystem, the config from the engine, the viewer
// count from the hub — so it is read from its owner at the moment it is needed
// rather than copied into a shared snapshot. The snapshot this replaces was
// written by the poll loop and read by nothing but the JSON encoder, which is
// how /api/state came to serve nulls whenever no browser was connected.
type sampledStats struct {
	mu     sync.Mutex
	system SystemStats
}

func (s *sampledStats) set(v SystemStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.system = v
}

func (s *sampledStats) get() SystemStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.system
}

// stateDocument is the JSON contract of GET /api/state.
//
// It is declared here rather than marshalled from an internal struct so that
// rearranging the server's own fields cannot silently change the wire format.
// Exported field names are the contract; renaming one breaks any script
// consuming it.
type stateDocument struct {
	Torrents       map[string]*engine.Torrent
	Downloads      *fsNode
	ConnectedUsers int
	Stats          statsDocument
}

type statsDocument struct {
	Title   string
	Version string
	Runtime string
	Uptime  time.Time
	System  SystemStats
}

// serveState publishes the server's state as JSON.
//
// The velox /sync endpoint used to be a machine-readable feed of exactly this
// document, so scripts and monitoring could consume it. Replacing the UI with
// HTML fragments would have removed that with no replacement; this keeps it,
// and is what makes the server debuggable when a fragment renders wrong.
//
// Every field is gathered at request time. That costs a directory walk per
// request — bounded by fileNumberLimit, the same walk the poll loop does every
// second while anyone is watching — and buys a document that is correct for a
// caller who is not a browser.
func (s *Server) serveState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Not allowed", http.StatusMethodNotAllowed)
		return
	}
	doc := stateDocument{
		Torrents:       s.engine.GetTorrents(),
		Downloads:      s.listFiles(),
		ConnectedUsers: s.watchers(),
		Stats: statsDocument{
			Title:   s.opts.Title,
			Version: s.version,
			Runtime: s.goRuntime,
			Uptime:  s.startedAt,
			System:  s.stats.get(),
		},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		http.Error(w, "Failed to encode state", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}
