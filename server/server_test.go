package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// freePort returns a port that is currently unbound.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()

	// Seed a config file so the engine binds a free port and downloads into a
	// scratch directory, rather than the shipped defaults.
	cfg := fmt.Sprintf(`{"DownloadDirectory":%q,"IncomingPort":%d,"EnableUpload":true,"AutoStart":true}`,
		filepath.Join(dir, "downloads"), freePort(t))
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	o := DefaultOptions()
	o.Port = freePort(t)
	o.ConfigPath = configPath

	s, err := New(o, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestStateIsServedAsJSON keeps the machine-readable feed alive.
//
// The velox /sync endpoint used to publish exactly this document, so scripts
// and monitoring could consume it. Replacing the UI with HTML fragments would
// have dropped that with no replacement; /api/state is the replacement, and it
// is also what makes a mis-rendering fragment debuggable.
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
	for _, field := range []string{"Config", "Torrents", "Stats", "ConnectedUsers"} {
		if _, ok := doc[field]; !ok {
			t.Errorf("state document missing %q", field)
		}
	}
}

// TestConcurrentEventStreams guards the remote-DoS fix that predates the
// migration: the old code wrote and deleted a per-connection map entry from
// each HTTP goroutine without holding the mutex, and two simultaneous clients
// produced "fatal error: concurrent map writes", which Go cannot recover from.
//
// The map is gone — connections are counted, not rostered — but the shape of
// the risk moved to the SSE hub, so the test moved with it. Run under -race.
func TestConcurrentEventStreams(t *testing.T) {
	s := newTestServer(t)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			sub := s.hub.subscribe()
			s.hub.unsubscribe(sub)
		}()
		go func() {
			defer wg.Done()
			s.hub.broadcast([]byte("event: x\ndata: <i>y</i>\n\n"))
		}()
		go func() {
			defer wg.Done()
			s.state.Update(func(st *State) { st.ConnectedUsers = s.watchers() })
			s.renderRegions()
		}()
	}
	wg.Wait()
}

// TestAPIRejectsCrossOrigin covers the CSRF fix. /api/* takes text/plain bodies,
// which browsers send cross-origin with no preflight, so any page could have
// POSTed /api/configure to repoint the download root at "/".
func TestAPIRejectsCrossOrigin(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	cases := []struct {
		name       string
		headers    map[string]string
		wantStatus int
	}{
		{"no origin (curl)", nil, http.StatusOK},
		{"same-origin fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"foreign origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := strings.NewReader("start:" + strings.Repeat("ab", 20))
			r := httptest.NewRequest(http.MethodPost, "/api/torrent", body)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if c.wantStatus == http.StatusForbidden {
				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", w.Code)
				}
				return
			}
			// Allowed through origin checking: it should fail on the torrent
			// being absent (404), not on the origin (403).
			if w.Code == http.StatusForbidden {
				t.Fatalf("same-origin request was rejected as cross-origin")
			}
		})
	}
}

// TestAPIStatusCodes covers the fix for every failure being a flat 400.
func TestAPIStatusCodes(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"missing torrent", http.MethodPost, "/api/torrent", "start:" + strings.Repeat("ab", 20), http.StatusNotFound},
		{"unknown action", http.MethodPost, "/api/nope", "", http.StatusNotFound},
		{"wrong method", http.MethodGet, "/api/torrent", "", http.StatusMethodNotAllowed},
		{"bad infohash", http.MethodPost, "/api/torrent", "start:zzz", http.StatusBadRequest},
		{"malformed body", http.MethodPost, "/api/torrent", "no-colon", http.StatusBadRequest},
		{"stop file unsupported", http.MethodPost, "/api/file", "stop:" + strings.Repeat("ab", 20) + ":x", http.StatusNotImplemented},
		{"bad config", http.MethodPost, "/api/configure", "{not json", http.StatusBadRequest},
		{"non-http url", http.MethodPost, "/api/url", "file:///etc/passwd", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("%s %s = %d (%q), want %d", c.method, c.path, w.Code, w.Body.String(), c.want)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain", ct)
			}
		})
	}
}

// TestSSRFGuard checks that /api/url refuses to reach into the host's own
// network. Previously it was an unauthenticated fetch of any URL with no
// timeout, no size limit, and a leaked response body.
func TestSSRFGuard(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	// Stand up a local target: a successful fetch would prove the guard failed.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("SSRF guard failed: the server reached a loopback address")
		fmt.Fprint(w, "reached")
	}))
	defer target.Close()

	for _, body := range []string{target.URL, "http://169.254.169.254/latest/meta-data/"} {
		r := httptest.NewRequest(http.MethodPost, "/api/url", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusOK {
			t.Fatalf("fetch of %s succeeded, want rejection", body)
		}
	}
}

// TestRouting pins the prefix dispatch, including the "/search" bug that used to
// swallow any path merely starting with those seven characters.
func TestRouting(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	cases := []struct{ path, wantNot string }{
		// Prefix routes must not swallow paths that merely start with the same
		// characters: "/nextdoor" is not "/next", "/eventsomething" is not
		// "/events". Both must fall through to the static handler.
		{"/nextdoor", "next"},
		{"/eventsomething", "events"},
		{"/fragmentsfoo", "fragments"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		// The static handler answers 404 for unknown paths; the scraper answers
		// differently. A 404 confirms it fell through correctly.
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 from the static handler", c.path, w.Code)
		}
	}
}

// TestSecurityHeaders pins the headers applied to every response.
func TestSecurityHeaders(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
