package server

import (
	"bufio"
	"context"
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

	"github.com/ndelucca/nd.cloud.torrent/internal/testutil"
	"github.com/ndelucca/nd.cloud.torrent/sysstat"
	"github.com/ndelucca/nd.cloud.torrent/web"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWith(t, nil)
}

// newTestServerWith is newTestServer with a hook to adjust the options before
// New sees them, for the tests that need a non-default chain (--auth, TLS).
func newTestServerWith(t *testing.T, tweak func(*Options)) *Server {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	// Retried, because testutil.FreePort is a TOCTOU by construction: it closes
	// its probe listeners before returning, so under load another process can
	// take the port before the engine binds it. engine.mustConfigure retries for
	// the same reason. Without this the failure lands on whichever test drew the
	// port, blaming the code under test for a collision in the fixture.
	var s *Server
	for attempt := 0; ; attempt++ {
		// Seeded so the engine binds a free port and downloads into a scratch
		// directory rather than the shipped defaults. Rewritten on each attempt
		// so a retry draws a new port instead of losing the same race again.
		cfg := fmt.Sprintf(`{"DownloadDirectory":%q,"IncomingPort":%d,"EnableUpload":true,"AutoStart":true}`,
			filepath.Join(dir, "downloads"), testutil.FreePort(t))
		if err := os.WriteFile(configPath, []byte(cfg), 0600); err != nil {
			t.Fatal(err)
		}

		// Rebuilt from the defaults each attempt: tweak takes a pointer and is
		// free to mutate, so reusing one Options across retries would compound
		// its edits.
		o := DefaultOptions()
		o.ConfigPath = configPath
		o.Port = testutil.FreePort(t)
		if tweak != nil {
			tweak(&o)
		}

		var err error
		s, err = New(o, "test")
		if err == nil {
			break
		}
		if attempt == 20 || !strings.Contains(err.Error(), "address already in use") {
			t.Fatalf("New: %v", err)
		}
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
	s.render.renderStats()

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
			s.render.renderStats()
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

	// The allowed cases expect 404, not 200: the request gets past the origin
	// check and then fails on the torrent being absent. Asserting the exact
	// status matters — "not 403" is satisfied by a 500, so a request that blew
	// up somewhere else entirely would read as "allowed through".
	cases := []struct {
		name       string
		headers    map[string]string
		wantStatus int
	}{
		{"no origin (curl)", nil, http.StatusNotFound},
		{"same-origin fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusNotFound},
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

			if w.Code != c.wantStatus {
				t.Fatalf("status = %d (%q), want %d", w.Code, w.Body.String(), c.wantStatus)
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

// TestDeleteDownloadReportsOutcomeAsFragment covers a delete that reported
// nothing.
//
// It was answered by files.Handler, which returns 200 with an EMPTY body on
// success and 500 plain text on failure, while the button swapped the reply into
// #downloads. So a success blanked the panel and a failure was silent, because
// htmx does not swap a non-2xx response. Routing it through apiRoute gives it
// the same api-ok/api-error fragment every other verb gets.
func TestDeleteDownloadReportsOutcomeAsFragment(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	root := s.downloadDir()
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	// Written outside the download root: nothing the server does may reach it.
	outside := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(outside, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	del := func(t *testing.T, path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodDelete, path, nil)
		r.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	t.Run("success reports ok and removes the file", func(t *testing.T) {
		target := filepath.Join(root, "gone.txt")
		if err := os.WriteFile(target, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		w := del(t, "/download/gone.txt")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — htmx will not swap a non-2xx", w.Code)
		}
		if !strings.Contains(w.Body.String(), "ok-msg") {
			t.Errorf("body = %q, want an api-ok fragment", w.Body.String())
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Errorf("file survived the delete: %v", err)
		}
	})

	t.Run("missing file reports an error rather than nothing", func(t *testing.T) {
		w := del(t, "/download/never-existed.txt")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 so htmx swaps the message", w.Code)
		}
		if !strings.Contains(w.Body.String(), "err-msg") {
			t.Errorf("body = %q, want an api-error fragment", w.Body.String())
		}
	})

	// The two spellings are stopped by different things, and the distinction is
	// the point: a literal ".." never reaches this package, so only the encoded
	// form actually exercises ResolveWithin.
	t.Run("a literal .. is cleaned away by the mux", func(t *testing.T) {
		w := del(t, "/download/../secret.txt")
		if w.Code != http.StatusTemporaryRedirect {
			t.Errorf("status = %d, want 307 — ServeMux cleans the path and "+
				"redirects, so the handler never sees this form", w.Code)
		}
		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("the file outside the download directory was deleted")
		}
	})

	t.Run("an encoded .. reaches the handler and is refused", func(t *testing.T) {
		// %2e%2e survives the mux and arrives at r.PathValue already decoded, so
		// this is the form a check written against the raw path would miss.
		// files.Remove is what stops it.
		w := del(t, "/download/%2e%2e/secret.txt")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "err-msg") {
			t.Errorf("status = %d body = %q, want a 200 api-error fragment",
				w.Code, w.Body.String())
		}
		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("an encoded traversal deleted a file outside the download directory")
		}
	})

	t.Run("the message never names the resolved path", func(t *testing.T) {
		w := del(t, "/download/never-existed.txt")
		if strings.Contains(w.Body.String(), root) {
			t.Errorf("body leaked the download root, which is a layout oracle: %q",
				w.Body.String())
		}
	})
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
// property of the method, not of a route someone remembered to wrap. As
// middleware over the whole mux it covers a mutating route before that route is
// written — including DELETE /download/, which is its own route now.
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

	// All three that securityHeaders sets. Referrer-Policy was previously
	// unasserted, so removing it would have failed nothing.
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// TestTLSRequiresBothPaths pins a startup rule that had no test at all: TLS is
// configured by two options and one of them alone is a misconfiguration, not a
// half-enabled server. Failing at New is what stops it silently serving
// plaintext on the port an operator believed was HTTPS.
func TestTLSRequiresBothPaths(t *testing.T) {
	for name, tweak := range map[string]func(*Options){
		"cert without key": func(o *Options) { o.CertPath = "/tmp/cert.pem" },
		"key without cert": func(o *Options) { o.KeyPath = "/tmp/key.pem" },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			o := DefaultOptions()
			o.Port = testutil.FreePort(t)
			o.ConfigPath = filepath.Join(dir, "config.json")
			tweak(&o)

			s, err := New(o, "test")
			if err == nil {
				s.Close()
				t.Fatal("New succeeded with only half of the TLS pair")
			}
			if !strings.Contains(err.Error(), "key and cert") {
				t.Errorf("error = %v, want it to name the missing half", err)
			}
		})
	}
}

// TestTLSWithBothPathsIsAccepted is the other half: with both set, New gets past
// the check and the server knows it is serving TLS. That flag decides whether
// the session cookie is marked Secure, so it is not cosmetic.
func TestTLSWithBothPathsIsAccepted(t *testing.T) {
	dir := t.TempDir()
	o := DefaultOptions()
	o.Port = testutil.FreePort(t)
	o.ConfigPath = filepath.Join(dir, "config.json")
	o.CertPath = filepath.Join(dir, "cert.pem")
	o.KeyPath = filepath.Join(dir, "key.pem")
	// Seeded, because this test needs New to get all the way past the engine.
	// Without a file the engine gets configfile.Defaults() and binds its fixed
	// IncomingPort, which collides with any other instance on the machine.
	writeConfig(t, o.ConfigPath, "")

	s, err := New(o, "test")
	if err != nil {
		t.Fatalf("New with both TLS paths: %v", err)
	}
	defer s.Close()
	if !s.isTLS {
		t.Error("isTLS is false with both paths set; the session cookie would " +
			"not be marked Secure")
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
			// An empty pattern means the mux's own 404 handler, i.e. the path
			// falls through to no route of ours.
			h, matched := mux.Handler(r)
			if matched == "" || h == nil {
				t.Fatalf("%s resolves to no route", path)
			}
		})
	}
}

// TestStaticAssetsAreServed is the other half of TestKnownRoutesResolve, for
// the paths where resolution is not evidence.
//
// The assets are mounted at prefixes, so mux.Handler matches /css/anything and
// would report a renamed stylesheet as fine. Only a real request proves the
// file is in the binary, which is what page.html depends on.
func TestStaticAssetsAreServed(t *testing.T) {
	h := newTestServer(t).routes()
	for _, p := range web.StaticAssets {
		t.Run(p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: %d, want 200 — page.html loads this and the embedded "+
					"FS does not have it", p, rec.Code)
			}
			if rec.Body.Len() == 0 {
				t.Fatalf("GET %s: 200 with an empty body", p)
			}
		})
	}
}

// TestContentSecurityPolicy pins the app's own policy and, just as importantly,
// that it does not displace the stricter one on downloaded content.
//
// The directive that matters is script-src without 'unsafe-inline'. Every script
// this app loads is a same-origin file, so the policy is not about them — it is
// about a script an attacker gets into the page, and this app renders
// torrent-supplied file names into markup that includes an Alpine x-data sink
// the html/template escaper cannot see.
func TestContentSecurityPolicy(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	get := func(t *testing.T, path string) string {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Header().Get("Content-Security-Policy")
	}

	t.Run("app pages", func(t *testing.T) {
		csp := get(t, "/")
		if csp == "" {
			t.Fatal("no CSP on the app's own pages")
		}
		// Both escapes asserted absent explicitly rather than trusting the
		// directive string to stay right. unsafe-inline is what stops an
		// injected script running at all; unsafe-eval is gone because Alpine
		// ships as its CSP build, so reintroducing it would mean someone swapped
		// the bundle back.
		for _, escape := range []string{"unsafe-inline", "unsafe-eval"} {
			if strings.Contains(csp, escape) {
				t.Errorf("script-src allows %s: %q", escape, csp)
			}
		}
		for _, want := range []string{
			"default-src 'self'",
			"script-src 'self'",
			"style-src 'self'",
			"frame-ancestors 'none'",
			"object-src 'none'",
			"base-uri 'none'",
			"form-action 'self'",
		} {
			if !strings.Contains(csp, want) {
				t.Errorf("CSP is missing %q: %q", want, csp)
			}
		}
	})

	// Static assets travel through the same middleware, so they must carry it
	// too — a policy that only covers the document is not a policy.
	t.Run("static assets", func(t *testing.T) {
		if csp := get(t, "/js/ct.js"); csp == "" {
			t.Error("no CSP on a static asset")
		}
	})

	// files.sandbox puts downloaded content in an opaque origin. That is
	// stricter and must win: a torrent containing an index.html is served as
	// text/html from this origin, and 'self' would let it run.
	t.Run("downloaded files keep the sandbox policy", func(t *testing.T) {
		root := s.downloadDir()
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "evil.html"),
			[]byte(`<script>fetch("/api/torrents/x", {method:"DELETE"})</script>`), 0600); err != nil {
			t.Fatal(err)
		}
		csp := get(t, "/download/evil.html")
		if !strings.Contains(csp, "sandbox") {
			t.Errorf("downloaded content lost its sandbox policy: %q", csp)
		}
		if strings.Contains(csp, "script-src 'self'") {
			t.Errorf("the app policy displaced the sandbox on downloaded "+
				"content, which would let it run same-origin: %q", csp)
		}
	})
}
