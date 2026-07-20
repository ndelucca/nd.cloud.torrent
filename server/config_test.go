package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ndelucca/nd.cloud.torrent/configfile"
	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/internal/testutil"
)

// newConfigTestServer builds a server against a config path that may or may not
// exist, so the persistence tests can observe what startup does to the file.
func newConfigTestServer(t *testing.T, configPath string) (*Server, error) {
	t.Helper()
	o := DefaultOptions()
	o.Port = testutil.FreePort(t)
	o.ConfigPath = configPath
	s, err := New(o, "test")
	if s != nil {
		t.Cleanup(func() { s.Close() })
	}
	return s, err
}

// writeConfig seeds a config file with a free incoming port.
func writeConfig(t *testing.T, path string, extra string) {
	t.Helper()
	dir := filepath.Dir(path)
	body := `{"DownloadDirectory":"` + filepath.ToSlash(filepath.Join(dir, "downloads")) +
		`","IncomingPort":` + itoa(testutil.FreePort(t)) + extra + `}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestNewDoesNotWriteConfig covers startup rewriting a file it had no reason to
// touch. New called reconfigure, which persisted unconditionally, so every boot
// re-serialized the config — a chance to corrupt it that bought nothing, and it
// made New's own "performs no I/O beyond reading the config file" false.
func TestNewDoesNotWriteConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, "")

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newConfigTestServer(t, path); err != nil {
		t.Fatalf("New: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("startup rewrote the config file\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestNewWithNoConfigCreatesNone is the same rule for a first run: defaults are
// applied in memory, and nothing is written until the user saves something.
//
// The file has to be genuinely absent, which is the one case that cannot seed a
// free port: the engine gets configfile.Defaults() and binds its fixed
// IncomingPort for real. Whether that bind wins is not what this test is about —
// another instance on the machine may hold the port — so the assertion is made
// independent of it. Either outcome, no file may appear.
func TestNewWithNoConfigCreatesNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := newConfigTestServer(t, path)
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("startup created %s; want no file until a save", path)
	}
	if err != nil && s != nil {
		t.Fatalf("New returned both a server and an error: %v", err)
	}
}

// TestBadPortInConfigIsReported covers two policies for one rule.
//
// configfile.Load preserves an out-of-range port and engine.Config.Validate
// rejects it, so the value the user wrote is reported rather than silently
// replaced. A clamp in Load could only ever fire on a port someone explicitly
// chose, and having both would let the two policies disagree.
func TestBadPortInConfigIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"IncomingPort":99999}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := newConfigTestServer(t, path)
	if err == nil {
		t.Fatal("an out-of-range port must be reported, not silently replaced")
	}
	if !strings.Contains(err.Error(), "99999") {
		t.Fatalf("error must name the rejected port, got: %v", err)
	}
}

// TestConcurrentConfigureKeepsBothFields covers a lost update.
//
// /api/configure is read-merge-apply-persist: it reads the desired config,
// overlays the submitted fields, hands the result to the engine and writes it.
// Without one lock over the whole sequence, two saves in flight together each
// begin from the same config and the second silently undoes the first.
//
// It asserts on the *persisted* config rather than on the engine's. Both fields
// here need a restart, so the engine deliberately does not apply them — what has
// to survive the race is what was saved, which is also what the settings form
// will render back.
//
// Probabilistic, so it loops.
func TestConcurrentConfigureKeepsBothFields(t *testing.T) {
	// One server, reset between rounds: standing up twenty engines races the
	// kernel for UDP ports and would fail on that instead of on the bug.
	s := newTestServer(t)
	h := s.handler()
	// The response is checked rather than discarded, so a round that fails for
	// an unrelated reason says so here instead of surfacing as "setup: reset did
	// not take" on the *next* round, which points at the wrong thing entirely.
	post := func(t *testing.T, body string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/configure", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://"+r.Host)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		// Errorf, not Fatalf: post runs on the concurrent goroutines below, and
		// Fatalf calls runtime.Goexit, which is only defined behaviour on the
		// test's own goroutine.
		if rec.Code != http.StatusOK {
			t.Errorf("configure %q: status %d, body %q", body, rec.Code, rec.Body.String())
		}
	}

	// saved reads back what is on disk, which is the contract the form and the
	// next boot both depend on.
	saved := func(t *testing.T) engine.Config {
		t.Helper()
		c, err := configfile.Load(s.opts.ConfigPath)
		if err != nil {
			t.Fatalf("reading back the saved config: %v", err)
		}
		return c
	}

	for i := 0; i < 10; i++ {
		post(t, "EnableSeeding=false&DisableEncryption=false")
		if c := saved(t); c.EnableSeeding || c.DisableEncryption {
			t.Fatalf("setup: reset did not take, got %+v", c)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); post(t, "EnableSeeding=false&EnableSeeding=true") }()
		go func() { defer wg.Done(); post(t, "DisableEncryption=false&DisableEncryption=true") }()
		wg.Wait()

		got := saved(t)
		if !got.EnableSeeding || !got.DisableEncryption {
			t.Fatalf("a concurrent save was lost on round %d: EnableSeeding=%v DisableEncryption=%v",
				i, got.EnableSeeding, got.DisableEncryption)
		}
		// The form renders desired, so it must agree with the file.
		if d := s.desiredConfig(); d.EnableSeeding != got.EnableSeeding ||
			d.DisableEncryption != got.DisableEncryption {
			t.Fatalf("round %d: desired and the file disagree; the settings form "+
				"would render something that was never saved: desired=%+v file=%+v", i, d, got)
		}
	}
}

// TestConfigureNeedingRestartStillPersists is the whole reason
// engine.ErrRestartRequired is not treated as a failure here.
//
// Most settings are fixed for the lifetime of a torrent client, so the engine
// refuses to apply them. If the server also refused to *save* them, there would
// be no way to change the listen port at all short of editing cloud-torrent.json
// by hand — that removes the feature rather than deferring it.
//
// So: 200 with a message saying to restart, the value on disk, and the settings
// form rendering what was saved rather than what is running.
func TestConfigureNeedingRestartStillPersists(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	livePort := s.engine.Config().IncomingPort
	newPort := testutil.FreePort(t)

	r := httptest.NewRequest(http.MethodPost, "/api/configure",
		strings.NewReader("IncomingPort="+itoa(newPort)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+r.Host)
	r.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: saving a setting that needs a restart "+
			"is not a failed request", rec.Code)
	}
	// The ok fragment, not the error one — the save worked.
	if body := rec.Body.String(); !strings.Contains(body, "ok-msg") ||
		!strings.Contains(strings.ToLower(body), "restart") {
		t.Errorf("body = %q, want an api-ok fragment mentioning a restart", body)
	}

	if c, err := configfile.Load(s.opts.ConfigPath); err != nil {
		t.Fatal(err)
	} else if c.IncomingPort != newPort {
		t.Errorf("saved IncomingPort = %d, want %d: the change was reported as "+
			"accepted and never written", c.IncomingPort, newPort)
	}
	if got := s.desiredConfig().IncomingPort; got != newPort {
		t.Errorf("the settings form would render port %d, but %d was saved",
			got, newPort)
	}
	// And the running client is untouched, which is the honest half.
	if got := s.engine.Config().IncomingPort; got != livePort {
		t.Errorf("live port changed to %d; the client cannot rebind in place", got)
	}
}
