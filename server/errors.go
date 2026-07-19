package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/fetch"
)

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
// The axis is not "engine error versus server error", it is: did what the
// caller sent cause this?
//
//   - Input — the magnet URI, the remote URL, the .torrent bytes, a config
//     value. The wrapped detail is the only useful information ("no info hash
//     in magnet link") and it is bounded parser prose, so it is shown.
//   - Operational — disk, bind, upstream, closed. The wrapped detail is a
//     syscall string and a filesystem-layout oracle, so a fixed message is
//     shown and the chain goes to the log.
//
// The default is 500, so an unclassified failure is never reported to the user
// as their own mistake.
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
