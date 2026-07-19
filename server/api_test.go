package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFailedActionStillKicks covers a partially applied action being invisible.
//
// The render loop was woken only when the API call returned nil. But an action
// can apply partially and still report an error — uploading five torrents where
// two are malformed adds three and returns 400 — so those three stayed off the
// page until the next tick. kick is coalesced and floored, so waking it
// unconditionally costs at most one extra render.
func TestFailedActionStillKicks(t *testing.T) {
	s := newTestServer(t)
	// Drain anything startup left pending, so the assertion is about this call.
	select {
	case <-s.kickCh:
	default:
	}

	req := httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader("not-a-magnet"))
	req.Header.Set("Origin", "http://"+req.Host)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code < 400 {
		t.Fatalf("setup: expected the action to fail, got %d", rec.Code)
	}
	select {
	case <-s.kickCh:
	default:
		t.Fatal("a failed action left the render loop asleep; a partial success " +
			"would stay invisible until the next tick")
	}
}
