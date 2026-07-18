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
	}
	for _, s := range blocked {
		if !isDisallowedIP(net.ParseIP(s)) {
			t.Errorf("%s must be refused", s)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1::"}
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

// allowLoopback swaps in an unguarded dialer for the duration of a test, so the
// paths *after* the guard can be exercised against a local target. Anything
// asserting the guard itself must not use it.
func allowLoopback(t *testing.T) {
	t.Helper()
	prev := dialContext
	dialContext = (&net.Dialer{}).DialContext
	t.Cleanup(func() { dialContext = prev })
}

// TestTorrentCapsTheBody pins that a hostile or broken remote cannot stream an
// unbounded body into memory. It asserts on what Torrent returned, not on a
// re-implementation of the limit.
func TestTorrentCapsTheBody(t *testing.T) {
	allowLoopback(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for written := 0; written < MaxSize+(1<<16); written += 1 << 16 {
			if _, err := w.Write(make([]byte, 1<<16)); err != nil {
				return
			}
		}
	}))
	defer target.Close()

	body, err := Torrent(context.Background(), target.URL)
	if err != nil {
		t.Fatalf("Torrent: %v", err)
	}
	if len(body) != MaxSize {
		t.Errorf("got %d bytes, want the read to stop at MaxSize (%d)", len(body), MaxSize)
	}
}

// TestTorrentReturnsTheBody covers the success path, which is otherwise
// unreachable in a unit test: every listener a test can bind is on loopback.
func TestTorrentReturnsTheBody(t *testing.T) {
	allowLoopback(t)

	want := "d8:announce…" // not a real torrent; fetch does not parse
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(want))
	}))
	defer target.Close()

	got, err := Torrent(context.Background(), target.URL)
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
	allowLoopback(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer target.Close()

	_, err := Torrent(context.Background(), target.URL)
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want it to name the upstream status", err)
	}
}
