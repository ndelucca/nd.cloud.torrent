package server

import (
	"log"
	"net/http"
	"strings"

	"github.com/ndelucca/nd.cloud.torrent/internal/reqlog"

	"github.com/klauspost/compress/gzhttp"
	"github.com/ndelucca/nd.cloud.torrent/files"
	"github.com/ndelucca/nd.cloud.torrent/internal/auth"
)

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
	// Deleting a download goes through apiRoute like every other mutation, so it
	// gets the render kick, classify's status mapping and an api-ok/api-error
	// fragment for htmx. Answering it from files.Handler instead meant a 200
	// with an empty body on success — which htmx swapped, blanking the panel —
	// and a 500 on failure, which htmx does not swap at all, so a failed delete
	// reported nothing.
	//
	// Authorization is unchanged: requireSameOrigin wraps the whole mux by
	// method, so this was gated before it was its own route and still is.
	//
	// Most-specific-wins, and a method-bearing pattern beats a method-less
	// prefix, so GET and HEAD still reach files.Handler below.
	mux.Handle("DELETE /download/{path...}", s.apiRoute(s.handleDeleteFile))
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
