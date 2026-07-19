package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// newConfigTestServer builds a server against a config path that may or may not
// exist, so the persistence tests can observe what startup does to the file.
func newConfigTestServer(t *testing.T, configPath string) (*Server, error) {
	t.Helper()
	o := DefaultOptions()
	o.Port = freePort(t)
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
		`","IncomingPort":` + itoa(freePort(t)) + extra + `}`
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

// TestSaveConfigIsAtomic pins that a save leaves either the old file or the new
// one, never a fragment, and cleans up after itself.
func TestSaveConfigIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, "")
	s, err := newConfigTestServer(t, path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c := s.engine.Config()
	c.EnableSeeding = true
	if err := s.saveConfig(c); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got engine.Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("saved config does not parse: %v\n%s", err, b)
	}
	if !got.EnableSeeding {
		t.Fatal("saved config lost the change")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cloud-torrent-") {
			t.Fatalf("temp file %s was left behind", e.Name())
		}
	}
}

// TestSaveConfigCreatesParentDirectory covers --config-path pointing somewhere
// that does not exist yet, which used to fail the save outright.
func TestSaveConfigCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "config.json")
	s, err := newConfigTestServer(t, path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.saveConfig(s.engine.Config()); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config was not written: %v", err)
	}
}

// TestBadPortInConfigIsReported covers two policies for one rule.
//
// loadConfig silently rewrote an out-of-range port to the default while
// engine.Config.Validate rejected the identical value. The clamp could only
// ever fire on a port someone explicitly wrote, so silently ignoring it was the
// worst of the options — and having both meant they could disagree.
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
	s.serveAPI(rec, req)

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
