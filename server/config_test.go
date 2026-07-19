package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

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
func TestNewWithNoConfigCreatesNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := newConfigTestServer(t, path); err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("startup created %s; want no file until a save", path)
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
// /api/configure is read-merge-apply: it reads the engine's current config,
// overlays the submitted fields and applies the result. The engine's own lock
// serializes the apply but not the read the merge starts from, so two saves in
// flight together could each begin from the same config and the second would
// silently undo the first. configMu covers the whole sequence.
//
// Probabilistic, so it loops.
func TestConcurrentConfigureKeepsBothFields(t *testing.T) {
	// One server, reset between rounds: standing up twenty engines races the
	// kernel for UDP ports and would fail on that instead of on the bug.
	s := newTestServer(t)
	h := s.handler()
	// The response is checked rather than discarded. Every one of these is a real
	// same-port engine rebind, so a round that exhausts rebindTimeout answers 500
	// — and discarding that surfaced it as "setup: reset did not take" on the
	// *next* round, which points at the wrong thing entirely.
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

	// Ten rounds, not twenty. Each round is three same-port rebinds, and a
	// rebind waits on the kernel releasing the listening socket — this test was
	// most of the package's wall time. Ten rounds still fails reliably with
	// configMu removed, which is the only thing the count has to buy.
	for i := 0; i < 10; i++ {
		post(t, "EnableSeeding=false&DisableEncryption=false")
		if c := s.engine.Config(); c.EnableSeeding || c.DisableEncryption {
			t.Fatalf("setup: reset did not take, got %+v", c)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); post(t, "EnableSeeding=false&EnableSeeding=true") }()
		go func() { defer wg.Done(); post(t, "DisableEncryption=false&DisableEncryption=true") }()
		wg.Wait()

		got := s.engine.Config()
		if !got.EnableSeeding || !got.DisableEncryption {
			t.Fatalf("a concurrent save was lost on round %d: EnableSeeding=%v DisableEncryption=%v",
				i, got.EnableSeeding, got.DisableEncryption)
		}
	}
}
