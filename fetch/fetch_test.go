package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsDisallowedIP is the table the guard turns on. Everything else in this
// package is plumbing around this decision.
func TestIsDisallowedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", // loopback
		"10.0.0.1", "192.168.1.1", "172.16.0.1", // private v4
		"fd00::1",         // private v6 (unique local)
		"169.254.169.254", // link-local: the cloud metadata service
		"fe80::1",         // link-local v6
		"0.0.0.0", "::",   // unspecified
		"224.0.0.1", "ff02::1", // multicast
		"255.255.255.255",   // broadcast: IsMulticast tests 0xe0, so 0xff slips past it
		"100.64.0.1",        // CGNAT — the internal network on many hosted setups
		"192.0.0.1",         // IETF protocol assignments
		"198.18.0.1",        // benchmarking
		"240.0.0.1",         // reserved
		"64:ff9b::7f00:1",   // NAT64 wrapping 127.0.0.1
		"2002:7f00:1::",     // 6to4 wrapping 127.0.0.1
		"::ffff:100.64.0.1", // v4-mapped CGNAT: must not slip past the v4 prefixes
	}
	for _, s := range blocked {
		if !isDisallowedIP(net.ParseIP(s)) {
			t.Errorf("%s must be refused", s)
		}
	}

	// TEST-NET stays allowed on purpose: blocking it buys nothing and costs the
	// only routable-but-dead address a test can point at.
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1::", "192.0.2.1"}
	for _, s := range allowed {
		if isDisallowedIP(net.ParseIP(s)) {
			t.Errorf("%s is a public address and must be allowed", s)
		}
	}
}

func TestTorrentRejectsNonHTTPSchemes(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"ftp://example.com/x.torrent",
		"gopher://example.com/",
		"javascript:alert(1)",
	} {
		_, err := Torrent(context.Background(), raw)
		if !errors.Is(err, ErrInvalidURL) {
			t.Errorf("Torrent(%q) = %v, want ErrInvalidURL", raw, err)
		}
	}
}

// TestTorrentRefusesLoopback is the SSRF case with a live target: a successful
// fetch would prove the guard is not wired in.
func TestTorrentRefusesLoopback(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer target.Close()

	_, err := Torrent(context.Background(), target.URL)
	if err == nil {
		t.Fatal("fetching a loopback address succeeded")
	}
	if reached {
		t.Error("the guard let the request through to a loopback listener")
	}
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v, want it to wrap ErrUpstream so the server maps it to 502", err)
	}
	// The dial-time refusal is what produced it, not a connection failure.
	if !strings.Contains(err.Error(), ErrBlocked.Error()) {
		t.Errorf("err = %v, want the blocked-address reason", err)
	}
}

func TestTorrentRefusesLinkLocalMetadata(t *testing.T) {
	_, err := Torrent(context.Background(), "http://169.254.169.254/latest/meta-data/")
	if err == nil {
		t.Fatal("fetching the cloud metadata service succeeded")
	}
	if !strings.Contains(err.Error(), ErrBlocked.Error()) {
		t.Errorf("err = %v, want the blocked-address reason", err)
	}
}

// loopbackClient is a Client that may reach a local target, so the paths *after*
// the guard can be exercised. Anything asserting the guard itself must use the
// zero Client instead.
func loopbackClient() *Client {
	return &Client{Dial: (&net.Dialer{}).DialContext}
}

// TestZeroClientIsGuarded pins the direction the default has to fail in: a
// Client nobody configured must refuse, not allow.
func TestZeroClientIsGuarded(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the zero Client reached a loopback listener")
	}))
	defer target.Close()

	if _, err := (&Client{}).Torrent(context.Background(), target.URL); err == nil {
		t.Fatal("a Client with no Dial set must still be guarded")
	}
}

// TestTorrentCapsTheBody pins that a hostile or broken remote cannot stream an
// unbounded body into memory. It asserts on what Torrent returned, not on a
// re-implementation of the limit.
//
// This assertion was inverted deliberately. It used to require exactly MaxSize
// bytes back with a nil error — enshrining a silent truncation, which handed
// the torrent parser a valid-looking prefix that then failed as "Invalid
// torrent file" and sent the user looking in the wrong place. An oversized
// body is now an error that says so.
func TestTorrentCapsTheBody(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for written := 0; written < MaxSize+(1<<16); written += 1 << 16 {
			if _, err := w.Write(make([]byte, 1<<16)); err != nil {
				return
			}
		}
	}))
	defer target.Close()

	body, err := loopbackClient().Torrent(context.Background(), target.URL)
	if err == nil {
		t.Fatalf("an oversized body must be reported, got %d bytes and no error", len(body))
	}
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("error = %v, want it to wrap ErrUpstream", err)
	}
	if body != nil {
		t.Errorf("got %d bytes back alongside the error; a truncated torrent must not be returned", len(body))
	}
}

// TestTorrentReturnsTheBody covers the success path, which is otherwise
// unreachable in a unit test: every listener a test can bind is on loopback.
func TestTorrentReturnsTheBody(t *testing.T) {
	want := "d8:announce…" // not a real torrent; fetch does not parse
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(want))
	}))
	defer target.Close()

	got, err := loopbackClient().Torrent(context.Background(), target.URL)
	if err != nil {
		t.Fatalf("Torrent: %v", err)
	}
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestTorrentReportsUpstreamStatus: a 404 from the remote is not a client error
// here, and the server maps ErrUpstream to 502.
func TestTorrentReportsUpstreamStatus(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer target.Close()

	_, err := loopbackClient().Torrent(context.Background(), target.URL)
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want it to name the upstream status", err)
	}
}

// TestAllowedIPsPartitions covers the difference between "we refused" and "it
// did not answer".
//
// The dial loop used to return ErrBlocked whenever no candidate connected, so a
// host that was simply down told the user we were refusing to connect to a
// non-public address — a lie, in a string shown to them verbatim, on the most
// common failure there is. Partitioning first is what keeps the two apart; the
// end-to-end half is not tested here because it needs a 10s dial timeout
// against a black hole.
func TestAllowedIPsPartitions(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"all public", []string{"8.8.8.8", "1.1.1.1"}, []string{"8.8.8.8", "1.1.1.1"}},
		{"all refused", []string{"127.0.0.1", "10.0.0.1"}, nil},
		{"mixed keeps only the public one", []string{"127.0.0.1", "8.8.8.8", "169.254.169.254"}, []string{"8.8.8.8"}},
		{"empty", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var in []net.IPAddr
			for _, s := range c.in {
				in = append(in, net.IPAddr{IP: net.ParseIP(s)})
			}
			got := allowedIPs(in)
			if len(got) != len(c.want) {
				t.Fatalf("allowedIPs(%v) = %v, want %v", c.in, got, c.want)
			}
			for i, ip := range got {
				if ip.String() != c.want[i] {
					t.Errorf("index %d = %s, want %s", i, ip, c.want[i])
				}
			}
		})
	}
}
