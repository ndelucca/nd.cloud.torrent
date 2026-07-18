package server

import (
	"sync"
	"time"

	"github.com/jpillora/velox"
	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// State is the single source of truth pushed to browsers. Exported field names
// are the wire format consumed by the web UI — renaming one breaks the client.
//
// It must be mutated through Update, which takes the lock and pushes. Lock and
// Unlock are exported only because velox.Marshal type-asserts to sync.Locker to
// guard serialization; do not call them directly.
type State struct {
	velox.State
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

// Lock implements sync.Locker for velox.Marshal. Prefer Update.
func (s *State) Lock() { s.mu.Lock() }

// Unlock implements sync.Locker for velox.Marshal. Prefer Update.
func (s *State) Unlock() { s.mu.Unlock() }

// Update mutates the state under lock and then notifies connected clients.
// Every mutation must go through here.
func (s *State) Update(fn func(*State)) {
	s.mu.Lock()
	fn(s)
	s.mu.Unlock()
	s.Push()
}

// Read runs fn against the state under lock, for callers that only observe.
func (s *State) Read(fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}
