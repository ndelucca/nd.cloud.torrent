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
	"net/netip"
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

// Sentinel errors. The server maps these onto HTTP status codes and decides
// what the user is shown, so they are ordinary lowercase Go error strings.
var (
	// ErrInvalidURL is a caller mistake: a malformed URL or a scheme we will
	// not follow.
	ErrInvalidURL = errors.New("invalid remote torrent URL")
	// ErrBlocked means the address resolved somewhere we refuse to go. It is
	// wrapped by ErrUpstream at the dial site so callers need not special-case
	// it; it exists so a test can assert why a fetch failed.
	ErrBlocked = errors.New("refusing to connect to a non-public address")
	// ErrUpstream is any failure attributable to the remote end.
	ErrUpstream = errors.New("failed to fetch remote torrent")
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
		// %w twice, not %w plus %s: the dial error carries ErrBlocked, and
		// flattening it to a string broke the chain, so a caller checking for
		// ErrBlocked never saw it. The refusal was reported as a generic
		// upstream failure — a 502 saying "could not fetch" rather than a 400
		// saying which address was refused.
		return nil, fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: remote returned HTTP %d", ErrUpstream, resp.StatusCode)
	}
	// MaxSize+1: reading exactly MaxSize cannot tell "this is the whole file"
	// from "there is more". Truncating silently handed the parser a valid-
	// looking prefix, which then failed as "Invalid torrent file" and sent the
	// user hunting in the wrong place.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUpstream, err)
	}
	if len(body) > MaxSize {
		return nil, fmt.Errorf("%w: remote torrent is larger than %d bytes", ErrUpstream, MaxSize)
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

	// Partition before dialling, so "we refused" and "it did not answer" stay
	// distinguishable. Returning ErrBlocked for any dial failure told a user
	// whose host was simply down that we were refusing to connect to a
	// non-public address — a lie, in a string shown to them verbatim, on the
	// most common failure there is.
	allowed := allowedIPs(ips)
	if len(allowed) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrBlocked, host)
	}

	d := &net.Dialer{Timeout: dialTimeout}
	var lastErr error
	for _, ip := range allowed {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// allowedIPs returns the addresses that are safe to dial.
func allowedIPs(ips []net.IPAddr) []net.IP {
	var out []net.IP
	for _, ip := range ips {
		if !isDisallowedIP(ip.IP) {
			out = append(out, ip.IP)
		}
	}
	return out
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

// reservedPrefixes are ranges the stdlib predicates below do not cover. Each is
// reachable from a public-looking hostname and each lands somewhere that is not
// the public internet.
var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT — the internal network on many hosted setups
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved; also covers 255.255.255.255, which IsMulticast misses
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64 — encodes an arbitrary v4 target
	netip.MustParsePrefix("2002::/16"),     // 6to4 — likewise
}

func isDisallowedIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true // unparseable: refuse rather than guess
	}
	// Unmap first, or ::ffff:100.64.0.1 would miss every v4 prefix below.
	addr = addr.Unmap()
	for _, p := range reservedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
