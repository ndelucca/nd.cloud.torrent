package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/fetch"
)

// TestClassify pins the error policy.
//
// The axis is whether the caller's input caused the failure, not which package
// the error came from. Input errors show their wrapped detail because that
// detail is the useful part and is bounded parser prose; operational errors get
// a fixed message because theirs is a syscall string and a filesystem-layout
// oracle.
func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		// showsDetail asserts the wrapped text reaches the user, or must not.
		detail      string
		showsDetail bool
	}{
		{
			name:       "malformed magnet is the caller's",
			err:        fmt.Errorf("%w: invalid magnet URI: no info hash", engine.ErrInvalidInput),
			wantStatus: http.StatusBadRequest,
			detail:     "no info hash", showsDetail: true,
		},
		{
			name:       "bad infohash is the caller's",
			err:        fmt.Errorf("%w: infohash is not a hex string", engine.ErrInvalidInput),
			wantStatus: http.StatusBadRequest,
			detail:     "hex string", showsDetail: true,
		},
		{
			name:       "bad remote URL is the caller's",
			err:        fetch.ErrInvalidURL,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a refused address is about what was asked for",
			err:        fmt.Errorf("%w: %w: 10.0.0.1", fetch.ErrUpstream, fetch.ErrBlocked),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing torrent",
			err:        fmt.Errorf("%w abc", engine.ErrMissingTorrent),
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "already started",
			err:        engine.ErrAlreadyStarted,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "engine unavailable",
			err:        engine.ErrNotConfigured,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "upstream failure hides its detail",
			err:        fmt.Errorf("%w: dial tcp 93.184.216.34:443: connect: connection refused", fetch.ErrUpstream),
			wantStatus: http.StatusBadGateway,
			detail:     "connection refused", showsDetail: false,
		},
		{
			name:       "apiError carries its own status",
			err:        apiError{http.StatusForbidden, errors.New("cross-origin request rejected")},
			wantStatus: http.StatusForbidden,
			detail:     "origin request rejected", showsDetail: true,
		},
		{
			// The regression this whole change exists for: a disk or permission
			// failure was served as 400, telling the user it was their mistake,
			// with the filesystem path attached.
			name:       "an unknown failure is the server's, and says nothing",
			err:        fmt.Errorf("failed to save configuration: open /etc/secret/x: permission denied"),
			wantStatus: http.StatusInternalServerError,
			detail:     "/etc/secret", showsDetail: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, msg := classify(c.err)
			if status != c.wantStatus {
				t.Errorf("status = %d, want %d (message %q)", status, c.wantStatus, msg)
			}
			if c.detail != "" {
				got := strings.Contains(msg, c.detail)
				if got != c.showsDetail {
					verb := "must not appear in"
					if c.showsDetail {
						verb = "must appear in"
					}
					t.Errorf("%q %s the message, got %q", c.detail, verb, msg)
				}
			}
			if msg == "" {
				t.Error("every classified error needs a message")
			}
			if r := []rune(msg); r[0] < 'A' || r[0] > 'Z' {
				t.Errorf("message %q must read as UI copy: capitalised", msg)
			}
		})
	}
}

// TestSentence covers the presentation helper the server owns now that error
// strings are conventional Go again.
func TestSentence(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"already started":     "Already started.",
		"missing torrent abc": "Missing torrent abc.",
		"Already punctuated.": "Already punctuated.",
		"really?":             "Really?",
	}
	for in, want := range cases {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}
