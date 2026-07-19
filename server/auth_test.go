package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// internal/auth is the best-covered package in the repo, and none of it was
// wired up here: no test built a Server with Auth set, so handler()'s auth
// branch was never executed. What that branch decides is *ordering* — auth sits
// inside the security headers and outside gzip — and ordering is exactly the
// kind of thing unit tests of the middleware itself cannot see.

const (
	testUser = "naza"
	testPass = "hunter2"
)

func newAuthServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWith(t, func(o *Options) {
		o.Auth = testUser + ":" + testPass
	})
}

func TestAuthChallengesAnonymousRequests(t *testing.T) {
	s := newAuthServer(t)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
	}
}

func TestAuthAcceptsCredentialsAndIssuesASession(t *testing.T) {
	s := newAuthServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth(testUser, testPass)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "ct_session" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie issued")
	}
	// Without TLS the cookie must not be Secure, or the browser would never send
	// it back and every request would re-prompt.
	if session.Secure {
		t.Error("cookie is Secure on a plaintext server; it would never be returned")
	}
	if !session.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
}

func TestAuthRejectsWrongPassword(t *testing.T) {
	s := newAuthServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth(testUser, "wrong")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestUnauthorizedIsNotCompressed asserts that a 401 goes out as plain bytes.
//
// It does NOT pin the middleware ordering, and saying so is the point. The
// documented rationale — "auth sits outside gzip so a 401 is never compressed" —
// is not what produces this result: gzhttp does not compress below
// DefaultMinSize (1 KiB) and the challenge body is a few dozen bytes, so moving
// auth inside gzip leaves the response identical. Verified by doing exactly that
// and watching this test still pass.
//
// The property is worth asserting because it is user-visible. The ordering claim
// it looks like it covers is carried by TestUnauthorizedKeepsSecurityHeaders,
// which does fail when the chain is reordered.
func TestUnauthorizedIsNotCompressed(t *testing.T) {
	s := newAuthServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want the 401 uncompressed", enc)
	}
}

// TestUnauthorizedKeepsSecurityHeaders is the ordering test that bites.
// securityHeaders is applied outside auth, so a rejected request still carries
// them. Nesting them the other way leaves every 401 — the response an
// unauthenticated attacker sees most — without nosniff or framing protection.
// Verified to fail (X-Frame-Options and Referrer-Policy empty) when auth is
// moved outside securityHeaders.
func TestUnauthorizedKeepsSecurityHeaders(t *testing.T) {
	s := newAuthServer(t)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// TestAuthGuardsTheAPIAndDownloads is the point of the feature: --auth must
// cover the mutating surface and the file surface, not just the page. A
// challenge on / with an open /api/add would be worse than no authentication,
// because it would look protected.
func TestAuthGuardsTheAPIAndDownloads(t *testing.T) {
	s := newAuthServer(t)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/state"},
		{http.MethodPost, "/api/add"},
		{http.MethodGet, "/download/anything"},
		{http.MethodGet, "/events"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
		s.handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

// TestNoAuthOptionCostsNoWrapper pins that authentication is off by default and
// that the disabled path is not merely permissive but absent — auth.Wrap returns
// the handler untouched when both credentials are empty.
func TestNoAuthOptionCostsNoWrapper(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no --auth", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q, want none", got)
	}
}
