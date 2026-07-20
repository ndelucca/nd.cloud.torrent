package server

import (
	"log"
	"net/http"
	"strings"

	"github.com/ndelucca/nd.cloud.torrent/internal/reqlog"
	ctstatic "github.com/ndelucca/nd.cloud.torrent/static"

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
	// One handler, three mounts. Not a catch-all "/": that matches every
	// unrouted path, so ServeMux could never answer 405 and a GET /api/add
	// would reach the file server and 404.
	static := ctstatic.FileSystemHandler()
	mux.Handle("GET /css/", static)
	mux.Handle("GET /js/", static)
	mux.Handle("GET /cloud-favicon.png", static)
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
				// classify owns the status as well as the message; a literal
				// here would be a second place for the 403 to live.
				status, msg := classify(err)
				http.Error(w, msg, status)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// appCSP is the policy for the app's own pages.
//
// The load-bearing directive is script-src without 'unsafe-inline'. That is not
// about our own scripts — every one of them is a same-origin file — it is about
// a script an attacker gets into the page. This app renders torrent-supplied
// file names, and web/templates/downloads.html documents an injection sink that
// html/template's escaper cannot see (a name interpolated into x-data would be
// evaluated). The escaper is the primary defence; this is the second layer for
// when it is bypassed.
//
// There is no 'unsafe-eval' either. Alpine ships as its CSP build, which parses
// attribute expressions into an AST and interprets them rather than compiling
// them with the AsyncFunction constructor, and htmx's eval paths are off (ct.js
// sets allowEval false). Nothing left needs it.
//
// style-src 'self' works because no template carries a style attribute and htmx's
// indicator <style> injection is disabled in ct.js. x-show writes
// element.style.display as a DOM property, which CSP does not govern.
//
// Downloaded files get a different, stricter policy — see files.sandbox. This one
// must not be applied to them.
const appCSP = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self'; " +
	"media-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'self'; " +
	"frame-src 'none'; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'; " +
	"base-uri 'none'"

// securityHeaders applies defaults that cost nothing and close off sniffing,
// framing and script injection.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head := w.Header()
		head.Set("X-Content-Type-Options", "nosniff")
		// Kept alongside frame-ancestors, which supersedes it: the header is
		// what older browsers understand.
		head.Set("X-Frame-Options", "DENY")
		head.Set("Referrer-Policy", "no-referrer")
		// Downloaded content gets files.sandbox's stricter policy instead. This
		// middleware wraps the mux, so it runs first and files.Handler's later
		// Set overwrites this one — which is the intended order, not a
		// coincidence to leave unstated.
		head.Set("Content-Security-Policy", appCSP)
		h.ServeHTTP(w, r)
	})
}
