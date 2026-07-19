package engine

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
		Files:    []File{{Path: "a.mkv", Percent: 10}, {Path: "b.mkv", Percent: 20}},
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
	if c.t != nil || c.spec != nil {
		t.Error("clone must not carry internal handles out of the engine")
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
	tor := &Torrent{InfoHash: "ih", Started: true, Files: []File{{Path: "a"}}}
	e.ts["ih"] = tor

	// t.t is nil here, so Drop is skipped, but the bookkeeping must still run.
	if err := e.StopTorrent("ih"); err == nil {
		t.Fatal("expected an error: 'ih' is not a valid infohash")
	}

	// Via the real path, with a valid hash.
	valid := "abababababababababababababababababababab"
	tor2 := &Torrent{InfoHash: valid, Started: true, Files: []File{{Path: "a"}}}
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
// mustConfigure configures e on a free port, retrying if the bind loses a race.
//
// freeTCPPort closes its probe listeners before the caller binds, so between the
// two another process can claim the port — and `go test ./...` runs packages in
// parallel, so the server package's fixtures are doing exactly that at the same
// time. The retry is the only way to close that window without serialising the
// suite; the alternative is an intermittent failure that blames whichever test
// happened to draw the port.
//
// c.IncomingPort is overwritten on each attempt.
func mustConfigure(t *testing.T, e *Engine, c Config) {
	t.Helper()
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		c.IncomingPort = freeTCPPort(t)
		if err = e.Configure(c); err == nil {
			return
		}
		if !strings.Contains(err.Error(), "address already in use") {
			t.Fatalf("configure: %v", err)
		}
	}
	t.Fatalf("configure kept losing the port race: %v", err)
}

// freeTCPPort returns a port free on both TCP and UDP.
//
// Both, because anacrolix binds TCP *and* UDP on the listen port: a
// TCP-only check handed out ports whose UDP half was taken, and Configure then
// failed with "subsequent listen: bind: address already in use". That surfaced
// as an intermittent failure in whichever test happened to draw the port,
// blaming the code under test for a collision in the fixture.
//
// Still a TOCTOU — the listeners are closed before the caller binds — but
// checking both halves removes the systematic collisions, which were the
// common case. The retry loop covers a genuine race with another process.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		// Probe the UDP half of the same port before releasing the TCP one, so
		// nothing else can claim the pair in between.
		pc, uerr := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
		l.Close()
		if uerr != nil {
			continue // UDP half taken; draw another
		}
		pc.Close()
		return port
	}
	t.Fatal("no port free on both TCP and UDP after 20 attempts")
	return 0
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
	mustConfigure(t, e, Config{DownloadDirectory: t.TempDir(), IncomingPort: 0})
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
//
// Iterations that fail to configure are skipped but counted, and the test fails
// if too few real ones ran. Without that it could pass having exercised the race
// zero times — a test that can silently degenerate to a no-op is indistinguishable
// from one that works.
func TestAddDuringClose(t *testing.T) {
	const rounds = 25
	exercised := 0
	for i := 0; i < rounds; i++ {
		e := New()
		// freeTCPPort only proves the TCP port is free, and anacrolix binds UDP
		// too. A collision here is the previous iteration's client still letting
		// go, which is not what this test is about.
		if err := e.Configure(Config{DownloadDirectory: t.TempDir(), IncomingPort: freeTCPPort(t)}); err != nil {
			e.Close()
			continue
		}
		exercised++

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
	// Port collisions are expected occasionally; a majority failing means the
	// test is measuring the environment rather than the code.
	if exercised < rounds/2 {
		t.Fatalf("only %d of %d rounds configured an engine; the race was barely exercised",
			exercised, rounds)
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
	//
	// It always adds with AutoStart off. addSpec registers a watcher, and with
	// the metadata already there that watcher fires immediately on its own
	// goroutine — so adding with AutoStart on would race every assertion below,
	// and the "autostart starts it" case would pass whether or not the call
	// under test did anything. Each subtest sets the flag afterwards, under the
	// lock, so the only thing that can act on it is the infoArrived call itself.
	newLoaded := func(t *testing.T) (*Engine, *Torrent) {
		t.Helper()
		e := New()
		t.Cleanup(func() { e.Close() })
		cfg := Config{DownloadDirectory: t.TempDir(), IncomingPort: freeTCPPort(t), AutoStart: false}
		if err := e.Configure(cfg); err != nil {
			t.Fatalf("configure: %v", err)
		}
		if err := e.NewTorrentFile(testTorrentFile(t)); err != nil {
			t.Fatalf("NewTorrentFile: %v", err)
		}
		return e, onlyTorrent(t, e)
	}

	t.Run("autostart starts it", func(t *testing.T) {
		e, tor := newLoaded(t)
		e.mu.Lock()
		e.config.AutoStart = true
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
		e, tor := newLoaded(t)
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
		e, tor := newLoaded(t)
		e.mu.Lock()
		e.config.AutoStart = true
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

// TestExtraReadsDoNotDisturbTheRate covers a rate that got quieter the more
// people looked at it.
//
// GetTorrents refreshes the cache on read, so every caller — the 1s poll loop,
// but also GET /api/state and opening a torrent's Files panel — produced a
// reading. Downloaded and updatedAt were advanced by all of them while the rate
// was only recomputed when the interval happened to be positive, so an extra
// read microseconds after the poll's consumed the interval the next real sample
// needed. Two clients polling once a second roughly halved every rate shown.
//
// The three fields are one sample: they move together or not at all.
func TestExtraReadsDoNotDisturbTheRate(t *testing.T) {
	t0 := time.Now()
	tor := &Torrent{Size: 10_000}

	// First reading: no interval yet, so no rate.
	tor.sample(0, t0)
	if tor.DownloadRate != 0 {
		t.Fatalf("first sample produced a rate of %v, want 0", tor.DownloadRate)
	}

	// One second later, 1000 bytes in: 1000 B/s.
	tor.sample(1000, t0.Add(time.Second))
	if tor.DownloadRate != 1000 {
		t.Fatalf("DownloadRate = %v, want 1000", tor.DownloadRate)
	}

	// Two extra readers arrive right behind the poll. Each must be dropped
	// whole — not applied to Downloaded while skipping the rate.
	tor.sample(1001, t0.Add(time.Second+time.Millisecond))
	tor.sample(1002, t0.Add(time.Second+2*time.Millisecond))
	if tor.DownloadRate != 1000 {
		t.Fatalf("an extra read changed the rate to %v", tor.DownloadRate)
	}
	if tor.Downloaded != 1000 {
		t.Fatalf("an extra read advanced Downloaded to %d, stealing the next "+
			"sample's interval", tor.Downloaded)
	}

	// The next real poll must still measure against t0+1s, not against the
	// readers. 1000 more bytes over 1s is still 1000 B/s.
	tor.sample(2000, t0.Add(2*time.Second))
	if tor.DownloadRate != 1000 {
		t.Fatalf("DownloadRate = %v after a real sample, want 1000", tor.DownloadRate)
	}
}

// TestSampleHandlesBytesGoingBackwards pins the re-add case: the old rate is
// meaningless once progress resets, and a negative delta must not produce a
// negative rate.
func TestSampleHandlesBytesGoingBackwards(t *testing.T) {
	t0 := time.Now()
	tor := &Torrent{Size: 10_000}
	tor.sample(5000, t0)
	tor.sample(6000, t0.Add(time.Second))
	tor.sample(0, t0.Add(2*time.Second))

	if tor.DownloadRate != 0 {
		t.Fatalf("DownloadRate = %v after progress reset, want 0", tor.DownloadRate)
	}
	if tor.Downloaded != 0 {
		t.Fatalf("Downloaded = %d, want the new reading", tor.Downloaded)
	}
}

// TestAddRejectsSpecWithoutInfohash covers a remote panic.
//
// AddTorrentSpec panics with "v1 infohash must be nonzero or v2 infohash must
// be set", and TorrentSpecFromMagnetUri happily produces such a spec:
// "magnet:?nonsense" parses without error and yields a zero hash. Anyone who
// could reach /api/add could therefore kill a request handler. Found by driving
// the running server, not by a test — which is why this one exists.
func TestAddRejectsSpecWithoutInfohash(t *testing.T) {
	e := New()
	defer e.Close()
	mustConfigure(t, e, Config{DownloadDirectory: t.TempDir(), IncomingPort: 0})

	for _, uri := range []string{"magnet:?nonsense", "magnet:?dn=name-only", "magnet:?xt=urn:btih:"} {
		t.Run(uri, func(t *testing.T) {
			// The assertion is as much "does not panic" as it is the error.
			err := e.NewMagnet(uri)
			if err == nil {
				t.Fatalf("%q was accepted; it has no infohash", uri)
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("error = %v, want it to wrap ErrInvalidInput so the server answers 400", err)
			}
		})
	}
}
