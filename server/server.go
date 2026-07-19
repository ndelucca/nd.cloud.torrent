// Package server owns the process shell and the HTTP surface: flags, config, the
// middleware chain, the route dispatcher, the /api/* command endpoints and the
// background loops. Rendering, file serving and the remote fetch are delegated
// to the web, files and fetch packages.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/gzhttp"
	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/files"
	"github.com/ndelucca/nd.cloud.torrent/internal/auth"
	"github.com/ndelucca/nd.cloud.torrent/internal/reqlog"
	ctstatic "github.com/ndelucca/nd.cloud.torrent/static"
	"github.com/ndelucca/nd.cloud.torrent/sysstat"
	"github.com/ndelucca/nd.cloud.torrent/web"
)

const (
	defaultIncomingPort = 50007
	// pollInterval is how often torrent and download-tree state is refreshed.
	pollInterval = 1 * time.Second
	// statsInterval must stay fixed: cpu.Percent(0, …) reports usage since the
	// previous call, so the sampling period defines the window.
	statsInterval = 5 * time.Second
	// kickFloor rate-limits event-driven renders so a burst of API calls cannot
	// spin the render loop.
	kickFloor = 200 * time.Millisecond
	// shutdownTimeout bounds the graceful drain. SSE streams are released before
	// it starts, so what remains is real transfers: a large /download/ read, or
	// a zip still streaming.
	shutdownTimeout = 10 * time.Second
)

// Options is the CLI surface. The flags, shorthands and environment variables
// that fill it are registered in main, not derived from tags here.
type Options struct {
	Title      string
	Port       int
	Host       string // empty means every interface
	Auth       string // "user:password"; empty disables authentication
	ConfigPath string
	KeyPath    string
	CertPath   string
	Log        bool
	Open       bool
}

// DefaultOptions returns the shipped defaults. Keeping them here rather than in
// main means a zero-configuration server.Options is never silently wrong.
func DefaultOptions() Options {
	return Options{
		Title:      "Cloud Torrent",
		Port:       3000,
		ConfigPath: "cloud-torrent.json",
	}
}

// Server is the runtime: handlers, engine, and the sampled host stats.
type Server struct {
	opts    Options
	version string
	isTLS   bool
	// Fixed for the process lifetime, so they need no synchronization.
	goRuntime string
	startedAt time.Time

	engine *engine.Engine
	stats  sampledStats

	ui     *web.UI
	kickCh chan struct{}

	// configMu serializes read-merge-apply on /api/configure. The engine's own
	// lock covers the apply but not the read the merge starts from, so without
	// this two concurrent saves each begin from the same config and the second
	// silently undoes the first.
	configMu sync.Mutex

	static http.Handler
}

// downloadDir reads the live directory from the engine, which owns the config.
// /api/configure can move it at any time, which is why the files handler holds
// this func rather than a string.
func (s *Server) downloadDir() string {
	return s.engine.Config().DownloadDirectory
}

// New builds a server from options, reads the config file and applies it to the
// engine — which binds the incoming torrent port. It writes nothing.
func New(o Options, version string) (*Server, error) {
	isTLS := o.CertPath != "" || o.KeyPath != ""
	if isTLS && (o.CertPath == "" || o.KeyPath == "") {
		return nil, errors.New("you must provide both key and cert paths")
	}

	s := &Server{
		opts:      o,
		version:   version,
		isTLS:     isTLS,
		goRuntime: strings.TrimPrefix(runtime.Version(), "go"),
		startedAt: time.Now(),
		// Buffered and coalesced: a burst of API calls between two ticks costs
		// one extra render, not one per call.
		kickCh: make(chan struct{}, 1),
	}

	s.engine = engine.New()

	ui, err := web.New(web.Deps{
		Title:    o.Title,
		Version:  version,
		Runtime:  s.goRuntime,
		Uptime:   s.startedAt,
		Torrents: s.engine.GetTorrents,
		Tree:     func() *files.Node { return files.List(s.downloadDir()) },
		Config:   s.engine.Config,
		Kick:     s.kick,
	})
	if err != nil {
		return nil, err
	}
	s.ui = ui
	s.static = ctstatic.FileSystemHandler()

	c, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	// applyConfig, not reconfigure: startup applies but never writes. Rewriting
	// the config on every boot is a chance to corrupt it that buys nothing.
	if _, err := s.applyConfig(c); err != nil {
		return nil, fmt.Errorf("initial configure failed: %w", err)
	}
	return s, nil
}

func (s *Server) loadConfig() (engine.Config, error) {
	c := engine.Config{
		DownloadDirectory: "./downloads",
		EnableUpload:      true,
		AutoStart:         true,
		IncomingPort:      defaultIncomingPort,
	}
	b, err := os.ReadFile(s.opts.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read configuration error: %w", err)
	}
	if len(b) == 0 {
		return c, nil //ignore empty file
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("malformed configuration: %w", err)
	}
	// The port is deliberately not clamped. c starts from the defaults above, so
	// an absent IncomingPort already keeps defaultIncomingPort — a clamp could
	// only fire on a value someone explicitly wrote, and silently rewriting that
	// is worse than reporting it. Port validity is engine.Config.Validate's call
	// and nowhere else; two policies for one rule end up disagreeing.
	return c, nil
}

// Run serves until ctx is cancelled, then shuts down gracefully. It is
// one-shot: the hub latches closed, so a second call would serve no events.
//
// It returns nil for any completed shutdown, including one that overran its
// drain budget — Ctrl-C is a requested action, and a slow-draining download is
// not a failed run. A non-nil error means serving genuinely failed (the port
// could not be bound, TLS could not start), which is what main exits 1 on.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	// Registered before cancel so it runs after it — defers are LIFO, and
	// waiting before cancelling would deadlock. Joining the loops means no
	// engine call is in flight when main's deferred Close releases the engine.
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	// Covers every return path, including a failed Listen below. Idempotent.
	defer s.ui.Close()

	wg.Add(2)
	go func() { defer wg.Done(); s.pollLoop(ctx) }()
	go func() { defer wg.Done(); s.statsLoop(ctx) }()

	host := s.opts.Host
	if host == "" {
		host = "0.0.0.0"
	}
	addr := net.JoinHostPort(host, fmt.Sprint(s.opts.Port))
	proto := "http"
	if s.isTLS {
		proto += "s"
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: s.handler(),
		// Without these a single idle connection can hold a goroutine forever.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: /events is long-lived and downloads can be large.
	}

	// Bind before announcing, so the logged URL is only printed once it is real.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	log.Printf("Listening at %s://%s", proto, addr)

	if s.opts.Open {
		openhost := host
		if openhost == "0.0.0.0" {
			openhost = "localhost"
		}
		url := fmt.Sprintf("%s://%s:%d", proto, openhost, s.opts.Port)
		go func() {
			if err := openBrowser(url); err != nil {
				log.Printf("failed to open browser: %s", err)
			}
		}()
	}

	errc := make(chan error, 1)
	go func() {
		if s.isTLS {
			errc <- srv.ServeTLS(ln, s.opts.CertPath, s.opts.KeyPath)
		} else {
			errc <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Printf("Shutting down…")

		// Release the SSE streams BEFORE Shutdown. Shutdown waits for
		// connections to become idle and does not cancel request contexts, so a
		// long-lived /events handler is never released by it — one connected
		// browser burns the entire drain budget.
		s.ui.Close()

		shutdownCtx, done := context.WithTimeout(context.Background(), shutdownTimeout)
		defer done()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Only real transfers can reach this now — a large download or a zip
			// still streaming. Stop waiting on them.
			log.Printf("graceful shutdown exceeded %s, closing remaining connections: %s",
				shutdownTimeout, err)
			_ = srv.Close()
		}
		return nil
	}
}

// Close releases the engine. Safe to call once Run has returned.
func (s *Server) Close() error {
	return s.engine.Close()
}

// kick asks the render loop to run before its next tick. Without it, pressing
// Start would take up to a full pollInterval to show any effect.
func (s *Server) kick() {
	select {
	case s.kickCh <- struct{}{}:
	default: // a render is already pending; coalesce
	}
}

func (s *Server) pollLoop(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		// Gated on watchers because files.List walks the download directory with
		// up to files.Limit stat calls, and rendering for nobody is waste.
		//
		// Torrent *freshness* does not ride on this gate: the engine samples on
		// its own cadence, so GetTorrents here is a pure read of the latest
		// sample. When reads sampled, this gate silently doubled as the sampling
		// schedule — with nobody connected nothing sampled, and the first
		// reading after a browser connected computed its rate over however long
		// that was.
		if s.watchers() > 0 {
			s.renderStats()
			s.ui.RenderTorrents(s.engine.GetTorrents())
			s.ui.RenderDownloads(files.List(s.downloadDir()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kickCh:
			// Floor the rate so a burst of API calls cannot spin this loop.
			select {
			case <-ctx.Done():
				return
			case <-time.After(kickFloor):
			}
		}
	}
}

func (s *Server) statsLoop(ctx context.Context) {
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	for {
		// Sample unconditionally, render only for an audience. cpu.Percent
		// measures since the previous call anywhere in the process, so the
		// interval *is* the measurement window — gating the sample on watchers
		// would make the first reading after an idle spell an average over
		// however long nobody was watching, reported as trustworthy. The sample
		// is one syscall and a ReadMemStats; rendering is what is worth skipping.
		s.stats.set(sysstat.Sample(s.downloadDir()))
		if s.watchers() > 0 {
			s.renderStats()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// watchers counts connected browsers.
func (s *Server) watchers() int { return s.ui.Watchers() }

// renderStats hands the latest sample to the UI.
func (s *Server) renderStats() {
	s.ui.RenderStats(web.StatsData{
		System:         s.stats.get(),
		ConnectedUsers: s.watchers(),
	})
}

// reconfigure applies a config to the engine and persists it. The engine restart
// happens first: if it fails, nothing is written and the old config stands.
func (s *Server) reconfigure(c engine.Config) error {
	c, err := s.applyConfig(c)
	if err != nil {
		return err
	}
	return s.saveConfig(c)
}

// applyConfig absolutizes the download directory and hands the config to the
// engine, returning what was actually applied. It writes nothing: startup uses
// it alone, so a run that changes no settings leaves the config file untouched.
func (s *Server) applyConfig(c engine.Config) (engine.Config, error) {
	dldir, err := filepath.Abs(c.DownloadDirectory)
	if err != nil {
		return c, fmt.Errorf("invalid path: %w", err)
	}
	c.DownloadDirectory = dldir
	if err := s.engine.Configure(c); err != nil {
		return c, err
	}
	return c, nil
}

// saveConfig persists a config atomically: the file is either the old one or
// the new one, never a fragment. An interrupted write-in-place leaves a
// truncated file that loadConfig rejects as malformed, and the server then
// refuses to start until someone deletes it by hand.
func (s *Server) saveConfig(c engine.Config) error {
	b, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode configuration: %w", err)
	}
	path := s.opts.ConfigPath
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("failed to save configuration: %w", err)
		}
	}
	// Same directory as the target: rename is only atomic within a filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cloud-torrent-*.json")
	if err != nil {
		return fmt.Errorf("failed to save configuration: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds
	// 0600: the file lives next to the binary and holds operational settings.
	// CreateTemp already makes it 0600, but say so rather than rely on it.
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to save configuration: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to save configuration: %w", err)
	}
	// Sync before rename: without it the rename can land before the bytes do,
	// which is the same truncated file this function exists to prevent.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to save configuration: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to save configuration: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("failed to save configuration: %w", err)
	}
	return nil
}

// routes declares the HTTP surface.
//
// Pattern order is irrelevant — ServeMux matches most-specific-wins. "/{$}" is
// the exact root, GET patterns also match HEAD, and a wrong method on a declared
// path yields 405 with an Allow header rather than needing a guard in the
// handler.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", s.ui.ServeEvents)
	mux.HandleFunc("GET /api/state", s.serveState)
	mux.HandleFunc("GET /{$}", s.ui.ServePage)
	mux.HandleFunc("GET /fragments/downloads", s.ui.ServeDownloads)
	mux.HandleFunc("GET /fragments/torrent/{hash}/files", s.ui.ServeTorrentFiles)
	mux.Handle("POST /api/add", s.apiRoute(s.handleAdd))
	mux.Handle("POST /api/torrentfile", s.apiRoute(s.handleTorrentFile))
	mux.Handle("POST /api/configure", s.apiRoute(s.handleConfigure))
	// Each torrent verb is its own route, so the hash is a path parameter and
	// none of them takes a body.
	mux.Handle("POST /api/torrents/{hash}/start", s.apiRoute(s.handleStart))
	mux.Handle("POST /api/torrents/{hash}/stop", s.apiRoute(s.handleStop))
	mux.Handle("DELETE /api/torrents/{hash}", s.apiRoute(s.handleDelete))
	// StripPrefix because files.Handler reads the request path as relative to
	// the download root.
	mux.Handle("/download/", http.StripPrefix("/download/",
		&files.Handler{Root: s.downloadDir}))
	// Mounted at the three prefixes the assets occupy, not behind a catch-all
	// "/": a catch-all matches every unrouted path, so ServeMux could never
	// answer 405 — a GET to /api/add would reach the file server and 404. The
	// cost is a line here when a new asset directory appears.
	mux.Handle("GET /css/", s.static)
	mux.Handle("GET /js/", s.static)
	mux.Handle("GET /cloud-favicon.png", s.static)
	return mux
}

// handler assembles the middleware chain, outermost first.
func (s *Server) handler() http.Handler {
	h := requireSameOrigin(s.routes())
	// gzhttp skips already-compressed content types, so /download/ does not burn
	// CPU re-compressing media and zip archives.
	//
	// text/event-stream must be excluded outright: gzhttp buffers until 1 KiB
	// before deciding whether to compress, and an SSE frame is usually smaller,
	// so the first event would sit in the buffer and never reach the browser.
	h = s.gzip(h)
	if s.opts.Auth != "" {
		user, pass, _ := strings.Cut(s.opts.Auth, ":")
		// isTLS decides the Secure attribute: setting it without TLS would stop
		// the browser ever sending the cookie back.
		h = auth.Wrap(h, user, pass, s.isTLS)
		log.Printf("Enabled HTTP authentication")
	}
	h = securityHeaders(h)
	if s.opts.Log {
		h = reqlog.Wrap(h)
	}
	return h
}

// gzip wraps h, compressing everything except the SSE stream.
func (s *Server) gzip(h http.Handler) http.Handler {
	wrapper, err := gzhttp.NewWrapper(gzhttp.ExceptContentTypes([]string{"text/event-stream"}))
	if err != nil {
		// Only reachable from a bad option constant, i.e. a programming error.
		log.Printf("gzip wrapper: %s; serving uncompressed", err)
		return h
	}
	return wrapper(h)
}

// requireSameOrigin rejects cross-site writes by method, so it covers every
// mutating route including ones not yet written. GET and HEAD are exempt: they
// are not writes, and /download/ links must work from anywhere.
func requireSameOrigin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if err := checkSameOrigin(r); err != nil {
				_, msg := classify(err)
				http.Error(w, msg, http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// securityHeaders applies defaults that cost nothing and close off sniffing and
// framing.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head := w.Header()
		head.Set("X-Content-Type-Options", "nosniff")
		head.Set("X-Frame-Options", "DENY")
		head.Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}
