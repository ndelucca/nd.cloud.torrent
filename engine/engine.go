// Package engine wraps anacrolix/torrent in a small, server-friendly facade:
// one client, a mutable Config, and a cache of view models keyed by hex
// infohash. It knows nothing about HTTP.
package engine

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// infoHashHexLen is the length of a hex-encoded 20-byte BitTorrent infohash.
const infoHashHexLen = 2 * metainfo.HashSize

// Sentinel errors. The server maps these onto HTTP statuses and decides what
// the user is shown, so they are ordinary lowercase Go error strings — wrap
// them with %w.
var (
	ErrNotConfigured  = errors.New("engine is not configured")
	ErrMissingTorrent = errors.New("missing torrent")
	ErrAlreadyStarted = errors.New("already started")
	ErrAlreadyStopped = errors.New("already stopped")
	ErrClosed         = errors.New("engine is closed")
	// ErrInvalidInput marks a failure the caller caused: a malformed magnet
	// URI, unparseable .torrent bytes, a bad infohash. It is what lets the
	// server show the wrapped detail — which is useful, bounded parser prose —
	// while keeping a fixed message for everything else.
	ErrInvalidInput = errors.New("invalid input")
)

// Engine wraps anacrolix/torrent in a server-friendly facade: one client, a
// mutable Config, and a cache of Torrent view models keyed by hex infohash.
//
// Every exported method is safe for concurrent use. All access to client, config
// and ts happens under mu.
type Engine struct {
	mu     sync.Mutex
	client *torrent.Client
	config Config
	ts     map[string]*torrentState

	// Lifecycle for the per-torrent metadata watchers.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// sampled carries one notification per completed sample. See Sampled.
	sampled chan struct{}
}

// Sampled returns a channel that receives after every sample.
//
// It is a hint, not a queue. Sends are non-blocking and dropped when the buffer
// is full, so nobody listening costs nothing and — the load-bearing half — a
// slow reader can never stall the sampler. That is what keeps SampleInterval an
// honest measurement window for DownloadRate.
//
// It is never closed: a closed channel makes a select spin, so a consumer must
// exit on its own context instead.
//
// It fires on every tick, including ones where the engine has no client. The
// render loop also draws the download tree and the host stats, which have to
// keep moving while the engine is unconfigured.
func (e *Engine) Sampled() <-chan struct{} { return e.sampled }

// SampleInterval is the engine's sampling cadence, and therefore the window
// DownloadRate is measured over. It is a constant rather than a Config field: a
// user-tunable window is a rate whose meaning changes under the user.
//
// Exported because the render loop is driven by Sampled rather than a clock of
// its own, so a caller reasoning about how often it wakes needs this number.
const SampleInterval = 1 * time.Second

func New() *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		ts:     map[string]*torrentState{},
		ctx:    ctx,
		cancel: cancel,
		// Buffered and coalesced: a reader slower than the sampler sees one
		// pending signal, never a queue.
		sampled: make(chan struct{}, 1),
	}
	// Safe without a lock, unlike watchInfoLocked's Add: New has not returned
	// the pointer, so no other goroutine can hold the engine and no Close — and
	// therefore no wg.Wait — can be in flight. The Add strictly happens-before
	// every possible Wait.
	e.wg.Add(1)
	go e.sampleLoop()
	return e
}

// sampleLoop takes a reading on a fixed cadence for the engine's whole life.
//
// It is started here rather than by Configure so a rebind needs no sampler
// lifecycle at all: the loop keeps ticking, refresh finds client == nil during
// the evict window and returns, and picks up the replacement on the next tick.
// The cost is that the first tick after a rebind spans ~2s rather than ~1s,
// which stays correct because the rate is computed from timestamps rather than
// assumed from the cadence.
//
// Sampling is not gated on whether anyone is watching. cpu.Percent-style
// reasoning applies: the interval between readings defines the measurement
// window, so skipping ticks would make the first reading after an idle spell an
// average over however long nobody was connected.
func (e *Engine) sampleLoop() {
	defer e.wg.Done()
	t := time.NewTicker(SampleInterval)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-t.C:
			e.refresh(now)
			// Outside refresh, so this also fires on ticks where the engine has
			// no client. See Sampled.
			select {
			case e.sampled <- struct{}{}:
			default: // the last signal is still pending; coalesce
			}
		}
	}
}

// Config returns a copy of the current configuration.
func (e *Engine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.config
}

// ErrRestartRequired reports a setting that cannot be applied to a running
// client. It is not a failure of the request: the server persists the config
// anyway, so the change takes effect on the next start.
var ErrRestartRequired = errors.New("this setting needs a restart to take effect")

// Configure applies a configuration.
//
// Everything the client bakes in at construction is fixed for the process
// lifetime, verified against anacrolix/torrent v1.59.1:
//
//   - DataDir is read exactly once, in Client.init, to build the default
//     storage. Writing it on a live client is a silent no-op.
//   - NoUpload, Seed and HeaderObfuscationPolicy are read live, but from a
//     *ClientConfig the client aliases (Client.init keeps the caller's pointer)
//     and its own goroutines read without any lock we hold. Writing them is a
//     data race, and only partly effective anyway: HeaderObfuscationPolicy is
//     snapshotted per dial.
//   - IncomingPort is the listening socket.
//
// There are no Client setters for any of them. So a change to any one reports
// ErrRestartRequired rather than pretending. AutoStart is the exception, and the
// only one: the client never sees it — infoArrived is the sole reader — so it
// applies live.
//
// The alternative, rebuilding the client in place, is what this replaces. It
// needed a second mutex, an evict-then-retry dance against a listening socket
// the kernel had not released yet, and a re-add of every cached torrent into the
// replacement — and it silently dropped bytes already on disk when the download
// directory moved.
func (e *Engine) Configure(c Config) error {
	// Validation first, before the restart check: a config that is invalid on
	// its own terms must report *that*, not "this needs a restart". The server
	// test for an out-of-range port in the config file depends on this order.
	if err := c.Validate(); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx.Err() != nil {
		return ErrClosed
	}

	if e.client != nil {
		if needsRestart(e.config, c) {
			return ErrRestartRequired
		}
		e.config = c // AutoStart, and nothing else reaches this line
		return nil
	}

	client, err := torrent.NewClient(clientConfig(c))
	if err != nil {
		return fmt.Errorf("failed to start torrent client: %w", err)
	}
	e.client = client
	e.config = c
	return nil
}

// needsRestart reports whether moving from old to next requires a new client.
//
// Written as "copy across what applies live, then compare" rather than as a list
// of fields, so a field added to Config requires a restart by default. That is
// the safe direction, and it is not something anyone has to remember.
func needsRestart(old, next Config) bool {
	old.AutoStart = next.AutoStart
	return old != next
}

// clientConfig translates our Config into anacrolix's. Pure, so the mapping can
// be read without any of the lifecycle around it.
func clientConfig(c Config) *torrent.ClientConfig {
	tc := torrent.NewDefaultClientConfig()
	tc.DataDir = c.DownloadDirectory
	tc.NoUpload = !c.EnableUpload
	tc.Seed = c.EnableSeeding
	tc.ListenPort = c.IncomingPort
	tc.HeaderObfuscationPolicy.Preferred = !c.DisableEncryption
	tc.HeaderObfuscationPolicy.RequirePreferred = false
	return tc
}

// Close shuts the engine down and releases the underlying client. It is
// idempotent, and once it has run the engine stays closed: Configure reports
// ErrClosed rather than building a client nothing would ever release.
func (e *Engine) Close() error {
	// Cancel before taking mu, which is what makes the wg.Wait below safe — see
	// the note there.
	e.cancel()

	// Configure runs entirely under mu, so there is no window in which it holds
	// a half-built client this could miss. Taking mu here is therefore enough to
	// serialize against it: either Configure installed its client before this
	// point and the client is released below, or it runs after and finds the
	// context already cancelled.
	e.mu.Lock()
	client := e.client
	e.client = nil
	// Drop the cache. Without this, GetTorrents on a closed engine kept
	// returning torrents that no longer exist anywhere.
	clear(e.ts)
	e.mu.Unlock()

	// mu must be released before this Wait, and that is load-bearing rather than
	// incidental: the sampler takes mu on every tick, so waiting while holding it
	// deadlocks immediately.
	//
	// The counter is safe against concurrent Add for two separate reasons. The
	// sampler's Add happens in New, before the engine is reachable. Every
	// watcher's Add happens under mu after an e.ctx.Err() check, and cancel ran
	// before mu was taken above — so a watcher is either already counted, or
	// registers after mu is released and sees the cancelled context. Waiting
	// without that is the documented "Add called concurrently with Wait" misuse,
	// which panics at a zero counter.
	e.wg.Wait()

	if client == nil {
		return nil
	}
	return errors.Join(client.Close()...)
}

// NewMagnet adds a torrent from a magnet URI.
func (e *Engine) NewMagnet(magnetURI string) error {
	spec, err := torrent.TorrentSpecFromMagnetUri(magnetURI)
	if err != nil {
		return fmt.Errorf("%w: invalid magnet URI: %s", ErrInvalidInput, err)
	}
	return e.addSpec(spec)
}

// NewTorrentFile adds a torrent from raw .torrent file bytes. Keeping the
// parsing here means callers do not need to import anacrolix/torrent.
func (e *Engine) NewTorrentFile(data []byte) error {
	info, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%w: invalid torrent file: %s", ErrInvalidInput, err)
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(info)
	if err != nil {
		return fmt.Errorf("%w: invalid torrent file: %s", ErrInvalidInput, err)
	}
	return e.addSpec(spec)
}

func (e *Engine) addSpec(spec *torrent.TorrentSpec) error {
	// AddTorrentSpec *panics* on a spec with no infohash, and the magnet parser
	// happily produces one: "magnet:?nonsense" parses without error and yields a
	// zero hash. Anyone who can reach /api/add could therefore take down a
	// request handler, so this check is load-bearing, not defensive padding.
	if spec.InfoHash.IsZero() && !spec.InfoHashV2.Ok {
		return fmt.Errorf("%w: no infohash in torrent source", ErrInvalidInput)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client == nil {
		return ErrNotConfigured
	}
	tt, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		return fmt.Errorf("torrent error: %w", err)
	}
	t := e.upsertLocked(tt, time.Now())
	t.spec = spec
	// Unconditional, not gated on AutoStart: the watcher is also what resumes a
	// torrent the user starts before its metadata lands. When the info is
	// already present GotInfo is closed, so the watcher settles immediately.
	e.watchInfoLocked(t, tt)
	return nil
}

// watchInfoLocked arranges for infoArrived to run once the torrent's metadata
// resolves. Callers must hold e.mu.
//
// Exactly one watcher per torrent, parked on the handle it was registered for.
// The stopWatch below is what holds that: registering unconditionally, as this
// once did, parks a second watcher whenever AddTorrentSpec returns the existing
// handle for a duplicate add. The watching check on top is not what makes it
// correct — it only spares a handle already covered a pointless cancel and
// respawn.
//
// Registering under the lock is what makes wg.Add safe against Close's wg.Wait:
// Close takes mu and cancels the context before waiting, so a watcher is either
// already counted or never registered.
func (e *Engine) watchInfoLocked(t *torrentState, tt *torrent.Torrent) {
	if e.ctx.Err() != nil {
		return
	}
	if t.watching == tt {
		return
	}
	t.stopWatch()

	// A child of e.ctx, so Close still releases every watcher at once, while
	// stopping or deleting one torrent releases only its own.
	ctx, cancel := context.WithCancel(e.ctx)
	t.watching = tt
	t.cancelWatch = cancel

	ih := t.InfoHash
	e.wg.Add(1)
	go func() {
		// Unregisters the child from e.ctx on every path, including the one
		// where the metadata simply arrives and nobody cancels.
		defer cancel()
		defer e.wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-tt.GotInfo():
		}
		e.infoArrived(ih, tt)
	}()
}

// infoArrived is the single place the post-metadata decision is made. It is a
// method rather than a closure so a test can drive it without racing a real
// metadata fetch.
func (e *Engine) infoArrived(ih string, tt *torrent.Torrent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.ts[ih]
	// t.t != tt means the torrent was stopped, deleted or re-added under a new
	// client while we waited — do not resurrect it.
	if !ok || t.t != tt {
		return
	}
	if t.Started {
		// Started before the metadata arrived, so startLocked could not call
		// DownloadAll. This is the only place that can put it right; without it
		// the torrent stays flagged as running while downloading nothing, for
		// the lifetime of the process.
		tt.DownloadAll()
		return
	}
	if !e.config.AutoStart {
		return
	}
	if err := e.startLocked(t); err != nil {
		log.Printf("engine: auto-start %s: %s", ih, err)
	}
}

// GetTorrents returns a deep copy of the last sample.
//
// It is a pure read: it does not touch the client and does not advance any
// torrent's reading. Sampling belongs to refresh alone — see sampleLoop — and
// there is deliberately no exported way to force one. A Refresh method looks
// like an obvious convenience and is not: if reads sampled, every extra reader
// (/api/state, opening a Files panel) would steal the interval the next real
// sample needs, and two clients polling at 1 Hz would roughly halve every rate
// on the page.
//
// The copy matters: the caller marshals this concurrently with engine mutations.
//
// No file lists — see Torrent. This runs once per sample for every connected
// browser, and the streamed row discards them.
func (e *Engine) GetTorrents() map[string]*Torrent {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]*Torrent, len(e.ts))
	for ih, t := range e.ts {
		out[ih] = t.view()
	}
	return out
}

// GetTorrentsWithFiles is GetTorrents plus the file tables, for /api/state. One
// document per request rather than a per-tick cost.
func (e *Engine) GetTorrentsWithFiles() map[string]*TorrentWithFiles {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]*TorrentWithFiles, len(e.ts))
	for ih, t := range e.ts {
		out[ih] = t.viewWithFiles()
	}
	return out
}

// TorrentWithFiles returns one torrent by hex infohash, file table included.
//
// A keyed lookup, so answering "what is in this row" costs one torrent's files
// rather than a copy of every torrent and every file in the engine.
func (e *Engine) TorrentWithFiles(infohash string) (*TorrentWithFiles, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.getLocked(infohash)
	if err != nil {
		return nil, err
	}
	return t.viewWithFiles(), nil
}

// refresh takes one progress reading for every torrent the client holds. It is
// the only thing that samples.
func (e *Engine) refresh(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client == nil {
		return
	}
	for _, tt := range e.client.Torrents() {
		e.upsertLocked(tt, now)
	}
}

func (e *Engine) upsertLocked(tt *torrent.Torrent, now time.Time) *torrentState {
	ih := tt.InfoHash().HexString()
	t, ok := e.ts[ih]
	if !ok {
		t = &torrentState{Torrent: Torrent{InfoHash: ih}}
		e.ts[ih] = t
	}
	t.update(tt, now)
	return t
}

func (e *Engine) getLocked(infohash string) (*torrentState, error) {
	ih, err := str2ih(infohash)
	if err != nil {
		return nil, err
	}
	t, ok := e.ts[ih.HexString()]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrMissingTorrent, ih.HexString())
	}
	return t, nil
}

func (e *Engine) StartTorrent(infohash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.getLocked(infohash)
	if err != nil {
		return err
	}
	if t.Started {
		return ErrAlreadyStarted
	}
	return e.startLocked(t)
}

func (e *Engine) startLocked(t *torrentState) error {
	// Stopping drops the underlying torrent, so restarting means re-adding it.
	// Without this, start-after-stop flips the flag and downloads nothing.
	if t.t == nil {
		if e.client == nil {
			return ErrNotConfigured
		}
		if t.spec == nil {
			return fmt.Errorf("%w: cannot restart, original source is unknown", ErrMissingTorrent)
		}
		tt, _, err := e.client.AddTorrentSpec(t.spec)
		if err != nil {
			return fmt.Errorf("torrent error: %w", err)
		}
		t.t = tt
		// The re-added torrent needs its own watcher: stopping dropped the
		// metadata, and a magnet's retained spec carries no InfoBytes, so
		// Info() is nil here and the DownloadAll below is skipped. The watcher
		// from the original add is parked on the previous handle and
		// infoArrived's t.t != tt guard makes it a no-op, so without this a
		// restarted magnet stays flagged as running and downloads nothing.
		e.watchInfoLocked(t, tt)
	}
	t.Started = true
	if t.t.Info() != nil {
		t.t.DownloadAll()
	}
	return nil
}

func (e *Engine) StopTorrent(infohash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.getLocked(infohash)
	if err != nil {
		return err
	}
	if !t.Started {
		return ErrAlreadyStopped
	}
	// There is no pause in anacrolix/torrent — stopping drops the torrent. The
	// spec is retained so StartTorrent can re-add it.
	//
	// Drop does not close GotInfo, so the watcher has to be released here; the
	// re-add in startLocked parks a fresh one on the new handle.
	t.stopWatch()
	if t.t != nil {
		t.t.Drop()
		t.t = nil
	}
	t.Started = false
	return nil
}

func (e *Engine) DeleteTorrent(infohash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.getLocked(infohash)
	if err != nil {
		return err
	}
	t.stopWatch()
	if t.t != nil {
		t.t.Drop()
	}
	delete(e.ts, t.InfoHash)
	return nil
}

// str2ih parses a hex infohash. The length is checked before decoding:
// hex.Decode is bounded by len(src), not len(dst), so an over-long input writes
// past the end of the array and panics.
func str2ih(str string) (metainfo.Hash, error) {
	var ih metainfo.Hash
	if len(str) != infoHashHexLen {
		return ih, fmt.Errorf("%w: infohash must be %d hex chars, got %d", ErrInvalidInput, infoHashHexLen, len(str))
	}
	if _, err := hex.Decode(ih[:], []byte(str)); err != nil {
		return ih, fmt.Errorf("%w: infohash is not a hex string", ErrInvalidInput)
	}
	return ih, nil
}
