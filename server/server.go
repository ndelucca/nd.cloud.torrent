package server

import (
	"bytes"
	"context"
	"crypto/tls"
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
	"time"

	"github.com/jpillora/cloud-torrent/engine"
	ctstatic "github.com/jpillora/cloud-torrent/static"
	"github.com/jpillora/cookieauth"
	"github.com/jpillora/requestlog"
	"github.com/jpillora/scraper/scraper"
	"github.com/jpillora/velox"
	"github.com/klauspost/compress/gzhttp"
)

const (
	defaultIncomingPort = 50007
	// pollInterval is how often torrent and download-tree state is refreshed.
	pollInterval = 1 * time.Second
	// statsInterval must stay fixed: cpu.Percent(0, …) reports usage since the
	// previous call, so the sampling period defines the window.
	statsInterval = 5 * time.Second
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
	// Opt-in: the document at this URL decides which hosts the server scrapes,
	// so it is off unless explicitly configured.
	SearchConfigURL string `help:"Optional URL to periodically fetch search providers from"`
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

	static      http.Handler
	syncHandler http.Handler
	scraper     *scraper.Handler
	scraperh    http.Handler

	// searchConfig is the last scraper config successfully applied; guarded by
	// state's lock via applySearchConfig.
	searchConfig []byte
}

// New builds a server from options. It performs no I/O beyond reading the config
// file, so it is safe to call in tests.
func New(o Options, version string) (*Server, error) {
	isTLS := o.CertPath != "" || o.KeyPath != ""
	if isTLS && (o.CertPath == "" || o.KeyPath == "") {
		return nil, errors.New("You must provide both key and cert paths")
	}

	s := &Server{opts: o, version: version, isTLS: isTLS}

	s.state.Stats = Stats{
		Title:   o.Title,
		Version: version,
		Runtime: strings.TrimPrefix(runtime.Version(), "go"),
		Uptime:  time.Now(),
	}

	s.static = ctstatic.FileSystemHandler()

	if err := s.setupScraper(); err != nil {
		return nil, err
	}

	// velox.SyncHandler initializes the *embedded* velox.State. velox.Sync does
	// not — it builds a detached State, which silently made every Push a no-op.
	s.syncHandler = velox.SyncHandler(&s.state)

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

func (s *Server) setupScraper() error {
	s.scraper = &scraper.Handler{
		Log: false, Debug: false,
		Headers: map[string]string{
			"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		},
	}
	// Pass a copy: the scraper's selector unmarshaler mutates the buffer it is
	// given, which would corrupt the embedded config for any later load.
	cfg := bytes.Clone(defaultSearchConfig)
	if err := s.scraper.LoadConfig(cfg); err != nil {
		return fmt.Errorf("failed to load search config: %w", err)
	}
	s.searchConfig = bytes.Clone(defaultSearchConfig)
	s.state.SearchProviders = s.scraper.Config
	// The scraper treats POST to its root as "replace my whole config", which is
	// an unauthenticated SSRF pivot. Only reads reach it.
	s.scraperh = readOnly(safeSearchParams(http.StripPrefix("/search", s.scraper)))
	return nil
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
	go s.fetchSearchConfigLoop(ctx)

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
		// No WriteTimeout: /sync is long-lived and downloads can be large.
		//disable http2 due to velox bug
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
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

func (s *Server) pollLoop(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		torrents := s.engine.GetTorrents()
		downloads := s.listFiles()
		conns := s.state.NumConnections()
		s.state.Update(func(st *State) {
			st.Torrents = torrents
			st.Downloads = downloads
			st.ConnectedUsers = conns
		})
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) statsLoop(ctx context.Context) {
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	for {
		sys := sampleSystemStats(s.engine.Config().DownloadDirectory)
		s.state.Update(func(st *State) { st.Stats.System = sys })
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
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
	h = gzhttp.GzipHandler(h)
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

// route dispatches by path prefix; order matters.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/js/velox.js":
		velox.JS.ServeHTTP(w, r)
	case r.URL.Path == "/sync":
		s.syncHandler.ServeHTTP(w, r)
	case r.URL.Path == "/search" || strings.HasPrefix(r.URL.Path, "/search/"):
		s.scraperh.ServeHTTP(w, r)
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

// readOnly rejects anything that could mutate the wrapped handler's state.
func readOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}
