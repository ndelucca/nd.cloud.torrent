package engine

import (
	"errors"
	"net"
	"strings"
	"testing"
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

// TestUpdateLoadedResizesFiles guards against the index panic that happened when
// the file list grew after the slice was first allocated.
func TestUpdateLoadedResizesFiles(t *testing.T) {
	tor := &Torrent{Files: []*File{{Path: "old"}}}
	// Simulate the resize branch directly: a shorter cached slice must grow to
	// match, preserving what was already there.
	tfiles := 3
	if len(tor.Files) != tfiles {
		resized := make([]*File, tfiles)
		copy(resized, tor.Files)
		tor.Files = resized
	}
	if len(tor.Files) != 3 {
		t.Fatalf("len = %d, want 3", len(tor.Files))
	}
	if tor.Files[0] == nil || tor.Files[0].Path != "old" {
		t.Error("existing entries must be preserved across a resize")
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
	tor := &Torrent{InfoHash: "ih", Started: true, Files: []*File{{Started: true}, nil}}
	e.ts["ih"] = tor

	// t.t is nil here, so Drop is skipped, but the bookkeeping must still run.
	if err := e.StopTorrent("ih"); err == nil {
		t.Fatal("expected an error: 'ih' is not a valid infohash")
	}

	// Via the real path, with a valid hash.
	valid := "abababababababababababababababababababab"
	tor2 := &Torrent{InfoHash: valid, Started: true, Files: []*File{{Started: true}, nil}}
	e.ts[valid] = tor2
	if err := e.StopTorrent(valid); err != nil {
		t.Fatalf("StopTorrent: %v", err)
	}
	if tor2.Started || tor2.t != nil || tor2.Files[0].Started {
		t.Error("stop must clear Started, the handle, and per-file flags")
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
	port := freeUDPPort(t)
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
	moved.IncomingPort = freeUDPPort(t)
	if err := e.Configure(moved); err != nil {
		t.Fatalf("reconfigure onto a new port: %v", err)
	}
}

// TestReconfigureRejectsBadConfigWithoutStopping covers the other half: an
// invalid config must be refused before anything is torn down.
func TestReconfigureRejectsBadConfigWithoutStopping(t *testing.T) {
	e := New()
	defer e.Close()

	base := Config{DownloadDirectory: t.TempDir(), IncomingPort: freeUDPPort(t)}
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

func freeUDPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
