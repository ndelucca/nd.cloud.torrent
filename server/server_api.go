package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jpillora/cloud-torrent/engine"
)

const (
	// maxAPIBody caps request bodies. A .torrent file is comfortably under this.
	maxAPIBody = 4 << 20 // 4 MiB
	// maxRemoteTorrent caps what /api/url will pull from a third party.
	maxRemoteTorrent = 4 << 20
	// remoteFetchTimeout bounds the server-side fetch in /api/url.
	remoteFetchTimeout = 15 * time.Second
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

// statusFor maps an engine error onto an HTTP status, so callers can stop
// treating disk and network failures as client errors.
func statusFor(err error) int {
	var ae apiError
	if errors.As(err, &ae) {
		return ae.status
	}
	switch {
	case errors.Is(err, engine.ErrMissingTorrent), errors.Is(err, engine.ErrMissingFile):
		return http.StatusNotFound
	case errors.Is(err, engine.ErrAlreadyStarted), errors.Is(err, engine.ErrAlreadyStopped):
		return http.StatusConflict
	case errors.Is(err, engine.ErrUnsupported):
		return http.StatusNotImplemented
	case errors.Is(err, engine.ErrNotConfigured):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := s.api(r); err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

func (s *Server) api(r *http.Request) error {
	defer r.Body.Close()
	if r.Method != http.MethodPost {
		return apiError{http.StatusMethodNotAllowed, errors.New("Invalid request method (expecting POST)")}
	}
	// The API accepts text/plain bodies, which browsers send cross-origin without
	// a preflight. Without this check any page could reconfigure the server.
	if err := checkSameOrigin(r); err != nil {
		return err
	}

	action := strings.TrimPrefix(r.URL.Path, "/api/")
	data, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBody))
	if err != nil {
		return badRequest("Failed to read request body")
	}

	switch action {
	case "url":
		body, err := fetchRemoteTorrent(r.Context(), string(data))
		if err != nil {
			return err
		}
		return s.engine.NewTorrentFile(body)
	case "torrentfile":
		return s.engine.NewTorrentFile(data)
	case "magnet":
		return s.engine.NewMagnet(string(data))
	case "configure":
		c := engine.Config{}
		if err := json.Unmarshal(data, &c); err != nil {
			return badRequest("Malformed configuration: %s", err)
		}
		return s.reconfigure(c)
	case "torrent":
		var state, infohash string
		if v := formValues(r, data); v != nil {
			state, infohash = v.Get("action"), v.Get("infohash")
			if state == "" || infohash == "" {
				return badRequest("Invalid request")
			}
		} else {
			var ok bool
			state, infohash, ok = strings.Cut(string(data), ":")
			if !ok {
				return badRequest("Invalid request")
			}
		}
		switch state {
		case "start":
			return s.engine.StartTorrent(infohash)
		case "stop":
			return s.engine.StopTorrent(infohash)
		case "delete":
			return s.engine.DeleteTorrent(infohash)
		default:
			return badRequest("Invalid state: %s", state)
		}
	case "file":
		parts := strings.SplitN(string(data), ":", 3)
		if len(parts) != 3 {
			return badRequest("Invalid request")
		}
		state, infohash, path := parts[0], parts[1], parts[2]
		switch state {
		case "start":
			return s.engine.StartFile(infohash, path)
		case "stop":
			return s.engine.StopFile(infohash, path)
		default:
			return badRequest("Invalid state: %s", state)
		}
	default:
		return apiError{http.StatusNotFound, fmt.Errorf("Invalid action: %s", action)}
	}
}

// formValues parses a form-encoded body, or returns nil if the request is not
// form-encoded.
//
// htmx posts application/x-www-form-urlencoded; the AngularJS UI posts a
// colon-delimited text/plain body (`start:<infohash>`), which cannot represent
// a path containing a colon. Both are accepted while the two UIs coexist; the
// colon scheme goes away with Angular.
//
// The body is parsed from the bytes already read rather than via r.ParseForm,
// which would find the body drained.
func formValues(r *http.Request, data []byte) url.Values {
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if strings.TrimSpace(ct) != "application/x-www-form-urlencoded" {
		return nil
	}
	v, err := url.ParseQuery(string(data))
	if err != nil {
		return nil
	}
	return v
}

// checkSameOrigin rejects cross-site writes. Requests with no Origin (curl, the
// CLI) are allowed; browsers always send one on cross-origin POSTs.
func checkSameOrigin(r *http.Request) error {
	rejected := apiError{http.StatusForbidden, errors.New("Cross-origin request rejected")}
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

// fetchRemoteTorrent downloads a .torrent by URL. It refuses non-HTTP schemes and
// private address ranges: without that, this endpoint is an SSRF pivot into the
// host's own network and the cloud metadata service.
func fetchRemoteTorrent(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, badRequest("Invalid remote torrent URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, badRequest("Remote torrent URL must be http or https")
	}

	client := &http.Client{
		Timeout: remoteFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
		// Filtering at dial time also covers redirects and closes the
		// DNS-rebinding gap a hostname pre-check would leave open.
		Transport: &http.Transport{DialContext: guardedDialContext},
	}

	ctx, cancel := context.WithTimeout(ctx, remoteFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, badRequest("Invalid remote torrent URL")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, apiError{http.StatusBadGateway, fmt.Errorf("Failed to fetch remote torrent: %s", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError{http.StatusBadGateway, fmt.Errorf("Remote torrent returned HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteTorrent))
	if err != nil {
		return nil, apiError{http.StatusBadGateway, fmt.Errorf("Failed to download remote torrent: %s", err)}
	}
	return body, nil
}

// guardedDialContext blocks connections to loopback, link-local and private
// ranges.
func guardedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	for _, ip := range ips {
		if isDisallowedIP(ip.IP) {
			continue
		}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("refusing to connect to %s: no permitted address", host)
}

func isDisallowedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}
