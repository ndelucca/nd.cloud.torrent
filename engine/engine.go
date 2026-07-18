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

// Sentinel errors. These strings are surfaced to the user by the server, so they
// double as UI copy — wrap them with %w rather than reformatting them.
var (
	ErrNotConfigured  = errors.New("Engine is not configured")
	ErrMissingTorrent = errors.New("Missing torrent")
	ErrAlreadyStarted = errors.New("Already started")
	ErrAlreadyStopped = errors.New("Already stopped")
	ErrClosed         = errors.New("Engine is closed")
)

// Engine wraps anacrolix/torrent in a server-friendly facade: one client, a
// mutable Config, and a cache of Torrent view models keyed by hex infohash.
//
// Every exported method is safe for concurrent use. All access to client, config
// and ts happens under mu.
type Engine struct {
	// configureMu serializes Configure end to end. mu alone is not enough: the
	// same-port path releases mu between dropping the old client and installing
	// the replacement, and a second caller entering that window saw
	// client == nil, took the non-retrying branch, and stole the port — leaving
	// the first caller to spin the whole rebind budget and fail, or worse, both
	// to succeed with the persisted config and the running engine disagreeing.
	configureMu sync.Mutex

	mu     sync.Mutex
	client *torrent.Client
	config Config
	ts     map[string]*Torrent

	// Lifecycle for the per-torrent metadata watchers.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New() *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		ts:     map[string]*Torrent{},
		ctx:    ctx,
		cancel: cancel,
	}
}

// Config returns a copy of the current configuration.
func (e *Engine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.config
}

// Configure replaces the underlying client. It validates and builds the
// replacement *before* tearing down the existing client, so a rejected config
// leaves the engine untouched and still running.
func (e *Engine) Configure(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	e.configureMu.Lock()
	defer e.configureMu.Unlock()

	// Checked under configureMu, which Close holds for its whole duration, so a
	// Close that has begun cannot be overtaken by a Configure that has not.
	if e.ctx.Err() != nil {
		return ErrClosed
	}

	tc := clientConfig(c)

	// Teardown order depends on whether the listen port is changing, so the
	// old client is detached first and the decision is made once.
	evicted := e.evictForRebind(c.IncomingPort)
	// e.ctx is cancelled only by Close. A rebind must never be cancellable by
	// the request that triggered it: aborting between evicting the old client
	// and installing the new one leaves the engine with none, so a user
	// navigating away mid-save would take their own torrent client down.
	client, err := buildClient(e.ctx, tc, evicted, c.IncomingPort)
	if err != nil {
		return err
	}

	replaced, rewatch := e.installClient(client, c)

	// Outside the lock: watchInfo takes mu itself when its metadata lands.
	for _, tt := range rewatch {
		e.watchInfo(tt)
	}

	// Only non-nil on the port-changed path; the same-port path already closed
	// and waited on the previous client. Closing last means a failed build
	// above never took the running client down with it.
	closeClient(replaced)
	return nil
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

// evictForRebind detaches the running client if the new config keeps its port,
// and returns it. A nil return means the caller may build the replacement while
// the current client keeps running.
//
// Different port: build the replacement first, so a failure leaves the running
// client untouched.
//
// Same port: the old client is bound to it, so a new one cannot be created
// until it lets go — building first fails with "address already in use" every
// single time. That is the common case, since any settings change that is not
// the port keeps the port.
func (e *Engine) evictForRebind(newPort int) *torrent.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client == nil || e.config.IncomingPort != newPort {
		return nil
	}
	evicted := e.client
	e.client = nil
	return evicted
}

// buildClient creates the replacement. If evicted is non-nil it is closed and
// waited on first, and the bind is retried.
//
// ctx is the engine's own, cancelled only by Close, so shutdown does not have
// to wait out a rebind that is retrying against a port the kernel has not
// released yet.
func buildClient(ctx context.Context, tc *torrent.ClientConfig, evicted *torrent.Client, port int) (*torrent.Client, error) {
	if evicted == nil {
		client, err := torrent.NewClient(tc)
		if err != nil {
			return nil, fmt.Errorf("Failed to start torrent client: %w", err)
		}
		return client, nil
	}

	evicted.Close()
	select {
	case <-evicted.Closed():
	case <-ctx.Done():
		return nil, ErrClosed
	}
	// Closed() only reports that the client finished shutting down; the kernel
	// can still hold the listening socket for a moment afterwards, so a bind
	// right after it intermittently fails with EADDRINUSE. This is what the
	// original code's fixed time.Sleep(1s) was papering over — retrying until
	// it binds is both faster in the common case and actually reliable, where a
	// fixed sleep is neither.
	client, err := newClientWithRetry(ctx, tc)
	if err != nil {
		if errors.Is(err, ErrClosed) {
			return nil, err // shutting down; not a configuration failure
		}
		// The old client is already gone, so there is nothing to fall back to.
		// Leaving e.client nil is honest: every operation now reports
		// ErrNotConfigured rather than acting on a dead client.
		return nil, fmt.Errorf("Failed to restart torrent client on port %d (the previous "+
			"client has been stopped): %w", port, err)
	}
	return client, nil
}

// installClient publishes the new client and config, and re-adds every cached
// torrent to it. It returns the client it displaced (nil on the same-port path,
// which already closed it) and the torrents needing a fresh metadata watcher.
func (e *Engine) installClient(client *torrent.Client, c Config) (*torrent.Client, []*torrent.Torrent) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replaced := e.client
	e.client = client
	e.config = c

	// Every cached handle points into the old client. Re-add the torrents we
	// know about so a settings change does not silently stop all downloads.
	var rewatch []*torrent.Torrent
	for _, t := range e.ts {
		t.t = nil
		if t.spec == nil {
			continue
		}
		tt, _, err := client.AddTorrentSpec(t.spec)
		if err != nil {
			log.Printf("engine: failed to re-add %s: %s", t.InfoHash, err)
			continue
		}
		t.t = tt
		switch {
		case t.Started && tt.Info() != nil:
			tt.DownloadAll()
		case tt.Info() == nil && c.AutoStart:
			// A magnet whose metadata had not arrived yet. Its original watcher
			// is parked on the OLD handle and will correctly decline to act once
			// it fires, so without a fresh watcher the torrent would never
			// auto-start — permanently, for the lifetime of the process.
			rewatch = append(rewatch, tt)
		}
	}
	return replaced, rewatch
}

// closeClient shuts a displaced client down, logging rather than returning its
// errors: the replacement is already live and nothing useful can be done here.
func closeClient(client *torrent.Client) {
	if client == nil {
		return
	}
	for _, err := range client.Close() {
		log.Printf("engine: error closing previous client: %s", err)
	}
}

// Close shuts the engine down and releases the underlying client. It is
// idempotent, and once it has run the engine stays closed: Configure reports
// ErrClosed rather than building a client nothing would ever release.
func (e *Engine) Close() error {
	// Signal first. An in-flight Configure may be seconds into a rebind, and
	// this is what lets it give up rather than making shutdown wait it out.
	e.cancel()

	// Then serialize against it. Without this lock, Close saw client == nil —
	// the rebind having already evicted it — returned clean, and Configure went
	// on to install a live torrent client, with its listening socket, goroutines
	// and DHT, into an engine everyone believed was shut down.
	e.configureMu.Lock()
	defer e.configureMu.Unlock()

	// Safe under configureMu: no Configure can be past its ErrClosed check, so
	// nothing will Add to the group after this returns.
	e.wg.Wait()

	e.mu.Lock()
	client := e.client
	e.client = nil
	e.mu.Unlock()

	if client == nil {
		return nil
	}
	return errors.Join(client.Close()...)
}

// NewMagnet adds a torrent from a magnet URI.
func (e *Engine) NewMagnet(magnetURI string) error {
	spec, err := torrent.TorrentSpecFromMagnetUri(magnetURI)
	if err != nil {
		return fmt.Errorf("Invalid magnet URI: %w", err)
	}
	return e.addSpec(spec)
}

// NewTorrentFile adds a torrent from raw .torrent file bytes. Keeping the
// parsing here means callers do not need to import anacrolix/torrent.
func (e *Engine) NewTorrentFile(data []byte) error {
	info, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("Invalid torrent file: %w", err)
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(info)
	if err != nil {
		return fmt.Errorf("Invalid torrent file: %w", err)
	}
	return e.addSpec(spec)
}

func (e *Engine) addSpec(spec *torrent.TorrentSpec) error {
	e.mu.Lock()
	if e.client == nil {
		e.mu.Unlock()
		return ErrNotConfigured
	}
	tt, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		e.mu.Unlock()
		return fmt.Errorf("Torrent error: %w", err)
	}
	t := e.upsertLocked(tt)
	t.spec = spec
	autoStart := e.config.AutoStart
	e.mu.Unlock()

	if autoStart {
		e.watchInfo(tt)
	}
	return nil
}

// watchInfo starts the torrent once its metadata resolves. The goroutine exits
// when the engine is closed, so an unresolvable magnet no longer leaks it.
func (e *Engine) watchInfo(tt *torrent.Torrent) {
	ih := tt.InfoHash().HexString()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		select {
		case <-e.ctx.Done():
			return
		case <-tt.GotInfo():
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		t, ok := e.ts[ih]
		// t.t != tt means the torrent was stopped, deleted or re-added under a
		// new client while we waited — do not resurrect it.
		if !ok || t.t != tt || t.Started {
			return
		}
		if err := e.startLocked(t); err != nil {
			log.Printf("engine: auto-start %s: %s", ih, err)
		}
	}()
}

// GetTorrents refreshes the cache from the client and returns a deep copy. The
// copy matters: the caller marshals this concurrently with engine mutations.
func (e *Engine) GetTorrents() map[string]*Torrent {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.client != nil {
		for _, tt := range e.client.Torrents() {
			e.upsertLocked(tt)
		}
	}
	out := make(map[string]*Torrent, len(e.ts))
	for ih, t := range e.ts {
		out[ih] = t.clone()
	}
	return out
}

func (e *Engine) upsertLocked(tt *torrent.Torrent) *Torrent {
	ih := tt.InfoHash().HexString()
	t, ok := e.ts[ih]
	if !ok {
		t = &Torrent{InfoHash: ih}
		e.ts[ih] = t
	}
	t.update(tt)
	return t
}

func (e *Engine) getLocked(infohash string) (*Torrent, error) {
	ih, err := str2ih(infohash)
	if err != nil {
		return nil, err
	}
	t, ok := e.ts[ih.HexString()]
	if !ok {
		return nil, fmt.Errorf("%w %s", ErrMissingTorrent, ih.HexString())
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

func (e *Engine) startLocked(t *Torrent) error {
	// Stopping drops the underlying torrent, so restarting means re-adding it.
	// Without this, start-after-stop flipped the flag and downloaded nothing.
	if t.t == nil {
		if e.client == nil {
			return ErrNotConfigured
		}
		if t.spec == nil {
			return fmt.Errorf("%w: cannot restart, original source is unknown", ErrMissingTorrent)
		}
		tt, _, err := e.client.AddTorrentSpec(t.spec)
		if err != nil {
			return fmt.Errorf("Torrent error: %w", err)
		}
		t.t = tt
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
	if t.t != nil {
		t.t.Drop()
	}
	delete(e.ts, t.InfoHash)
	return nil
}

// str2ih parses a hex infohash. The length is checked before decoding: hex.Decode
// is bounded by len(src), not len(dst), so an over-long input used to write past
// the end of the array and panic.
func str2ih(str string) (metainfo.Hash, error) {
	var ih metainfo.Hash
	if len(str) != infoHashHexLen {
		return ih, fmt.Errorf("Invalid infohash length (expected %d hex chars, got %d)", infoHashHexLen, len(str))
	}
	if _, err := hex.Decode(ih[:], []byte(str)); err != nil {
		return ih, fmt.Errorf("Invalid infohash: not a hex string")
	}
	return ih, nil
}

// rebindTimeout bounds how long Configure waits for a just-closed listen port
// to become bindable again.
const rebindTimeout = 5 * time.Second

// newClientWithRetry retries until the client starts or the budget runs out.
//
// Only used when replacing a client on the same port, where the one transient
// failure worth waiting out is the kernel not having released the listening
// socket yet. It retries on any error rather than matching EADDRINUSE, because
// that errno differs across the platforms this builds for (Windows reports
// WSAEADDRINUSE); the cost of being wrong is a few seconds before a genuinely
// bad config is reported, which is the better trade than a retry that silently
// does nothing on one platform.
func newClientWithRetry(ctx context.Context, tc *torrent.ClientConfig) (*torrent.Client, error) {
	deadline := time.Now().Add(rebindTimeout)
	delay := 20 * time.Millisecond
	for {
		client, err := torrent.NewClient(tc)
		if err == nil {
			return client, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ErrClosed
		case <-time.After(delay):
		}
		if delay < 200*time.Millisecond {
			delay *= 2
		}
	}
}
