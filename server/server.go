// Package server owns the process shell and every HTTP surface: the shared
// state snapshot, server-side rendering of the web UI, the SSE stream that
// drives it, the /api/* command endpoints, and download file serving.
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

	"github.com/jpillora/cookieauth"
	"github.com/jpillora/requestlog"
	"github.com/klauspost/compress/gzhttp"
	"github.com/ndelucca/nd.cloud.torrent/engine"
	ctstatic "github.com/ndelucca/nd.cloud.torrent/static"
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
)

// Options is the CLI surface. jpillora/opts derives flags, help and env from
// these struct tags.
type Options struct {
	Title      string `help:"Title of this instance" env:"TITLE"`
	Port       int    `help:"Listening port" env:"PORT"`
	Host       string `help:"Listening interface (default all)"`
	Auth       string `help:"Optional basic auth in form 'user:password'" env:"AUTH"`
	ConfigPath string `help:"Configuration file path"`
	KeyPath    string `help:"TLS Key file path"`
	CertPath   string `help:"TLS Certicate file path" short:"r"`
	Log        bool   `help:"Enable request logging"`
	Open       bool   `help:"Open now with your default browser"`
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

// Server is the runtime: handlers, engine, and the synced state object.
type Server struct {
	opts    Options
	version string
	isTLS   bool

	engine *engine.Engine
	state  State

	renderer *renderer
	hub      *hub
	kickCh   chan struct{}
	// renderMu serializes rendering. pollLoop and statsLoop both render, and
	// without it two goroutines can broadcast samples in the opposite order to
	// the one they were taken in, leaving browsers on the older one.
	// seenTorrents is covered by it too.
	renderMu     sync.Mutex
	seenTorrents map[string]bool

	static http.Handler
}

// New builds a server from options. It performs no I/O beyond reading the config
// file, so it is safe to call in tests.
func New(o Options, version string) (*Server, error) {
	isTLS := o.CertPath != "" || o.KeyPath != ""
	if isTLS && (o.CertPath == "" || o.KeyPath == "") {
		return nil, errors.New("You must provide both key and cert paths")
	}

	s := &Server{
		opts:    o,
		version: version,
		isTLS:   isTLS,
		hub:     newHub(),
		// Buffered and coalesced: a burst of API calls between two ticks costs
		// one extra render, not one per call.
		kickCh: make(chan struct{}, 1),
	}

	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	s.renderer = newRenderer(tmpl)

	s.state.Stats = Stats{
		Title:   o.Title,
		Version: version,
		Runtime: strings.TrimPrefix(runtime.Version(), "go"),
		Uptime:  time.Now(),
	}

	s.static = ctstatic.FileSystemHandler()

	s.engine = engine.New()
	c, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	if err := s.reconfigure(c); err != nil {
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
		return c, fmt.Errorf("Read configuration error: %w", err)
	}
	if len(b) == 0 {
		return c, nil //ignore empty file
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("Malformed configuration: %w", err)
	}
	if c.IncomingPort <= 0 || c.IncomingPort > 65535 {
		c.IncomingPort = defaultIncomingPort
	}
	return c, nil
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go s.pollLoop(ctx)
	go s.statsLoop(ctx)

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
		shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		return srv.Shutdown(shutdownCtx)
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
		// listFiles walks the download directory with up to fileNumberLimit stat
		// calls, so with nobody connected it is pure waste.
		if s.watchers() > 0 {
			torrents := s.engine.GetTorrents()
			downloads := s.listFiles()
			conns := s.watchers()
			s.state.Update(func(st *State) {
				st.Torrents = torrents
				st.Downloads = downloads
				st.ConnectedUsers = conns
			})
			s.renderRegions()
			s.renderTorrents(torrents)
			s.renderDownloads(downloads)
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
		// Sampling is cheap but not free, and with nobody watching the result
		// is discarded — same reasoning as pollLoop.
		if s.watchers() > 0 {
			sys := sampleSystemStats(s.engine.Config().DownloadDirectory)
			s.state.Update(func(st *State) { st.Stats.System = sys })
			s.renderRegions()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// watchers counts connected browsers.
func (s *Server) watchers() int { return s.hub.count() }

// renderRegions renders every SSE region once and fans the same bytes out to
// every subscriber. Rendering is deliberately not per-client.
func (s *Server) renderRegions() {
	s.renderMu.Lock()
	defer s.renderMu.Unlock()

	var view statsView
	s.state.Read(func(st *State) {
		view = statsView{
			Title:          st.Stats.Title,
			Version:        st.Stats.Version,
			Runtime:        st.Stats.Runtime,
			Uptime:         st.Stats.Uptime,
			ConnectedUsers: st.ConnectedUsers,
			System:         st.Stats.System,
			MemPercent:     percentOf(st.Stats.System.MemoryUsed, st.Stats.System.MemoryTotal),
			DiskPercent:    percentOf(st.Stats.System.DiskUsed, st.Stats.System.DiskTotal),
			DiskFree:       st.Stats.System.DiskTotal - st.Stats.System.DiskUsed,
		}
	})
	frame, err := s.renderer.render("stats", "stats", view)
	if err != nil {
		log.Printf("render stats: %s", err)
		return
	}
	s.hub.broadcast(frame)
}

// statsView is the stats region's view model. Percentages are computed here
// rather than in the template: html/template has no arithmetic, and the
// AngularJS version doing `100*used/total` inline was a source of divide-by-zero
// producing +Inf.
type statsView struct {
	Title          string
	Version        string
	Runtime        string
	Uptime         time.Time
	ConnectedUsers int
	System         SystemStats
	MemPercent     float64
	DiskPercent    float64
	DiskFree       int64
}

// reconfigure applies a config to the engine and persists it. The engine restart
// happens first: if it fails, nothing is written and the old config stands.
func (s *Server) reconfigure(c engine.Config) error {
	dldir, err := filepath.Abs(c.DownloadDirectory)
	if err != nil {
		return fmt.Errorf("Invalid path: %w", err)
	}
	c.DownloadDirectory = dldir

	if err := s.engine.Configure(c); err != nil {
		return err
	}
	b, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return fmt.Errorf("Failed to encode configuration: %w", err)
	}
	// 0600: the file lives next to the binary and holds operational settings.
	if err := os.WriteFile(s.opts.ConfigPath, b, 0600); err != nil {
		return fmt.Errorf("Failed to save configuration: %w", err)
	}
	s.state.Update(func(st *State) { st.Config = c })
	return nil
}

// handler assembles the middleware chain, outermost first.
func (s *Server) handler() http.Handler {
	var h http.Handler = http.HandlerFunc(s.route)
	// gzhttp skips already-compressed content types, so /download/ no longer
	// burns CPU re-compressing media and zip archives.
	//
	// text/event-stream must be excluded outright. gzhttp buffers until
	// DefaultMinSize (1 KiB) before deciding whether to compress, and an SSE
	// frame is usually smaller than that — the first event would sit in the
	// buffer and never reach the browser. TestEventsArriveImmediately pins this.
	h = s.gzip(h)
	if s.opts.Auth != "" {
		user, pass, _ := strings.Cut(s.opts.Auth, ":")
		h = cookieauth.New().SetUserPass(user, pass).Wrap(h)
		log.Printf("Enabled HTTP authentication")
	}
	h = securityHeaders(h)
	if s.opts.Log {
		h = requestlog.Wrap(h)
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

// route dispatches by path prefix; order matters.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/events":
		s.serveEvents(w, r)
	case r.URL.Path == "/api/state":
		s.serveState(w, r)
	case r.URL.Path == "/":
		s.servePage(w, r)
	case strings.HasPrefix(r.URL.Path, "/fragments/"):
		s.serveFragment(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/"):
		s.serveAPI(w, r)
	case strings.HasPrefix(r.URL.Path, "/download/"):
		s.serveDownload(w, r)
	default:
		s.static.ServeHTTP(w, r)
	}
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
