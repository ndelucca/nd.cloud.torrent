package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// State is the server's shared snapshot: what the page renderer reads, and what
// GET /api/state serves as JSON.
//
// It no longer publishes anything itself. Under velox, State.Update both
// mutated and pushed, and the single worst bug in this codebase's history was a
// Push() that silently did nothing — so rather than keep a method whose name
// implies delivery it cannot perform, publishing is now unambiguously the
// render loop's job (see renderRegions, renderTorrents, renderDownloads) and
// this type only guards the data.
//
// Callers that mutate outside the render loop should call Server.kick so the
// change is rendered promptly instead of on the next tick.
type State struct {
	mu sync.Mutex

	Config    engine.Config
	Downloads *fsNode
	Torrents  map[string]*engine.Torrent
	// ConnectedUsers is a count, not a roster: the previous map exposed every
	// client's IP:port to every other client.
	ConnectedUsers int
	Stats          Stats
}

// Stats is the static-plus-sampled telemetry block.
type Stats struct {
	Title   string
	Version string
	Runtime string
	Uptime  time.Time
	System  SystemStats
}

// Update mutates the state under lock. Every mutation must go through here.
func (s *State) Update(fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

// Read runs fn against the state under lock, for callers that only observe.
func (s *State) Read(fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

// serveState publishes the state as JSON.
//
// The velox /sync endpoint used to be a machine-readable feed of exactly this
// document, so scripts and monitoring could consume it. Replacing the UI with
// HTML fragments would have removed that with no replacement; this keeps it,
// costs almost nothing, and is what makes the server debuggable when a fragment
// renders wrong.
func (s *Server) serveState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body []byte
	var err error
	s.state.Read(func(st *State) {
		// Marshalled under the lock: Torrents and Downloads are pointers the
		// render loop replaces wholesale.
		body, err = json.Marshal(st)
	})
	if err != nil {
		http.Error(w, "Failed to encode state", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}
