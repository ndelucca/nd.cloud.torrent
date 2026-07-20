// Package server owns the process shell and the HTTP surface: flags, config, the
// middleware chain, the route dispatcher, the /api/* command endpoints and the
// background loops. Rendering, file serving and the remote fetch are delegated
// to the web, files and fetch packages.
package server

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/configfile"
	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/files"
	"github.com/ndelucca/nd.cloud.torrent/web"
)

const (
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
	opts  Options
	isTLS bool
	// meta is what this instance says about itself. One struct rather than
	// loose fields because it has two consumers — the rendered page and
	// /api/state — and declaring it twice is how they come to disagree about
	// the version running, which is the situation /api/state exists to debug.
	//
	// Fixed for the process lifetime, so it needs no synchronization.
	meta instanceMeta

	engine *engine.Engine
	stats  sampledStats

	ui *web.UI
	// render owns when the UI is redrawn; see renderLoop.
	render *renderLoop

	// configMu guards desired and serializes read-merge-apply-persist on
	// /api/configure. The engine's own lock covers the apply but not the read
	// the merge starts from, so without this two concurrent saves each begin
	// from the same config and the second silently undoes the first.
	configMu sync.Mutex
	// desired is the configuration the user has asked for, which is what the
	// file holds and what the settings form renders.
	//
	// It is deliberately NOT the same thing as engine.Config(), which is what is
	// actually running. Most settings are fixed for the lifetime of a torrent
	// client, so after saving one the two legitimately differ until a restart —
	// they are two different facts, not two copies of one. Merging a form over
	// the *live* config instead would drop every pending change on the next
	// save, and the form would render the old value back at the user.
	desired engine.Config
}

// instanceMeta is what the server reports about itself: who it is, what build,
// and since when.
type instanceMeta struct {
	Title     string
	Version   string
	GoRuntime string
	StartedAt time.Time
}

// downloadDir reads the live directory from the engine, which owns the config.
// /api/configure can move it at any time, which is why the files handler holds
// this func rather than a string.
func (s *Server) downloadDir() string {
	return s.engine.Config().DownloadDirectory
}

// New builds a server from options, reads the config file and applies it to the
// engine — which binds the incoming torrent port. It writes nothing.
func New(o Options, version string) (_ *Server, retErr error) {
	isTLS := o.CertPath != "" || o.KeyPath != ""
	if isTLS && (o.CertPath == "" || o.KeyPath == "") {
		return nil, errors.New("you must provide both key and cert paths")
	}
	// Checked here, with the TLS pair, because auth.Wrap treats empty
	// credentials as "no authentication" and returns the handler untouched. So
	// --auth ":" — a typo, or an environment variable that expanded to just the
	// separator — used to pass the "is it set?" test in routes(), disable
	// authentication, and log that it had been enabled. A half-configured
	// security option is a startup failure, not a degraded mode.
	if o.Auth != "" {
		user, pass, found := strings.Cut(o.Auth, ":")
		if !found || user == "" || pass == "" {
			return nil, errors.New("--auth must be user:password, with both parts non-empty")
		}
	}

	s := &Server{
		opts:  o,
		isTLS: isTLS,
		meta: instanceMeta{
			Title:     o.Title,
			Version:   version,
			GoRuntime: strings.TrimPrefix(runtime.Version(), "go"),
			StartedAt: time.Now(),
		},
	}

	s.engine = engine.New()
	// engine.New starts the sampler goroutine, and only Close releases it, so
	// every failure below has to hand it back — by the time applyConfig runs
	// there may also be a torrent client holding a bound TCP and UDP port.
	// main exits on the error and never notices; the tests do not.
	defer func() {
		if retErr != nil {
			_ = s.engine.Close()
		}
	}()

	ui, err := web.New(web.Deps{
		Title:    s.meta.Title,
		Version:  s.meta.Version,
		Runtime:  s.meta.GoRuntime,
		Uptime:   s.meta.StartedAt,
		Torrents: s.engine.GetTorrents,
		Tree:     func() *files.Node { return files.List(s.downloadDir()) },
		Config:   s.desiredConfig,
		TorrentFiles: func(hash string) (*engine.TorrentWithFiles, bool) {
			t, err := s.engine.TorrentWithFiles(hash)
			return t, err == nil
		},
		Kick: s.kick,
	})
	if err != nil {
		return nil, err
	}
	s.ui = ui
	s.render = newRenderLoop(ui, s.engine, s.downloadDir, &s.stats)

	c, err := configfile.Load(s.opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	// applyConfig, not reconfigure: startup applies but never writes. Rewriting
	// the config on every boot is a chance to corrupt it that buys nothing.
	applied, err := s.applyConfig(c)
	if err != nil {
		return nil, fmt.Errorf("initial configure failed: %w", err)
	}
	// No lock: New has not returned, so nothing else can reach this yet.
	s.desired = applied
	return s, nil
}

// kick and watchers delegate to the render loop. They stay on Server because
// the API handlers and /api/state reach for them and have no other business
// with the schedule.
func (s *Server) kick()         { s.render.kick() }
func (s *Server) watchers() int { return s.render.watchers() }

// Close releases the engine. Safe to call once Run has returned.
func (s *Server) Close() error {
	return s.engine.Close()
}
