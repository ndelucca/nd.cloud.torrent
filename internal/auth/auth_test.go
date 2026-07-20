package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	user = "naza"
	pass = "correct-horse"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("served"))
	})
}

// do runs one request through h, optionally with credentials and a cookie.
func do(h http.Handler, creds bool, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if creds {
		r.SetBasicAuth(user, pass)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// sessionCookie extracts the issued cookie, failing the test if absent.
func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName && c.Value != "" {
			return c
		}
	}
	t.Fatal("no session cookie was issued")
	return nil
}

// comparableHandler exists because http.HandlerFunc is a func value and so
// cannot be compared for identity.
type comparableHandler struct{}

func (comparableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("served"))
}

// TestNoCredentialsIsPassthrough: authentication is off by default, and Wrap
// must return the handler untouched rather than a wrapper that always allows.
func TestNoCredentialsIsPassthrough(t *testing.T) {
	h := comparableHandler{}
	got := Wrap(h, "", "", false)
	if got != http.Handler(h) {
		t.Error("Wrap with empty credentials must return the handler unchanged")
	}
	if w := do(got, false, nil); w.Code != http.StatusOK {
		t.Errorf("unauthenticated request blocked with auth disabled: %d", w.Code)
	}
}

func TestMissingAndWrongCredentialsAreRejected(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false)

	t.Run("no credentials", func(t *testing.T) {
		w := do(h, false, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
		if a := w.Header().Get("WWW-Authenticate"); !strings.HasPrefix(a, "Basic ") {
			t.Errorf("WWW-Authenticate = %q; the browser will not prompt", a)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetBasicAuth(user, "wrong")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
		if strings.Contains(w.Body.String(), "served") {
			t.Error("handler ran despite a bad password")
		}
	})

	t.Run("wrong user", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetBasicAuth("someone", pass)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
	})
}

func TestGoodCredentialsIssueAUsableSession(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false)

	w := do(h, true, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	c := sessionCookie(t, w)

	// The cookie alone must authenticate the next request.
	w2 := do(h, false, c)
	if w2.Code != http.StatusOK {
		t.Errorf("cookie did not authenticate: status = %d", w2.Code)
	}
	if w2.Body.String() != "served" {
		t.Errorf("body = %q", w2.Body.String())
	}
}

// TestCookieAttributes covers the three flags the previous implementation
// omitted.
func TestCookieAttributes(t *testing.T) {
	t.Run("without TLS", func(t *testing.T) {
		c := sessionCookie(t, do(Wrap(okHandler(), user, pass, false), true, nil))
		if !c.HttpOnly {
			t.Error("cookie is not HttpOnly: any XSS can read the session")
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("SameSite = %v, want Lax", c.SameSite)
		}
		if c.Secure {
			t.Error("Secure set without TLS: the browser would never send it back")
		}
	})

	t.Run("with TLS", func(t *testing.T) {
		c := sessionCookie(t, do(Wrap(okHandler(), user, pass, true), true, nil))
		if !c.Secure {
			t.Error("cookie is not Secure under TLS")
		}
	})
}

// TestCookieDoesNotDeriveFromThePassword is the regression test for the core
// design flaw: the old cookie value was a scrypt hash of "user:password", so
// possession of the cookie meant possession of a crackable password hash.
func TestCookieDoesNotDeriveFromThePassword(t *testing.T) {
	c := sessionCookie(t, do(Wrap(okHandler(), user, pass, false), true, nil))
	for _, secret := range []string{user, pass, user + ":" + pass} {
		if strings.Contains(c.Value, secret) {
			t.Errorf("cookie value contains %q", secret)
		}
	}

	// Two logins must not produce the same token, or it is a function of the
	// credentials rather than a random session identifier.
	h := Wrap(okHandler(), user, pass, false)
	first := sessionCookie(t, do(h, true, nil))
	second := sessionCookie(t, do(h, true, nil))
	if first.Value == second.Value {
		t.Error("two logins produced the same token: it is not random")
	}
}

// tokenAt mints a validly-signed token with a chosen expiry, so tests can place
// a session anywhere in its lifetime without a session table to reach into.
func tokenAt(a *authenticator, expiry time.Time) string {
	signed := make([]byte, expiryBytes+nonceBytes)
	binary.BigEndian.PutUint64(signed[:expiryBytes], uint64(expiry.Unix()))
	rand.Read(signed[expiryBytes:])
	return base64.RawURLEncoding.EncodeToString(append(signed, a.sign(signed)...))
}

// TestExpiredSessionIsRejected is the other regression test. The previous
// implementation parsed an expiry out of the cookie and never compared it to
// the clock, so a stolen cookie was valid indefinitely.
func TestExpiredSessionIsRejected(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false).(*authenticator)

	c := &http.Cookie{Name: cookieName, Value: tokenAt(h, time.Now().Add(-time.Minute))}
	if w := do(h, false, c); w.Code != http.StatusUnauthorized {
		t.Errorf("expired session accepted: status = %d, want 401", w.Code)
	}
}

// TestTamperedExpiryIsRejected is the behaviour the signature exists for.
//
// The expiry now travels inside the cookie rather than in a server-side table,
// so the thing that must not work is a client editing it to extend its own
// session. This fails against any implementation that reads the expiry without
// authenticating it.
func TestTamperedExpiryIsRejected(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false).(*authenticator)

	expired := tokenAt(h, time.Now().Add(-time.Minute))
	raw, err := base64.RawURLEncoding.DecodeString(expired)
	if err != nil {
		t.Fatal(err)
	}
	// Push the expiry a year out, leaving the MAC untouched.
	binary.BigEndian.PutUint64(raw[:expiryBytes], uint64(time.Now().Add(365*24*time.Hour).Unix()))
	forged := base64.RawURLEncoding.EncodeToString(raw)

	w := do(h, false, &http.Cookie{Name: cookieName, Value: forged})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a token with an edited expiry was accepted: status = %d", w.Code)
	}
}

// TestTamperedNonceIsRejected covers the rest of the payload.
func TestTamperedNonceIsRejected(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false).(*authenticator)
	raw, err := base64.RawURLEncoding.DecodeString(tokenAt(h, time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	raw[expiryBytes] ^= 0xff
	w := do(h, false, &http.Cookie{Name: cookieName, Value: base64.RawURLEncoding.EncodeToString(raw)})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a token with an edited nonce was accepted: status = %d", w.Code)
	}
}

func TestForgedTokenIsRejected(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false)
	w := do(h, false, &http.Cookie{Name: cookieName, Value: "not-a-real-token"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("forged token accepted: status = %d", w.Code)
	}
}

// TestSessionRefreshRotates covers the re-issue path: an old-but-valid session
// gets a fresh token, and the old one stops working.
func TestSessionRefreshRotates(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false).(*authenticator)

	// Old enough to refresh, still valid.
	c := &http.Cookie{Name: cookieName, Value: tokenAt(h, time.Now().Add(sessionTTL-refreshAfter-time.Minute))}

	w := do(h, false, c)
	if w.Code != http.StatusOK {
		t.Fatalf("valid session rejected: status = %d", w.Code)
	}
	fresh := sessionCookie(t, w)
	if fresh.Value == c.Value {
		t.Error("session was not rotated on refresh")
	}
	// The old token keeps working until its own expiry, which is the trade a
	// signed token makes: there is no table to revoke it in. It is stated here
	// rather than left to be discovered.
	if w2 := do(h, false, c); w2.Code != http.StatusOK {
		t.Errorf("the pre-refresh token should stay valid until it expires: status = %d", w2.Code)
	}
}

// TestManyLoginsAllocateNothingPersistent replaces a test for the sweep that
// kept the session table bounded. There is no table now: every request that
// arrived with an Authorization header used to mint a fortnight-long entry with
// no check for an existing session, so a scripted caller inflated it without
// limit while the sweep walked it under the lock. A signed token makes that
// structurally impossible, so what is worth pinning is that many logins still
// produce distinct, usable sessions.
func TestManyLoginsProduceDistinctSessions(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := sessionCookie(t, do(h, true, nil))
		if seen[c.Value] {
			t.Fatalf("token repeated after %d logins", i)
		}
		seen[c.Value] = true
		if w := do(h, false, c); w.Code != http.StatusOK {
			t.Fatalf("freshly issued session rejected: status = %d", w.Code)
		}
	}
}

// TestConcurrentLogins exercises the authenticator under -race. There is no
// shared mutable state left, which is the point: it must stay that way.
func TestConcurrentLogins(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false)
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			w := do(h, true, nil)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d", w.Code)
				return
			}
			for _, c := range w.Result().Cookies() {
				if c.Name == cookieName && c.Value != "" {
					do(h, false, c)
				}
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}

// TestUnusableCookieFallsThroughToCredentials covers a client that proved it
// knows the password and was refused anyway.
//
// A cookie that does not verify — expired, forged, or left over from a previous
// process, since the signing key is per-process — used to deny the request
// outright, without ever consulting the Authorization header. Browsers hid it:
// the 401 clears the cookie and re-prompts, so the next request succeeds. A
// scripted client with a cookie jar and credentials (an uptime probe, curl with
// -b and -u) got a hard 401 on its first request after the boundary.
//
// Strictly more permissive only to callers who already authenticate: with no
// credentials, or wrong ones, deny still fires — the two cases below.
func TestUnusableCookieFallsThroughToCredentials(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false).(*authenticator)

	stale := &http.Cookie{Name: cookieName, Value: tokenAt(h, time.Now().Add(-time.Minute))}
	forged := &http.Cookie{Name: cookieName, Value: "not-a-real-token"}

	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
	}{
		{"expired session", stale},
		{"forged token", forged},
	} {
		t.Run(tc.name+" with credentials", func(t *testing.T) {
			w := do(h, true, tc.cookie)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: valid credentials must be honoured "+
					"even when the request carries an unusable cookie", w.Code)
			}
			// And the caller is put back on a working session rather than
			// re-authenticating on every request from here on.
			if sessionCookie(t, w) == nil {
				t.Error("no fresh session was issued")
			}
		})

		t.Run(tc.name+" without credentials", func(t *testing.T) {
			if w := do(h, false, tc.cookie); w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401: an unusable cookie alone grants nothing", w.Code)
			}
		})
	}
}
