package server

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/fetch"
)

// maxAPIBody caps request bodies. A .torrent file is comfortably under this.
const maxAPIBody = 4 << 20 // 4 MiB

// apiError carries an HTTP status alongside the message shown to the user.
type apiError struct {
	status int
	err    error
}

func (e apiError) Error() string { return e.err.Error() }
func (e apiError) Unwrap() error { return e.err }

func badRequest(format string, a ...any) error {
	return apiError{http.StatusBadRequest, fmt.Errorf(format, a...)}
}

// classify maps an error onto the HTTP status and the message the user sees.
//
// The axis is not "engine error versus server error", it is: **did what the
// caller sent cause this?**
//
//   - Input errors — the magnet URI, the remote URL, the .torrent bytes, a
//     config value. Here the wrapped detail is the only useful information
//     ("no info hash in magnet link") and it is bounded prose from a parser, so
//     it is shown.
//   - Operational errors — disk, bind, upstream, closed. Here the wrapped
//     detail is a syscall string and a filesystem-layout oracle, so a fixed
//     message is shown and the chain goes to the log.
//
// The default is 500. It used to be 400, which meant a disk-full or permission
// failure was reported to the user as their own mistake — exactly what the
// function existed to prevent.
func classify(err error) (int, string) {
	var ae apiError
	if errors.As(err, &ae) {
		return ae.status, sentence(ae.Error())
	}
	switch {
	// Caused by the request.
	case errors.Is(err, engine.ErrInvalidInput),
		errors.Is(err, fetch.ErrInvalidURL),
		errors.Is(err, fetch.ErrBlocked):
		return http.StatusBadRequest, sentence(err.Error())
	case errors.Is(err, engine.ErrMissingTorrent):
		return http.StatusNotFound, sentence(err.Error())
	case errors.Is(err, engine.ErrAlreadyStarted), errors.Is(err, engine.ErrAlreadyStopped):
		return http.StatusConflict, sentence(err.Error())

	// Not caused by the request, but the state is worth naming.
	case errors.Is(err, engine.ErrNotConfigured), errors.Is(err, engine.ErrClosed):
		return http.StatusServiceUnavailable, sentence(err.Error())

	// Not caused by the request, and the detail is not the user's business.
	case errors.Is(err, fetch.ErrUpstream):
		return http.StatusBadGateway, "Could not fetch the remote torrent."
	default:
		return http.StatusInternalServerError, "Something went wrong. See the server log for details."
	}
}

// sentence capitalises a message for display. Error strings are lowercase and
// unpunctuated per Go convention, because they get wrapped; the server owns
// presentation, which is what lets them stay conventional.
func sentence(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	out := string(r)
	if !strings.HasSuffix(out, ".") && !strings.HasSuffix(out, "!") && !strings.HasSuffix(out, "?") {
		out += "."
	}
	return out
}

// finishAPI turns a handler's error into a response: it wakes the render loop,
// maps the error and picks the representation.
func (s *Server) finishAPI(w http.ResponseWriter, r *http.Request, err error) {
	// A mutation almost always changes what the UI shows; waking the render loop
	// makes the effect visible immediately rather than up to a tick later.
	//
	// Unconditional, including on failure: an action can apply partially and
	// still report an error. Uploading five torrents where two are malformed
	// returns 400, but the three that were added are already in the engine — and
	// gating the kick on success left them invisible until the next tick. kick
	// is coalesced and floored, so the cost of being wrong here is at most one
	// extra render.
	s.kick()

	status, msg := http.StatusOK, ""
	if err != nil {
		status, msg = classify(err)
		// The user gets a fixed message for anything they did not cause, so the
		// chain is only recoverable from here.
		if status >= http.StatusInternalServerError {
			log.Printf("api %s: %s", r.URL.Path, err)
		}
	}

	// htmx wants HTML to swap. It also does not swap non-2xx responses by
	// default, so the outcome is reported as a 200 fragment; the status codes
	// stay intact for every other client.
	if r.Header.Get("HX-Request") == "true" {
		s.ui.WriteAPIResult(w, msg)
		return
	}

	if err != nil {
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

// readBody drains a request body under the size cap.
func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBody))
	if err != nil {
		return nil, badRequest("failed to read request body")
	}
	return data, nil
}

// handleAdd takes a magnet link or an http(s) URL to a .torrent. One field; the
// server dispatches on the scheme, so the client needs no parsing rules of its
// own.
func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()
	// The omni form posts form-encoded; scripts post the bare string.
	if isForm(r) {
		if err := r.ParseForm(); err != nil {
			return badRequest("malformed form body")
		}
		return s.addURI(r, strings.TrimSpace(r.PostFormValue("uri")))
	}
	data, err := readBody(r)
	if err != nil {
		return err
	}
	return s.addURI(r, strings.TrimSpace(string(data)))
}

// handleTorrentFile takes raw .torrent bytes, or a multipart upload from the
// browser.
func (s *Server) handleTorrentFile(w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()
	// Multipart is checked before the body is drained: it is the only encoding
	// that must be parsed by the stdlib rather than read whole.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return s.addUploadedTorrents(w, r)
	}
	data, err := readBody(r)
	if err != nil {
		return err
	}
	return s.engine.NewTorrentFile(data)
}

// handleConfigure applies the settings form.
func (s *Server) handleConfigure(w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()
	if !isForm(r) {
		return badRequest("expected a form-encoded configuration")
	}
	if err := r.ParseForm(); err != nil {
		return badRequest("malformed form body")
	}
	// Read, merge and apply under one lock. engine.configureMu serializes the
	// apply, but not the read the merge is based on, so two concurrent saves
	// could each start from the same config and the second would silently undo
	// the first.
	s.configMu.Lock()
	defer s.configMu.Unlock()
	c, err := parseConfig(r.PostForm, s.engine.Config())
	if err != nil {
		return err
	}
	return s.reconfigure(c)
}

// isForm reports whether the body is url-encoded form data.
func isForm(r *http.Request) bool {
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	return strings.TrimSpace(ct) == "application/x-www-form-urlencoded"
}

// apiHandler is what an /api/* route does. The signature takes the
// ResponseWriter because the multipart path genuinely needs it, for
// http.MaxBytesReader — the doc used to claim handlers took only the request,
// which had already drifted from the code.
type apiHandler func(http.ResponseWriter, *http.Request) error

// apiRoute adapts an apiHandler into an http.Handler, applying the kick, the
// htmx fragment rendering and the status mapping in exactly one place.
func (s *Server) apiRoute(h apiHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.finishAPI(w, r, h(w, r))
	})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) error {
	return s.engine.StartTorrent(r.PathValue("hash"))
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) error {
	return s.engine.StopTorrent(r.PathValue("hash"))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) error {
	return s.engine.DeleteTorrent(r.PathValue("hash"))
}

// checkSameOrigin rejects cross-site writes. Requests with no Origin (curl, the
// CLI) are allowed; browsers always send one on cross-origin POSTs.
func checkSameOrigin(r *http.Request) error {
	rejected := apiError{http.StatusForbidden, errors.New("cross-origin request rejected")}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		if site == "same-origin" || site == "none" {
			return nil
		}
		return rejected
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host != r.Host {
		return rejected
	}
	return nil
}
