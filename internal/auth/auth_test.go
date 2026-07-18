package auth

import (
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

// TestExpiredSessionIsRejected is the other regression test. The previous
// implementation parsed an expiry out of the cookie and never compared it to
// the clock, so a stolen cookie was valid indefinitely.
func TestExpiredSessionIsRejected(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false).(*authenticator)

	c := sessionCookie(t, do(h, true, nil))

	// Backdate the session past its expiry.
	h.mu.Lock()
	h.sessions[c.Value] = time.Now().Add(-time.Minute)
	h.mu.Unlock()

	w := do(h, false, c)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expired session accepted: status = %d, want 401", w.Code)
	}

	// And it must be forgotten, not merely refused.
	h.mu.Lock()
	_, still := h.sessions[c.Value]
	h.mu.Unlock()
	if still {
		t.Error("expired session left in the map")
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
	c := sessionCookie(t, do(h, true, nil))

	// Age the session past refreshAfter but leave it valid.
	h.mu.Lock()
	h.sessions[c.Value] = time.Now().Add(sessionTTL - refreshAfter - time.Minute)
	h.mu.Unlock()

	w := do(h, false, c)
	if w.Code != http.StatusOK {
		t.Fatalf("valid session rejected: status = %d", w.Code)
	}
	fresh := sessionCookie(t, w)
	if fresh.Value == c.Value {
		t.Error("session was not rotated on refresh")
	}
	if w2 := do(h, false, c); w2.Code != http.StatusUnauthorized {
		t.Errorf("the rotated-out token still works: status = %d", w2.Code)
	}
}

// TestExpiredSessionsAreSwept keeps the map from growing without bound across
// long uptimes.
func TestExpiredSessionsAreSwept(t *testing.T) {
	h := Wrap(okHandler(), user, pass, false).(*authenticator)

	h.mu.Lock()
	for _, tok := range []string{"old-1", "old-2", "old-3"} {
		h.sessions[tok] = time.Now().Add(-time.Hour)
	}
	h.mu.Unlock()

	do(h, true, nil) // a fresh login sweeps

	h.mu.Lock()
	n := len(h.sessions)
	h.mu.Unlock()
	if n != 1 {
		t.Errorf("%d sessions after a sweep, want 1 (only the new one)", n)
	}
}

// TestConcurrentLogins exercises the map under -race; the previous bugs in this
// codebase were all unsynchronized map access.
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
