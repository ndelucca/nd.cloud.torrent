// Package fetch downloads a .torrent file from a URL supplied by the user.
//
// It exists as its own package because the interesting part is not the
// download, it is the refusal: without the address guard this is an
// unauthenticated SSRF pivot into the host's own network and, on a cloud
// instance, into the metadata service. Keeping it separate from the HTTP
// handlers means the guard can be tested without standing up a server, and
// means it cannot quietly acquire a dependency on request state.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MaxSize caps what will be pulled from a third party. A .torrent is
// comfortably under this.
const MaxSize = 4 << 20

const (
	timeout      = 15 * time.Second
	dialTimeout  = 10 * time.Second
	maxRedirects = 5
)

// Sentinel errors. The server maps these onto HTTP status codes, and their
// strings are shown to the user as-is, so they double as UI copy.
var (
	// ErrInvalidURL is a caller mistake: a malformed URL or a scheme we will
	// not follow.
	ErrInvalidURL = errors.New("Invalid remote torrent URL")
	// ErrBlocked means the address resolved somewhere we refuse to go. It is
	// wrapped by ErrUpstream at the dial site so callers need not special-case
	// it; it exists so a test can assert why a fetch failed.
	ErrBlocked = errors.New("refusing to connect to a non-public address")
	// ErrUpstream is any failure attributable to the remote end.
	ErrUpstream = errors.New("Failed to fetch remote torrent")
)

// Client downloads .torrent files. The zero value is ready to use and refuses
// non-public addresses; that is the only configuration anything outside tests
// should want.
type Client struct {
	// Dial, when nil, is the guarded dialer. It exists because every listener a
	// test can bind is on loopback, which the guard refuses by design — so
	// without it the only outcome reachable in a unit test would be failure.
	// Production code leaves it nil.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Torrent downloads the .torrent at raw using the default client.
func Torrent(ctx context.Context, raw string) ([]byte, error) {
	return (&Client{}).Torrent(ctx, raw)
}

// Torrent downloads the .torrent at raw.
func (c *Client) Torrent(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, ErrInvalidURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: must be http or https", ErrInvalidURL)
	}

	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			return nil
		},
		// Filtering at dial time also covers redirects and closes the
		// DNS-rebinding gap a hostname pre-check would leave open.
		Transport: &http.Transport{DialContext: c.dial()},
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrInvalidURL
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: remote returned HTTP %d", ErrUpstream, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxSize))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUpstream, err)
	}
	return body, nil
}

// guardedDialContext blocks connections to loopback, link-local and private
// ranges.
//
// The check is here rather than on the URL because a hostname says nothing
// about where it resolves: filtering at dial time is what makes redirects and
// DNS rebinding non-issues.
func guardedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: dialTimeout}
	for _, ip := range ips {
		if isDisallowedIP(ip.IP) {
			continue
		}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrBlocked, host)
}

// dial returns the configured dialer, defaulting to the guarded one. A nil Dial
// must mean "guarded", never "unrestricted": the safe behaviour has to be the
// one you get by forgetting to set anything.
func (c *Client) dial() func(context.Context, string, string) (net.Conn, error) {
	if c.Dial != nil {
		return c.Dial
	}
	return guardedDialContext
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
