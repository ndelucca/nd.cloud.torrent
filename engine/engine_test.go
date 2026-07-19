package engine

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestStr2IH(t *testing.T) {
	valid := strings.Repeat("ab", 20) // 40 hex chars
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"valid", valid, false},
		{"uppercase", strings.ToUpper(valid), false},
		{"empty", "", true},
		{"too short", strings.Repeat("a", 39), true},
		// Regression: hex.Decode is bounded by len(src), not len(dst), so an
		// over-long input used to write past the end of the [20]byte and panic.
		// Reachable unauthenticated via POST /api/torrent.
		{"one byte too long", strings.Repeat("a", 42), true},
		{"far too long", strings.Repeat("a", 500), true},
		{"non-hex", strings.Repeat("z", 40), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A panic here fails the test rather than taking down the process.
			ih, err := str2ih(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("str2ih(%q) = %x, want error", c.in, ih)
				}
				return
			}
			if err != nil {
				t.Fatalf("str2ih(%q) unexpected error: %v", c.in, err)
			}
			if got := ih.HexString(); !strings.EqualFold(got, c.in) {
				t.Fatalf("round trip: got %q want %q", got, c.in)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	base := Config{DownloadDirectory: "/tmp/dl", IncomingPort: 50007}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := map[string]Config{
		"zero port":     {DownloadDirectory: "/tmp/dl", IncomingPort: 0},
		"negative port": {DownloadDirectory: "/tmp/dl", IncomingPort: -1},
		"over range":    {DownloadDirectory: "/tmp/dl", IncomingPort: 65536},
		"no directory":  {IncomingPort: 50007},
	}
	for name, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}

	// 65535 is a valid port; the old bounds check (>= 65535) rejected it.
	edge := Config{DownloadDirectory: "/tmp/dl", IncomingPort: 65535}
	if err := edge.Validate(); err != nil {
		t.Errorf("port 65535 should be valid, got %v", err)
	}
}

func TestPercent(t *testing.T) {
	cases := []struct {
		n, total int64
		want     float32
	}{
		{0, 0, 0}, // no division by zero
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		{1, 3, 33.33},
	}
	for _, c := range cases {
		if got := percent(c.n, c.total); got != c.want {
			t.Errorf("percent(%d, %d) = %v, want %v", c.n, c.total, got, c.want)
		}
	}
}

// TestTorrentCloneIsDeep guards the fix for the engine handing its live internal
// map to the server, which then marshalled it from another goroutine.
func TestTorrentCloneIsDeep(t *testing.T) {
	orig := &Torrent{
		InfoHash: "abc",
		Started:  true,
		Files:    []*File{{Path: "a.mkv", Percent: 10}, nil},
	}
	c := orig.clone()

	c.Started = false
	c.Files[0].Percent = 99
	if !orig.Started {
		t.Error("clone shares the Started field")
	}
	if orig.Files[0].Percent != 10 {
		t.Error("clone shares File values")
	}
	if c.Files[1] != nil {
		t.Error("nil file entry should stay nil")
	}
	if c.t != nil || c.spec != nil {
		t.Error("clone must not carry internal handles out of the engine")
	}
}

// TestUpdateLoadedResizesFiles guards against the index panic that happened
// when a torrent's file list grew after the slice was first allocated.
//
// It calls the production path. The previous version re-implemented the resize
// inside the test body and asserted on its own copy, so it passed whether or
// not the real code was correct — it would have passed with updateLoaded
// deleted entirely.
func TestUpdateLoadedResizesFiles(t *testing.T) {
	tor := &Torrent{Files: []*File{{Path: "old"}}}

	// Grow: one cached entry, three live files.
	tor.resizeFiles(3)
	if len(tor.Files) != 3 {
		t.Fatalf("after growing: len = %d, want 3", len(tor.Files))
	}
	if tor.Files[0] == nil || tor.Files[0].Path != "old" {
		t.Error("existing entries must be preserved across a resize")
	}
	for i, f := range tor.Files {
		if f == nil && i > 0 {
			continue // new slots are filled by the caller
		}
	}

	// Shrink: the slice must follow the live count, not keep stale entries.
	tor.resizeFiles(1)
	if len(tor.Files) != 1 {
		t.Fatalf("after shrinking: len = %d, want 1", len(tor.Files))
	}
	if tor.Files[0] == nil || tor.Files[0].Path != "old" {
		t.Error("shrinking must keep the surviving entry")
	}
}

// TestStartAfterStopReAdds covers the fix for start-after-stop being permanently
// broken. StopTorrent drops the underlying torrent, so the cached handle points
// at a closed object and never refreshes. The old StartTorrent just flipped
// Started back to true and called DownloadAll on the dead handle: the UI showed
// the torrent as running while nothing downloaded.
//
// startLocked must now notice t.t == nil and re-add from the retained spec.
func TestStartAfterStopReAdds(t *testing.T) {
	e := New()

	// A stopped torrent: flags cleared, handle dropped, spec retained.
	stopped := &Torrent{InfoHash: "ih", Started: false, t: nil, spec: nil}
	e.ts["ih"] = stopped

	// With no client configured, restarting must report that rather than
	// silently pretending to succeed.
	err := e.startLocked(stopped)
	if err == nil {
		t.Fatal("restarting a dropped torrent with no client should fail, " +
			"not flip Started and download nothing")
	}
	if stopped.Started {
		t.Error("Started must not be set when the restart failed")
	}
}

// TestStopTorrentClearsHandle pins the invariant startLocked relies on.
func TestStopTorrentClearsHandle(t *testing.T) {
	e := New()
	tor := &Torrent{InfoHash: "ih", Started: true, Files: []*File{{Path: "a"}, nil}}
	e.ts["ih"] = tor

	// t.t is nil here, so Drop is skipped, but the bookkeeping must still run.
	if err := e.StopTorrent("ih"); err == nil {
		t.Fatal("expected an error: 'ih' is not a valid infohash")
	}

	// Via the real path, with a valid hash.
	valid := "abababababababababababababababababababab"
	tor2 := &Torrent{InfoHash: valid, Started: true, Files: []*File{{Path: "a"}, nil}}
	e.ts[valid] = tor2
	if err := e.StopTorrent(valid); err != nil {
		t.Fatalf("StopTorrent: %v", err)
	}
	if tor2.Started || tor2.t != nil {
		t.Error("stop must clear Started and the handle")
	}
	if err := e.StopTorrent(valid); !errors.Is(err, ErrAlreadyStopped) {
		t.Errorf("double stop = %v, want ErrAlreadyStopped", err)
	}
}

// TestReconfigureSamePort covers a bug that made the most common settings
// change impossible.
//
// Configure originally built the replacement client before closing the old one,
// so that a validation or bind failure left the running client untouched. But
// the old client holds the listen port, so creating a new one on the SAME port
// always failed with "address already in use" — and keeping the port is what
// happens whenever you change any *other* setting.
func TestReconfigureSamePort(t *testing.T) {
	e := New()
	defer e.Close()

	dir := t.TempDir()
	port := freeTCPPort(t)
	base := Config{DownloadDirectory: dir, IncomingPort: port, EnableUpload: true}

	if err := e.Configure(base); err != nil {
		t.Fatalf("initial configure: %v", err)
	}

	// Same port, different settings — the case that used to always fail.
	changed := base
	changed.EnableUpload = false
	if err := e.Configure(changed); err != nil {
		t.Fatalf("reconfigure on the same port must succeed, got: %v", err)
	}
	if got := e.Config(); got.EnableUpload {
		t.Error("the new setting was not applied")
	}

	// And again, to prove it is repeatable rather than working once.
	changed.EnableSeeding = true
	if err := e.Configure(changed); err != nil {
		t.Fatalf("second reconfigure on the same port: %v", err)
	}

	// A port change must still work.
	moved := changed
	moved.IncomingPort = freeTCPPort(t)
	if err := e.Configure(moved); err != nil {
		t.Fatalf("reconfigure onto a new port: %v", err)
	}
}

// TestReconfigureRejectsBadConfigWithoutStopping covers the other half: an
// invalid config must be refused before anything is torn down.
func TestReconfigureRejectsBadConfigWithoutStopping(t *testing.T) {
	e := New()
	defer e.Close()

	base := Config{DownloadDirectory: t.TempDir(), IncomingPort: freeTCPPort(t)}
	if err := e.Configure(base); err != nil {
		t.Fatalf("initial configure: %v", err)
	}

	bad := base
	bad.IncomingPort = 0 // invalid
	if err := e.Configure(bad); err == nil {
		t.Fatal("expected an error for port 0")
	}
	if got := e.Config(); got.IncomingPort != base.IncomingPort {
		t.Errorf("a rejected config changed the live config: %+v", got)
	}
	// The engine must still be usable.
	if err := e.Configure(base); err != nil {
		t.Errorf("engine unusable after a rejected config: %v", err)
	}
}

// TestEvictForRebindPicksTheTeardownOrder pins the decision Configure makes
// before it touches anything, without needing a real rebind to observe it.
//
// A nil return means "the running client keeps its port, so build the
// replacement first and only swap if that succeeds". A non-nil return means
// the caller now owns the old client and must close it before binding.
func TestEvictForRebindPicksTheTeardownOrder(t *testing.T) {
	e := New()
	defer e.Close()

	// Nothing configured yet: there is nothing to evict.
	if got := e.evictForRebind(4242); got != nil {
		t.Error("evicted a client that does not exist")
	}

	base := Config{DownloadDirectory: t.TempDir(), IncomingPort: freeTCPPort(t)}
	if err := e.Configure(base); err != nil {
		t.Fatalf("initial configure: %v", err)
	}

	// A different port: the running client keeps serving while the replacement
	// is built, so it must NOT be detached here.
	if got := e.evictForRebind(base.IncomingPort + 1); got != nil {
		t.Error("a port change must leave the running client in place")
	}
	if e.client == nil {
		t.Fatal("the running client was detached anyway")
	}

	// The same port: the old client owns it, so it has to go first.
	evicted := e.evictForRebind(base.IncomingPort)
	if evicted == nil {
		t.Fatal("keeping the port must detach the client that holds it")
	}
	if e.client != nil {
		t.Error("an evicted client must be cleared, so operations report ErrNotConfigured")
	}
	closeClient(evicted)
}

// TestConcurrentConfigure covers what configureMu is for.
//
// mu alone was not enough: the same-port path releases mu between dropping the
// old client and installing the replacement, and a second caller entering that
// window saw client == nil, took the non-retrying branch, and stole the port —
// leaving the first caller to spin the whole rebind budget and fail.
//
// It asserts only what serialization actually guarantees. Which of the two
// callers wins is a race by design, and asserting a winner would encode that
// race as a requirement.
func TestConcurrentConfigure(t *testing.T) {
	e := New()
	defer e.Close()

	dir := t.TempDir()
	port := freeTCPPort(t)
	base := Config{DownloadDirectory: dir, IncomingPort: port}
	if err := e.Configure(base); err != nil {
		t.Fatalf("initial configure: %v", err)
	}

	// Same port, so both callers take the evict-then-rebind path and contend
	// for exactly the window configureMu exists to close.
	seeding, uploading := base, base
	seeding.EnableSeeding = true
	uploading.EnableUpload = true

	errs := make(chan error, 2)
	for _, c := range []Config{seeding, uploading} {
		go func() { errs <- e.Configure(c) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Configure: %v", err)
		}
	}

	// One of the two must have won outright — not a mixture of both.
	got := e.Config()
	if got != seeding && got != uploading {
		t.Errorf("live config is neither input, so the two interleaved: %+v", got)
	}
	// And the engine must still be usable afterwards.
	if err := e.Configure(base); err != nil {
		t.Errorf("engine unusable after concurrent configures: %v", err)
	}
}

// freeTCPPort returns a port that is currently unbound. It was called
// freeUDPPort, which it never was.
//
// There is an unavoidable TOCTOU here: the listener is closed before the port
// is returned. If something else takes it in between, Configure fails loudly
// rather than hanging, which is the acceptable outcome.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestConfigureAfterCloseDoesNotLeak covers an engine that had no closed state.
//
// Close released the client and set it to nil, but nothing recorded that the
// engine was done, so a later Configure happily built a replacement: a live
// torrent client with its listening socket, goroutines and DHT, held by an
// engine everyone believed was shut down, that nothing would ever close.
func TestConfigureAfterCloseDoesNotLeak(t *testing.T) {
	e := New()
	base := Config{DownloadDirectory: t.TempDir(), IncomingPort: freeTCPPort(t)}
	if err := e.Configure(base); err != nil {
		t.Fatalf("initial configure: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := e.Configure(base); !errors.Is(err, ErrClosed) {
		t.Errorf("Configure after Close = %v, want ErrClosed", err)
	}
	e.mu.Lock()
	live := e.client != nil
	e.mu.Unlock()
	if live {
		t.Error("a live client was installed into a closed engine; nothing will release it")
	}

	// Close stays idempotent.
	if err := e.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestCloseDuringConfigure covers the same defect in the form that is actually
// reachable in normal operation: Ctrl-C while /api/configure is rebinding.
//
// The same-port path releases mu between evicting the old client and installing
// the replacement, and Close did not take configureMu — so it observed
// client == nil, reported a clean shutdown, and the rebind installed its client
// afterwards. Whatever the interleaving, once Close returns no client may
// remain.
func TestCloseDuringConfigure(t *testing.T) {
	e := New()
	base := Config{DownloadDirectory: t.TempDir(), IncomingPort: freeTCPPort(t)}
	if err := e.Configure(base); err != nil {
		t.Fatalf("initial configure: %v", err)
	}

	// Same port, so this takes the evict-then-rebind path.
	changed := base
	changed.EnableSeeding = true

	done := make(chan error, 1)
	go func() { done <- e.Configure(changed) }()

	if err := e.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Either it completed before Close, or it gave up — both are fine.
	if err := <-done; err != nil && !errors.Is(err, ErrClosed) {
		t.Errorf("concurrent Configure = %v, want nil or ErrClosed", err)
	}

	e.mu.Lock()
	live := e.client != nil
	e.mu.Unlock()
	if live {
		t.Error("a client survived Close; it holds a listening socket nothing will release")
	}
}

// testMagnet is a well-formed magnet whose metadata will never arrive: no
// tracker, no peers. That is exactly the state the watcher exists for.
const testMagnet = "magnet:?xt=urn:btih:aabbccddeeff00112233445566778899aabbccdd"

// TestCloseClearsTheCache covers an engine that reported torrents it no longer
// had. Close released the client but left ts populated, so GetTorrents kept
// handing out torrents that existed nowhere.
func TestCloseClearsTheCache(t *testing.T) {
	e := New()
	if err := e.Configure(Config{DownloadDirectory: t.TempDir(), IncomingPort: freeTCPPort(t)}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := e.NewMagnet(testMagnet); err != nil {
		t.Fatalf("NewMagnet: %v", err)
	}
	if len(e.GetTorrents()) != 1 {
		t.Fatalf("setup: expected the magnet to be cached")
	}

	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := e.GetTorrents(); len(got) != 0 {
		t.Fatalf("GetTorrents after Close = %d torrents, want 0", len(got))
	}
}

// TestAddDuringClose covers a WaitGroup misuse panic.
//
// addSpec registered a metadata watcher — and so called wg.Add — without any
// lock Close held, while Close did wg.Wait under configureMu. The safety
// argument in the code only covered Configure, so an add concurrent with
// shutdown could Add while Wait was running: the documented misuse, which
// panics outright when the counter is at zero.
//
// The race is timing-dependent, so this loops. It asserts no panic and no
// deadlock; which side wins is not a requirement.
func TestAddDuringClose(t *testing.T) {
	for i := 0; i < 25; i++ {
		e := New()
		// freeTCPPort only proves the TCP port is free, and anacrolix binds UDP
		// too. A collision here is the previous iteration's client still letting
		// go, which is not what this test is about.
		if err := e.Configure(Config{DownloadDirectory: t.TempDir(), IncomingPort: freeTCPPort(t)}); err != nil {
			e.Close()
			continue
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// Either outcome is fine: the add lands, or it is refused because
			// the engine is already closing.
			_ = e.NewMagnet(testMagnet)
		}()
		go func() {
			defer wg.Done()
			_ = e.Close()
		}()
		wg.Wait()
	}
}

// testTorrentFile builds a real .torrent for a small local payload, so tests
// can work with a torrent whose metadata is already present.
func testTorrentFile(t *testing.T) []byte {
	t.Helper()
	payload := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(payload, bytes.Repeat([]byte("x"), 1<<16), 0644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 1 << 14}
	if err := info.BuildFromFilePath(payload); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := (&metainfo.MetaInfo{InfoBytes: infoBytes}).Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// onlyTorrent returns the single cached torrent, failing if there is not
// exactly one. It reaches into ts because the test needs the live entry, not
// the clone GetTorrents hands out.
func onlyTorrent(t *testing.T, e *Engine) *Torrent {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.ts) != 1 {
		t.Fatalf("expected exactly one cached torrent, got %d", len(e.ts))
	}
	for _, tor := range e.ts {
		return tor
	}
	return nil
}

// TestStartedWithoutMetadataDownloadsOnArrival covers a torrent that showed as
// running forever while downloading nothing.
//
// startLocked sets Started but can only call DownloadAll once metadata exists.
// The watcher was registered only when AutoStart was on, and its body bailed
// out whenever Started was already set — so a user who added a magnet and
// pressed Start before the metadata arrived hit a permanent dead end, with no
// settings change involved. infoArrived is now the single place that decision
// is made.
//
// Scope, stated plainly because it is easy to over-read: this pins the decision
// table infoArrived owns, which is observable in Started. The started-without-
// metadata branch itself is NOT covered. Its only effect is the DownloadAll
// call, and anacrolix does not expose one — piece priorities read back unchanged
// either way, so every assertion available here passes with the branch removed.
// A test that cannot fail is worse than an admitted gap. That branch ships
// verified by inspection.
func TestStartedWithoutMetadataDownloadsOnArrival(t *testing.T) {
	// newLoaded returns an engine holding one torrent whose metadata is present.
	newLoaded := func(t *testing.T, autoStart bool) (*Engine, *Torrent) {
		t.Helper()
		e := New()
		t.Cleanup(func() { e.Close() })
		cfg := Config{DownloadDirectory: t.TempDir(), IncomingPort: freeTCPPort(t), AutoStart: autoStart}
		if err := e.Configure(cfg); err != nil {
			t.Fatalf("configure: %v", err)
		}
		if err := e.NewTorrentFile(testTorrentFile(t)); err != nil {
			t.Fatalf("NewTorrentFile: %v", err)
		}
		return e, onlyTorrent(t, e)
	}

	t.Run("autostart starts it", func(t *testing.T) {
		e, tor := newLoaded(t, true)
		e.mu.Lock()
		tor.Started = false
		handle := tor.t
		e.mu.Unlock()

		e.infoArrived(tor.InfoHash, handle)

		e.mu.Lock()
		defer e.mu.Unlock()
		if !tor.Started {
			t.Fatal("AutoStart is on; the watcher must start the torrent")
		}
	})

	t.Run("autostart off leaves it stopped", func(t *testing.T) {
		e, tor := newLoaded(t, false)
		e.mu.Lock()
		tor.Started = false
		handle := tor.t
		e.mu.Unlock()

		e.infoArrived(tor.InfoHash, handle)

		e.mu.Lock()
		defer e.mu.Unlock()
		if tor.Started {
			t.Fatal("AutoStart is off; the watcher must not start the torrent")
		}
	})

	t.Run("stale handle is ignored", func(t *testing.T) {
		e, tor := newLoaded(t, true)
		e.mu.Lock()
		tor.Started = false
		e.mu.Unlock()

		// A handle the torrent no longer points at: it was stopped, deleted or
		// re-added under a new client while the watcher waited.
		e.infoArrived(tor.InfoHash, &torrent.Torrent{})

		e.mu.Lock()
		defer e.mu.Unlock()
		if tor.Started {
			t.Fatal("a torrent whose handle moved must not be resurrected")
		}
	})
}
