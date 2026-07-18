// Package auth gates an HTTP handler behind a username and password.
//
// It replaces jpillora/cookieauth, which had three problems beyond the two
// modules it cost:
//
//   - The cookie's value was a scrypt hash of "user:password". Anyone who
//     obtained the cookie held an offline-crackable hash of the real password. A
//     session token should not be derived from the secret it stands in for.
//   - The expiry was an unsigned integer inside that value, and it was never
//     checked. It only decided whether to re-issue the cookie, so the Expires
//     attribute was a hint to the browser and nothing more: a stolen cookie
//     stayed valid forever, until the password changed.
//   - The cookie carried no HttpOnly, Secure or SameSite attribute.
//
// Here the cookie is an opaque random token, the server holds the expiry and
// enforces it, and the cookie is marked HttpOnly and SameSite=Lax (plus Secure
// under TLS).
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

const (
	cookieName = "ct_session"
	realm      = "cloud-torrent"
	// sessionTTL matches the fortnight the previous implementation advertised.
	sessionTTL = 14 * 24 * time.Hour
	// refreshAfter is how much of a session must elapse before a request
	// re-issues the cookie, so an active browser is not logged out mid-use
	// while an idle one still expires.
	refreshAfter = 24 * time.Hour
	tokenBytes   = 32
)

// Wrap returns a handler that requires user and pass via HTTP basic auth,
// issuing a session cookie on success. If both are empty it returns next
// untouched — the server runs without authentication by default.
//
// secure marks the cookie Secure; pass whether the server is serving TLS.
func Wrap(next http.Handler, user, pass string, secure bool) http.Handler {
	if user == "" && pass == "" {
		return next
	}
	return &authenticator{
		next:     next,
		user:     sha256.Sum256([]byte(user)),
		pass:     sha256.Sum256([]byte(pass)),
		secure:   secure,
		sessions: make(map[string]time.Time),
	}
}

type authenticator struct {
	next http.Handler
	// Credentials are stored hashed so the comparison operates on two equal,
	// fixed-length values. ConstantTimeCompare returns early on a length
	// mismatch, which would leak the length of the real password.
	user, pass [sha256.Size]byte
	secure     bool

	mu       sync.Mutex
	sessions map[string]time.Time // token → absolute expiry
}

func (a *authenticator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		expiry, ok := a.lookup(c.Value)
		if !ok {
			a.deny(w)
			return
		}
		// Re-issue once the session is old enough, so a browser in continuous
		// use is not logged out on the fortnight boundary.
		if time.Until(expiry) < sessionTTL-refreshAfter {
			if token, exp, err := a.issue(); err == nil {
				a.revoke(c.Value)
				http.SetCookie(w, a.cookie(token, exp))
			}
		}
		a.next.ServeHTTP(w, r)
		return
	}

	user, pass, ok := r.BasicAuth()
	if !ok || !a.valid(user, pass) {
		a.deny(w)
		return
	}
	token, expiry, err := a.issue()
	if err != nil {
		// Fail closed: without a token there is no session to grant, and
		// serving the request anyway would authenticate without one.
		http.Error(w, "Could not start a session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, a.cookie(token, expiry))
	a.next.ServeHTTP(w, r)
}

// valid compares credentials in constant time. Both halves are always compared;
// combining with & rather than && keeps the cost independent of which one is
// wrong.
func (a *authenticator) valid(user, pass string) bool {
	gotUser := sha256.Sum256([]byte(user))
	gotPass := sha256.Sum256([]byte(pass))
	okUser := subtle.ConstantTimeCompare(gotUser[:], a.user[:])
	okPass := subtle.ConstantTimeCompare(gotPass[:], a.pass[:])
	return okUser&okPass == 1
}

// lookup returns the session's expiry, enforcing it. This is the check the
// previous implementation was missing entirely.
func (a *authenticator) lookup(token string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	expiry, ok := a.sessions[token]
	if !ok {
		return time.Time{}, false
	}
	if !time.Now().Before(expiry) {
		delete(a.sessions, token)
		return time.Time{}, false
	}
	return expiry, true
}

// issue mints a session token. The map only grows on successful logins, so an
// unauthenticated caller cannot inflate it; expired entries are swept here.
func (a *authenticator) issue() (string, time.Time, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	expiry := time.Now().Add(sessionTTL)

	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for t, exp := range a.sessions {
		if !now.Before(exp) {
			delete(a.sessions, t)
		}
	}
	a.sessions[token] = expiry
	return token, expiry, nil
}

func (a *authenticator) revoke(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

func (a *authenticator) cookie(token string, expiry time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.secure,
	}
}

// deny clears any stale cookie and asks for credentials, so a browser holding
// an expired session is re-prompted rather than stuck.
func (a *authenticator) deny(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.secure,
	})
	w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}
