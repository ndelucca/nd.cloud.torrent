package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStateIsServedAsJSON keeps the machine-readable feed alive.
//
// The UI is HTML all the way down, so /api/state is the only way to see what
// the server actually believes — for scripts, for monitoring, and for debugging
// a fragment that renders wrong.
func TestStateIsServedAsJSON(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
	for _, field := range []string{"Torrents", "Downloads", "Stats", "ConnectedUsers"} {
		if _, ok := doc[field]; !ok {
			t.Errorf("state document missing %q", field)
		}
	}
	// Config was dropped: the engine owns it, and a second copy could drift.
	if _, ok := doc["Config"]; ok {
		t.Error("Config must not be republished here; it is the engine's")
	}
}

// TestStateIsLiveWithoutWatchers is the regression test for a feed that only
// worked when somebody was looking at it.
//
// Torrents and Downloads used to be filled in by the poll loop, which is gated
// on watchers() > 0, and served from that snapshot. So /api/state — the
// endpoint README offers for scripts and monitoring — answered with nulls
// unless a browser happened to have the UI open.
func TestStateIsLiveWithoutWatchers(t *testing.T) {
	s := newTestServer(t)
	if s.watchers() != 0 {
		t.Fatalf("watchers = %d, want 0: this test is about the unwatched case", s.watchers())
	}

	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/state", nil))

	var doc struct {
		Torrents  map[string]any
		Downloads *struct{ Name string }
		Stats     struct{ Version string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
	if doc.Torrents == nil {
		t.Error("Torrents is null with nobody watching; it must be read from the engine")
	}
	if doc.Downloads == nil {
		t.Error("Downloads is null with nobody watching; it must be read from the filesystem")
	}
	if doc.Stats.Version != "test" {
		t.Errorf("Stats.Version = %q, want %q", doc.Stats.Version, "test")
	}
}
