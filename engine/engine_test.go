package engine

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/ndelucca/nd.cloud.torrent/internal/testutil"
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

// TestViewWithFilesIsDeep guards the engine handing its live internal state to
// the server, which then marshals it from another goroutine.
//
// It no longer has to check that the internal handles stayed behind: Torrent has
// no field that could carry them, so that is unrepresentable rather than
// asserted. What still needs asserting is the Files slice, which is the one
// thing a shallow copy would share.
func TestViewWithFilesIsDeep(t *testing.T) {
	orig := &torrentState{
		Torrent: Torrent{InfoHash: "abc", Started: true},
		Files:   []File{{Path: "a.mkv", Percent: 10}, {Path: "b.mkv", Percent: 20}},
	}
	c := orig.viewWithFiles()

	c.Started = false
	c.Files[0].Percent = 99
	if !orig.Started {
		t.Error("the view shares the Started field")
	}
	if orig.Files[0].Percent != 10 {
		t.Error("the view shares the Files backing array")
	}
}

// TestViewRoundTrips checks that both views carry the record's identity and
// that viewWithFiles carries a file table.
//
// It does not — and cannot — assert that view() carries no files: Torrent has
// no such field, so the type system enforces that and a test claiming to would
// be asserting something unrepresentable. The separation itself is the point,
// because GetTorrents runs once per sample for every connected browser and the
// streamed row never renders a file table.
func TestViewRoundTrips(t *testing.T) {
	orig := &torrentState{
		Torrent: Torrent{InfoHash: "abc"},
		Files:   []File{{Path: "a.mkv"}},
	}
	if got := orig.view(); got.InfoHash != "abc" {
		t.Fatalf("view lost its fields: %+v", got)
	}
	if orig.viewWithFiles().Files == nil {
		t.Error("viewWithFiles dropped the file table")
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
	// Close even though nothing was configured: New starts the sampler, so an
	// engine that is never closed leaks a ticker goroutine per test.
	defer e.Close()

	// A stopped torrent: flags cleared, handle dropped, spec retained.
	stopped := &torrentState{Torrent: Torrent{InfoHash: "ih"}}
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
	defer e.Close()
	tor := &torrentState{Torrent: Torrent{InfoHash: "ih", Started: true}, Files: []File{{Path: "a"}}}
	e.ts["ih"] = tor

	// t.t is nil here, so Drop is skipped, but the bookkeeping must still run.
	if err := e.StopTorrent("ih"); err == nil {
		t.Fatal("expected an error: 'ih' is not a valid infohash")
	}

	// Via the real path, with a valid hash.
	valid := "abababababababababababababababababababab"
	tor2 := &torrentState{Torrent: Torrent{InfoHash: valid, Started: true}, Files: []File{{Path: "a"}}}
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

// TestConfigureAppliesAutoStartLive covers the one field that can change on a
// running client, and the fact that changing it does not disturb the client.
//
// AutoStart is the only setting anacrolix never sees: infoArrived is its sole
// reader. Everything else is baked into the client at construction, so a rebuild
// for AutoStart would drop and re-add every torrent for a value the client has
// no opinion about.
func TestConfigureAppliesAutoStartLive(t *testing.T) {
	e := New()
	defer e.Close()

	base := Config{DownloadDirectory: t.TempDir(), EnableUpload: true}
	base.IncomingPort = mustConfigure(t, e, base)

	e.mu.Lock()
	before := e.client
	e.mu.Unlock()

	changed := base
	changed.AutoStart = !base.AutoStart
	if err := e.Configure(changed); err != nil {
		t.Fatalf("changing AutoStart must not need a restart, got: %v", err)
	}
	if got := e.Config(); got.AutoStart != changed.AutoStart {
		t.Error("AutoStart was not applied")
	}

	e.mu.Lock()
	after := e.client
	e.mu.Unlock()
	if before != after {
		t.Error("the client was rebuilt for a field it never reads")
	}
}

// TestConfigureRejectsChangesNeedingARestart covers everything the client bakes
// in at construction.
//
// None of these can be applied to a running client: DataDir is read once to
// build the default storage; NoUpload, Seed and HeaderObfuscationPolicy are read
// live but from a struct the client's own goroutines touch with no lock we hold;
// IncomingPort is the listening socket. Reporting is the honest option — the
// alternative is a setting that appears to save and does nothing.
func TestConfigureRejectsChangesNeedingARestart(t *testing.T) {
	e := New()
	defer e.Close()

	dir := t.TempDir()
	base := Config{DownloadDirectory: dir, EnableUpload: true}
	base.IncomingPort = mustConfigure(t, e, base)

	for name, mutate := range map[string]func(*Config){
		"port":       func(c *Config) { c.IncomingPort = testutil.FreePort(t) },
		"directory":  func(c *Config) { c.DownloadDirectory = t.TempDir() },
		"upload":     func(c *Config) { c.EnableUpload = !c.EnableUpload },
		"seeding":    func(c *Config) { c.EnableSeeding = !c.EnableSeeding },
		"encryption": func(c *Config) { c.DisableEncryption = !c.DisableEncryption },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if err := e.Configure(changed); !errors.Is(err, ErrRestartRequired) {
				t.Fatalf("Configure(%s changed) = %v, want ErrRestartRequired", name, err)
			}
			if got := e.Config(); got != base {
				t.Errorf("a rejected change altered the live config: %+v", got)
			}
			e.mu.Lock()
			live := e.client != nil
			e.mu.Unlock()
			if !live {
				t.Error("a rejected change took the client down")
			}
		})
	}

	// Re-applying the identical config is a no-op, not a restart request: a
	// settings form resubmitting every field unchanged must not report one.
	if err := e.Configure(base); err != nil {
		t.Errorf("re-applying the same config = %v, want nil", err)
	}
}

// TestNeedsRestart is the table the function above cannot be read off. It is
// pure — no ports, no wall clock — and it is the one piece here whose failure
// mode is silent: a new Config field that nobody adds to a list would simply
// stop requiring a restart.
func TestNeedsRestart(t *testing.T) {
	base := Config{
		DownloadDirectory: "/d", IncomingPort: 1234,
		EnableUpload: true, EnableSeeding: true,
		DisableEncryption: true, AutoStart: true,
	}
	for name, tc := range map[string]struct {
		mutate func(*Config)
		want   bool
	}{
		"identical":         {func(*Config) {}, false},
		"AutoStart":         {func(c *Config) { c.AutoStart = !c.AutoStart }, false},
		"DownloadDirectory": {func(c *Config) { c.DownloadDirectory = "/other" }, true},
		"IncomingPort":      {func(c *Config) { c.IncomingPort = 4321 }, true},
		"EnableUpload":      {func(c *Config) { c.EnableUpload = !c.EnableUpload }, true},
		"EnableSeeding":     {func(c *Config) { c.EnableSeeding = !c.EnableSeeding }, true},
		"DisableEncryption": {func(c *Config) { c.DisableEncryption = !c.DisableEncryption }, true},
	} {
		t.Run(name, func(t *testing.T) {
			next := base
			tc.mutate(&next)
			if got := needsRestart(base, next); got != tc.want {
				t.Errorf("needsRestart(%s) = %t, want %t", name, got, tc.want)
			}
		})
	}
}

// TestReconfigureRejectsBadConfigWithoutStopping covers the other half: an
// invalid config must be refused before anything is torn down.
func TestReconfigureRejectsBadConfigWithoutStopping(t *testing.T) {
	e := New()
	defer e.Close()

	base := Config{DownloadDirectory: t.TempDir()}
	base.IncomingPort = mustConfigure(t, e, base)

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

// mustConfigure configures e on a free port, retrying if the bind loses a race.
//
// testutil.FreePort closes its probe listeners before the caller binds, so
// between the two another process can claim the port — and `go test ./...` runs
// packages in parallel, so the server package's fixtures are doing exactly that
// at the same time. The retry is the only way to close that window without
// serialising the suite; the alternative is an intermittent failure that blames
// whichever test happened to draw the port.
//
// c.IncomingPort is overwritten on each attempt, so the port that was actually
// bound is returned. A test that reconfigures on "the same port" must write it
// back into its own config, or it asks for a port the engine is not holding and
// silently exercises the restart-required path instead.
func mustConfigure(t *testing.T, e *Engine, c Config) int {
	t.Helper()
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		c.IncomingPort = testutil.FreePort(t)
		if err = e.Configure(c); err == nil {
			return c.IncomingPort
		}
		if !strings.Contains(err.Error(), "address already in use") {
			t.Fatalf("configure: %v", err)
		}
	}
	t.Fatalf("configure kept losing the port race: %v", err)
	return 0
}

// TestConcurrentConfigure pins that Configure is atomic: the live config is
// always one whole input, never a mixture of two.
//
// Configure now runs entirely under mu, so this is nearly free — which is the
// point. It used to release mu midway through rebuilding the client, and a
// second caller entering that window saw client == nil and stole the port. This
// is the test that fails if anything ever moves work back outside the lock.
//
// It asserts only what serialization guarantees. Which caller wins is a race by
// design, and asserting a winner would encode that race as a requirement.
func TestConcurrentConfigure(t *testing.T) {
	e := New()
	defer e.Close()

	base := Config{DownloadDirectory: t.TempDir()}
	base.IncomingPort = mustConfigure(t, e, base)

	// AutoStart is the only field that applies live, so it is the only one two
	// callers can both succeed at.
	on, off := base, base
	on.AutoStart = true
	off.AutoStart = false

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		c := on
		if i%2 == 1 {
			c = off
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.Configure(c); err != nil {
				t.Errorf("concurrent Configure: %v", err)
			}
		}()
	}
	wg.Wait()

	got := e.Config()
	if got != on && got != off {
		t.Errorf("live config is neither input, so two calls interleaved: %+v", got)
	}
	if err := e.Configure(base); err != nil {
		t.Errorf("engine unusable after concurrent configures: %v", err)
	}
}

// TestConfigureAfterCloseDoesNotLeak covers an engine that had no closed state.
//
// Close released the client and set it to nil, but nothing recorded that the
// engine was done, so a later Configure happily built a replacement: a live
// torrent client with its listening socket, goroutines and DHT, held by an
// engine everyone believed was shut down, that nothing would ever close.
func TestConfigureAfterCloseDoesNotLeak(t *testing.T) {
	e := New()
	base := Config{DownloadDirectory: t.TempDir()}
	base.IncomingPort = mustConfigure(t, e, base)
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

// TestCloseDuringConfigure is the shutdown race in the form that is actually
// reachable: Ctrl-C while /api/configure is in flight.
//
// Whatever the interleaving, once Close returns no client may remain — a
// surviving one holds a listening socket, goroutines and a DHT node that
// nothing will ever release.
func TestCloseDuringConfigure(t *testing.T) {
	e := New()
	base := Config{DownloadDirectory: t.TempDir()}
	base.IncomingPort = mustConfigure(t, e, base)

	changed := base
	changed.AutoStart = !base.AutoStart

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
		// testutil.FreePort is still a TOCTOU, and anacrolix binds UDP
		// too. A collision here is the previous iteration's client still letting
		// go, which is not what this test is about.
		if err := e.Configure(Config{DownloadDirectory: t.TempDir(), IncomingPort: testutil.FreePort(t)}); err != nil {
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
// the view GetTorrents hands out.
func onlyTorrent(t *testing.T, e *Engine) *torrentState {
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
	// It always adds with AutoStart off, and it waits for addSpec's watcher to
	// finish before returning. Both halves are needed and only the first was
	// here before.
	//
	// The metadata is already present, so GotInfo is closed and that watcher
	// fires at once on its own goroutine. Each subtest then flips AutoStart
	// under the lock — and a watcher still in flight reads the *new* value,
	// starts the torrent, and fails the assertion for a reason the subtest is
	// not about. "stale handle is ignored" failed roughly one run in five that
	// way, blaming the guard it exists to cover.
	//
	// Waiting on the goroutine rather than sleeping: the watcher exits once
	// infoArrived returns, so a count back at the baseline means it is done.
	newLoaded := func(t *testing.T) (*Engine, *torrentState) {
		t.Helper()
		base := liveWatchers()
		e := New()
		t.Cleanup(func() { e.Close() })
		mustConfigure(t, e, Config{DownloadDirectory: t.TempDir(), AutoStart: false})
		if err := e.NewTorrentFile(testTorrentFile(t)); err != nil {
			t.Fatalf("NewTorrentFile: %v", err)
		}
		waitWatchers(t, base, 0)
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

// TestSampleHandlesBytesGoingBackwards pins the re-add case: the old rate is
// meaningless once progress resets, and a negative delta must not produce a
// negative rate.
func TestSampleHandlesBytesGoingBackwards(t *testing.T) {
	t0 := time.Now()
	tor := &torrentState{Torrent: Torrent{Size: 10_000}}
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
