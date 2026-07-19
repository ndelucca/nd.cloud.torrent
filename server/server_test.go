package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/sysstat"
	"github.com/ndelucca/nd.cloud.torrent/web"
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

// waitForListener blocks until addr accepts a connection. A dial loop rather
// than a sleep: the point is to start the test as soon as Run is serving, not
// to guess how long that takes.
func waitForListener(t *testing.T, addr string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s after %s", addr, limit)
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}

// TestRunShutsDownPromptlyWithSSEClients is the regression test for a shutdown
// that could not finish.
//
// http.Server.Shutdown waits for connections to become idle and does not cancel
// request contexts. An /events handler parked in its select is therefore never
// released by it, so with a single browser connected Shutdown burned its entire
// 10s budget, returned context.DeadlineExceeded, and main turned that into
// log.Fatal — a clean Ctrl-C exiting 1 after a ten second hang.
func TestRunShutsDownPromptlyWithSSEClients(t *testing.T) {
	if testing.Short() {
		t.Skip("binds a port and waits on a real shutdown")
	}
	s := newTestServer(t)
	// Populate a region so a connecting client gets a snapshot frame.
	s.renderStats()

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(runCtx) }()

	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(s.opts.Port))
	waitForListener(t, addr, 5*time.Second)

	// A transport of our own, and deliberately no context tied to runCtx.
	// Sharing it would cancel these requests at the same instant we ask the
	// server to stop; the handlers would exit through r.Context().Done() and the
	// test would pass against the very bug it exists to catch.
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}

	const clients = 2
	bodies := make([]io.ReadCloser, 0, clients)
	for i := 0; i < clients; i++ {
		resp, err := client.Get("http://" + addr + "/events")
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		defer resp.Body.Close()
		// Reading a line proves the handler is past its headers and parked in
		// the select, which is where the bug lives.
		if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
			t.Fatalf("client %d first line: %v", i, err)
		}
		bodies = append(bodies, resp.Body)
	}
	// Otherwise the test could pass trivially with nobody subscribed.
	waitFor(t, 2*time.Second, "both clients to subscribe", func() bool {
		return s.watchers() == clients
	})

	start := time.Now()
	stop()

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run = %v, want nil: a requested shutdown is not a failed run, "+
				"and main log.Fatals on a non-nil error (exit 1)", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("shutdown took %s: Shutdown is waiting out its full budget on the "+
				"SSE streams — the hub must be released before srv.Shutdown", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation")
	}

	// The client-facing half: a released stream must actually end. Draining one
	// that is still held open would block until the test binary times out.
	for i, body := range bodies {
		drained := make(chan struct{})
		go func() { io.Copy(io.Discard, body); close(drained) }()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
			t.Errorf("client %d: stream did not end after shutdown", i)
		}
	}
}

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

// TestConcurrentEventStreams guards the remote-DoS fix that predates the
// migration: the old code wrote and deleted a per-connection map entry from
// each HTTP goroutine without holding the mutex, and two simultaneous clients
// produced "fatal error: concurrent map writes", which Go cannot recover from.
//
// The map is gone — connections are counted, not rostered — but the shape of
// the risk moved to the SSE hub. web.TestHubConcurrentSubscribers hammers the
// hub directly, which is where the data race would now live; this one keeps the
// original shape of the bug, real simultaneous /events connections against a
// real handler chain, because that is what reproduced it. Run under -race.
func TestConcurrentEventStreams(t *testing.T) {
	s := newTestServer(t)

	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(2)
		// A client that connects, reads a little and leaves.
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
			if err != nil {
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			buf := make([]byte, 256)
			_, _ = resp.Body.Read(buf)
		}()
		// And the render loop's side of it, running concurrently.
		go func() {
			defer wg.Done()
			s.stats.set(sysstat.Stats{Set: true, GoRoutines: s.watchers()})
			s.renderStats()
		}()
	}
	wg.Wait()
}

// TestAPIRejectsCrossOrigin covers the CSRF fix. /api/* takes text/plain and
// form-encoded bodies, both of which browsers send cross-origin with no
// preflight, so any page could have POSTed /api/configure to repoint the
// download root at "/".
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
			path := "/api/torrents/" + strings.Repeat("ab", 20) + "/start"
			r := httptest.NewRequest(http.MethodPost, path, nil)
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
		form                     bool
		want                     int
	}{
		{"missing torrent", http.MethodPost, "/api/torrents/" + strings.Repeat("ab", 20) + "/start", "", false, http.StatusNotFound},
		{"missing torrent on delete", http.MethodDelete, "/api/torrents/" + strings.Repeat("ab", 20), "", false, http.StatusNotFound},
		{"unknown action", http.MethodPost, "/api/nope", "", false, http.StatusNotFound},
		{"wrong method", http.MethodGet, "/api/torrents/x/start", "", false, http.StatusMethodNotAllowed},
		{"bad infohash", http.MethodPost, "/api/torrents/zzz/start", "", false, http.StatusBadRequest},
		// The "malformed body" and "non-form body" cases that used to live here
		// are gone with the form: the hash is a path segment now, so a torrent
		// verb has no body to get wrong.
		{"non-form config body", http.MethodPost, "/api/configure", "{not json", false, http.StatusBadRequest},
		{"non-http url", http.MethodPost, "/api/add", "file:///etc/passwd", false, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			if c.form {
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
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

// TestSSRFGuard checks that the remote-torrent fetch refuses to reach into the
// host's own network. Previously it was an unauthenticated fetch of any URL
// with no timeout, no size limit, and a leaked response body.
//
// It goes through /api/add, which dispatches an http(s) URL to the same fetch
// the deleted /api/url endpoint used.
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
		r := httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusOK {
			t.Fatalf("fetch of %s succeeded, want rejection", body)
		}
	}
}

// TestRouting asserts that every declared route reaches the handler that owns
// it.
//
// It used to assert only the opposite — that "/nextdoor" does not match "/next"
// — because dispatch was a hand-rolled, order-sensitive prefix switch and
// swallowing a neighbouring path was the live hazard. ServeMux patterns make
// that impossible by construction, so the useful assertions are the positive
// ones, which nothing covered before.
func TestRouting(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	cases := []struct {
		method, path string
		wantNot      int // a status that would mean the route did not resolve
		want         int // 0 means "anything but wantNot"
	}{
		{method: http.MethodGet, path: "/", want: http.StatusOK},
		{method: http.MethodGet, path: "/api/state", want: http.StatusOK},
		{method: http.MethodGet, path: "/fragments/downloads", want: http.StatusOK},
		{method: http.MethodGet, path: "/js/ct.js", want: http.StatusOK},
		{method: http.MethodGet, path: "/css/ct.css", want: http.StatusOK},
		{method: http.MethodGet, path: "/cloud-favicon.png", want: http.StatusOK},
		// Resolves to the fragment handler, which reports the torrent is gone —
		// not to the mux's 404, which is what the old string surgery produced
		// for a hash containing a slash.
		{method: http.MethodGet, path: "/fragments/torrent/deadbeef/files", want: http.StatusNotFound},
		// Neighbouring paths still must not be swallowed.
		{method: http.MethodGet, path: "/nextdoor", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/eventsomething", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/fragmentsfoo", want: http.StatusNotFound},
		// A wrong method on a declared path is 405, not 404. ServeMux only
		// manages that because the static assets are mounted at their real
		// prefixes rather than behind a catch-all.
		{method: http.MethodGet, path: "/api/add", want: http.StatusMethodNotAllowed},
		{method: http.MethodPost, path: "/", want: http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			r := httptest.NewRequest(c.method, c.path, nil)
			r.Header.Set("Origin", "http://"+r.Host)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("%s %s = %d, want %d (body %q)", c.method, c.path, w.Code, c.want,
					strings.TrimSpace(w.Body.String()))
			}
		})
	}
}

// TestSameOriginIsEnforcedByMethod pins that the cross-origin gate is a
// property of the method, not of a route someone remembered to wrap.
//
// checkSameOrigin used to be called from two places — inside api() and again in
// serveDownload's DELETE branch — so the invariant held by convention, and the
// package doc had to warn that adding a mutating route which bypassed it was
// how that became a bug. As middleware it covers a route before that route is
// written.
func TestSameOriginIsEnforcedByMethod(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/api/add", http.StatusForbidden},
		{http.MethodPost, "/api/configure", http.StatusForbidden},
		{http.MethodDelete, "/download/anything", http.StatusForbidden},
		// Reads are exempt: they are not writes, and /download/ links have to
		// work from anywhere.
		{http.MethodGet, "/api/state", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			r := httptest.NewRequest(c.method, c.path, strings.NewReader(""))
			r.Header.Set("Origin", "http://evil.example")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("cross-origin %s %s = %d, want %d", c.method, c.path, w.Code, c.want)
			}
		})
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

// TestKnownRoutesResolve is the server half of the htmx contract check.
//
// web.KnownRoutes lists the paths the templates ask for; web's own test asserts
// every hx-get and sse-connect attribute is on that list, and this one asserts
// every entry on the list reaches a handler here. Together they are the missing
// assertion: a URL in a template is a URL the server answers. Nothing tied the
// two together before — region names, fragment URLs and swap targets were
// string-matched across Go consts, {{define}} names and HTML attributes with no
// test in between.
func TestKnownRoutesResolve(t *testing.T) {
	s := newTestServer(t)
	mux, ok := s.routes().(*http.ServeMux)
	if !ok {
		t.Fatal("routes() no longer returns a *http.ServeMux; this test needs Handler()")
	}

	for _, pattern := range web.KnownRoutes {
		t.Run(pattern, func(t *testing.T) {
			// Substitute a concrete value for the wildcard so the pattern
			// becomes a requestable path.
			path := strings.ReplaceAll(pattern, "{hash}", strings.Repeat("ab", 20))
			r := httptest.NewRequest(http.MethodGet, path, nil)
			h, matched := mux.Handler(r)
			if matched == "" || h == nil {
				t.Fatalf("%s resolves to no route", path)
			}
			// A matched-but-empty pattern is the mux's own 404 handler.
			if matched == "" {
				t.Fatalf("%s falls through to the default handler", path)
			}
		})
	}
}
