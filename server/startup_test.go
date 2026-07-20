package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/internal/testutil"
)

// startableOptions returns options whose config file is valid and whose ports
// are free, so New succeeds unless the test breaks something deliberately.
//
// Unlike newTestServerWith it does not call New, because these tests are about
// what New itself does with them.
func startableOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	cfg := fmt.Sprintf(`{"DownloadDirectory":%q,"IncomingPort":%d}`,
		filepath.Join(dir, "downloads"), testutil.FreePort(t))
	if err := os.WriteFile(configPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	o := DefaultOptions()
	o.ConfigPath = configPath
	o.Port = testutil.FreePort(t)
	return o
}

// TestAuthMustCarryBothParts covers an instance that reported itself protected
// and was not.
//
// auth.Wrap treats empty credentials as "authentication is off" and returns the
// handler untouched, so the guard in routes() — a bare `Auth != ""` — let
// `--auth ":"` through: the wrapper no-opped, the chain lost its auth layer,
// and the server logged "Enabled HTTP authentication" on the way past. A typo,
// or an environment variable that expanded to just the separator, silently
// published the instance.
//
// Rejecting at startup rather than defaulting to on: the operator asked for
// something the server cannot deliver, and TLS already sets the precedent that
// a half-configured security option stops the process.
func TestAuthMustCarryBothParts(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth string
	}{
		{"separator only", ":"},
		{"no password", "user:"},
		{"no user", ":secret"},
		{"no separator", "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A config that starts cleanly on its own, so --auth is the only
			// thing that can fail this New. Without it the default config binds
			// the shipped port, and a busy port would fail the startup for a
			// reason that has nothing to do with the check under test.
			o := startableOptions(t)
			o.Auth = tc.auth

			s, err := New(o, "test")
			if s != nil {
				t.Cleanup(func() { s.Close() })
			}
			if err == nil {
				t.Fatal("a half-configured --auth must fail startup; " +
					"accepting it serves the instance unauthenticated")
			}
			if !strings.Contains(err.Error(), "auth") {
				t.Errorf("error = %q, want it to name the option at fault", err)
			}
		})
	}
}

// TestAuthAcceptsAWellFormedPair is the other half: the check must not reject
// what has always worked.
func TestAuthAcceptsAWellFormedPair(t *testing.T) {
	s := newTestServerWith(t, func(o *Options) { o.Auth = "user:secret" })
	if s == nil {
		t.Fatal("a complete user:password pair must start")
	}
}

// liveSamplers counts engine sampler goroutines by stack.
//
// By name rather than by NumGoroutine because the failing New below still
// builds a UI and the runtime carries its own: only the sampler answers the
// question this test asks.
func liveSamplers() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "engine.(*Engine).sampleLoop")
}

// TestNewReleasesTheEngineOnFailure pins the constructor's cleanup.
//
// engine.New starts the sampler unconditionally and only Close releases it, so
// every failure return after it leaked a goroutine for the lifetime of the
// process. main exits on the error and never notices, which is why this stood:
// the place it is observable is the test suite, where config_test only
// registers a cleanup when New returned a server, so each run of the bad-config
// test left one behind.
//
// A malformed config file is the trigger because configfile.Load runs after the
// engine exists. Counting the sampler is the assertion because it is the thing
// that leaks — the engine has no client on this path, so no port is held and a
// rebind would prove nothing.
func TestNewReleasesTheEngineOnFailure(t *testing.T) {
	base := liveSamplers()

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{ not json"), 0600); err != nil {
		t.Fatal(err)
	}
	o := DefaultOptions()
	o.ConfigPath = configPath

	s, err := New(o, "test")
	if err == nil {
		s.Close()
		t.Fatal("a malformed config file must fail startup")
	}
	if s != nil {
		t.Fatal("a failed New must return a nil server")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got := liveSamplers() - base
		if got == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine samplers still running after a failed New: %d; "+
				"the constructor kept the engine it built", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestOversizeAPIBodyIsRefusedNotTruncated covers a misleading error.
//
// readBody used io.LimitReader, which reports the cap as a clean io.EOF, so a
// .torrent over maxAPIBody arrived truncated and was reported as *malformed* —
// sending the user to look for corruption in a file that has none. The
// multipart path always got this right; this is the raw-bytes path catching up.
func TestOversizeAPIBodyIsRefusedNotTruncated(t *testing.T) {
	s := newTestServer(t)

	body := bytes.Repeat([]byte("x"), maxAPIBody+(1<<10))
	req := httptest.NewRequest(http.MethodPost, "/api/torrentfile", bytes.NewReader(body))
	req.Header.Set("Origin", "http://example.com")
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	// The reason, not the status: a truncated body also answers 400, by failing
	// to parse. Only the MaxBytesReader message separates "we refused to read
	// it" from "we read 4 MiB of it and disliked what we got".
	if msg := rec.Body.String(); !strings.Contains(msg, "too large") {
		t.Fatalf("body = %q, want it to report the body was too large; "+
			"a parse failure here means the body was silently truncated", msg)
	}
}

// TestInstanceMetaReachesBothConsumers ties the two places this server
// describes itself: the rendered page and the /api/state document.
//
// They were built from separate copies of the same four values, with nothing
// asserting they agreed. Collapsing them into one instanceMeta makes a
// disagreement mostly unrepresentable; this covers the part that remains — that
// both consumers are actually wired to it, so reintroducing a second source
// fails here rather than shipping a page and an API that name different builds.
//
// Only Title is checkable on the page: the stats region ships as a placeholder
// and is filled over SSE, so Version and Runtime never appear in the initial
// HTML. Asserting them there would be asserting a thing that is not true.
func TestInstanceMetaReachesBothConsumers(t *testing.T) {
	const version = "test"
	s := newTestServer(t)

	page := httptest.NewRecorder()
	s.handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", page.Code)
	}
	if !strings.Contains(page.Body.String(), DefaultOptions().Title) {
		t.Fatalf("the page does not carry the title from instanceMeta")
	}

	// The API document.
	api := httptest.NewRecorder()
	s.handler().ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if api.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d, want 200", api.Code)
	}
	var doc struct {
		Stats struct {
			Title   string
			Version string
			Runtime string
		}
	}
	if err := json.Unmarshal(api.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode /api/state: %v", err)
	}
	if doc.Stats.Version != version {
		t.Errorf("/api/state version = %q, want %q", doc.Stats.Version, version)
	}
	if doc.Stats.Title != DefaultOptions().Title {
		t.Errorf("/api/state title = %q, want %q", doc.Stats.Title, DefaultOptions().Title)
	}
	if doc.Stats.Runtime == "" {
		t.Error("/api/state reports no Go runtime; instanceMeta is not wired to it")
	}
}
