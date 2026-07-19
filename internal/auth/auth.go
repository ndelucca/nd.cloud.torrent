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
// Here the cookie is a signed token: a random nonce and an absolute expiry,
// authenticated with HMAC-SHA256 under a key generated per process. The server
// decides the expiry and verifies it on every request — it is signed, not
// trusted from the client — and the cookie is marked HttpOnly and SameSite=Lax
// (plus Secure under TLS).
//
// There is deliberately no session table. Holding one meant every request that
// arrived with an Authorization header minted a fresh 32-byte entry with a
// fortnight TTL, with no check for an existing session: a scripted client — an
// uptime probe, curl in a loop — inflated the map without bound, and the sweep
// that ran on the login path walked it under the lock, so the cost grew with
// the abuse. A signed token makes that structurally impossible rather than
// merely bounded.
//
// The cost is that an individual session cannot be revoked server-side. Nothing
// here revoked one except the refresh path, and sessions still do not survive a
// restart, since the key is per process.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"net/http"
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
	nonceBytes   = 16
	// A token is expiry (8 bytes, big-endian Unix seconds) ‖ nonce ‖ MAC.
	expiryBytes = 8
	tokenBytes  = expiryBytes + nonceBytes + sha256.Size
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
	a := &authenticator{
		next:   next,
		user:   sha256.Sum256([]byte(user)),
		pass:   sha256.Sum256([]byte(pass)),
		secure: secure,
	}
	// Per process, and never persisted: a restart invalidates every session,
	// which is the same behaviour the session table had.
	if _, err := rand.Read(a.key[:]); err != nil {
		// Without a key no token can be signed, so every request would be
		// denied. Failing here is louder and earlier than failing per request.
		panic("auth: cannot read random key: " + err.Error())
	}
	return a
}

type authenticator struct {
	next http.Handler
	// Credentials are stored hashed so the comparison operates on two equal,
	// fixed-length values. ConstantTimeCompare returns early on a length
	// mismatch, which would leak the length of the real password.
	user, pass [sha256.Size]byte
	secure     bool
	// key signs session tokens. Generated per process, never written down.
	key [32]byte
}

func (a *authenticator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		expiry, ok := a.verify(c.Value)
		if !ok {
			a.deny(w)
			return
		}
		// Re-issue once the session is old enough, so a browser in continuous
		// use is not logged out on the fortnight boundary.
		if time.Until(expiry) < sessionTTL-refreshAfter {
			if token, exp, err := a.issue(); err == nil {
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

// verify checks a token's signature and its expiry. Both are the server's:
// the expiry travels in the cookie but is authenticated, so a client cannot
// extend its own session by editing it.
func (a *authenticator) verify(token string) (time.Time, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != tokenBytes {
		return time.Time{}, false
	}
	signed, mac := raw[:expiryBytes+nonceBytes], raw[expiryBytes+nonceBytes:]
	if !hmac.Equal(mac, a.sign(signed)) {
		return time.Time{}, false
	}
	expiry := time.Unix(int64(binary.BigEndian.Uint64(signed[:expiryBytes])), 0)
	if !time.Now().Before(expiry) {
		return time.Time{}, false
	}
	return expiry, true
}

// issue mints a session token.
func (a *authenticator) issue() (string, time.Time, error) {
	expiry := time.Now().Add(sessionTTL)
	signed := make([]byte, expiryBytes+nonceBytes)
	binary.BigEndian.PutUint64(signed[:expiryBytes], uint64(expiry.Unix()))
	if _, err := rand.Read(signed[expiryBytes:]); err != nil {
		return "", time.Time{}, err
	}
	return base64.RawURLEncoding.EncodeToString(append(signed, a.sign(signed)...)), expiry, nil
}

// sign authenticates a token's payload.
func (a *authenticator) sign(payload []byte) []byte {
	m := hmac.New(sha256.New, a.key[:])
	m.Write(payload)
	return m.Sum(nil)
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
