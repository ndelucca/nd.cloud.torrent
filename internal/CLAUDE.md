# internal

## Purpose

Small stdlib-only packages that are not part of the public surface: replacements
for third-party helpers that cost more than they earned, plus fixture helpers
shared across packages' tests.

## Ownership

- `auth/` — `Wrap`: basic-auth login plus an opaque session cookie. Replaced
  `jpillora/cookieauth`.
- `cli/` — `App`: flag registration with env fallbacks and the help screen.
  Replaced `jpillora/opts`.
- `reqlog/` — `Wrap`: one log line per request. Replaced `jpillora/requestlog`.
- `testutil/` — `FreePort`, the one fixture helper more than one package needs.

## Local Contracts

- **Stdlib only.** These packages exist to remove dependencies; if one starts
  needing a third-party module, that is the signal to reconsider the replacement,
  not to add the import.
- Nothing here may import `server` or `engine`. `testutil` is imported from the
  `_test.go` files of `server` and `engine`, which creates no package edge —
  nothing in a shipped build links it.

`auth`:

- The cookie value is a **signed token** — expiry ‖ nonce ‖ HMAC-SHA256 under a
  per-process key — and must never be derived from the credentials. A cookie
  carrying a hash of the password makes cookie theft equivalent to password theft.
- The expiry is the server's and is verified on every request. It travels in the
  cookie but is *authenticated*, so a client cannot extend its own session by
  editing it — a cookie's `Expires` attribute is a browser hint, not access
  control.
- **There is deliberately no session table.** One grows without bound under a
  scripted client (an uptime probe, curl in a loop), since every request carrying
  an `Authorization` header mints an entry and the sweep reclaiming them walks the
  map under a lock — the cost grows with the abuse. A signed token makes that
  structurally impossible rather than merely bounded.
- **The trade, stated rather than discovered: an individual session cannot be
  revoked server-side.** The refresh path re-issues rather than invalidates, so a
  pre-refresh token stays valid until its own expiry. A restart invalidates
  everything: the signing key is generated in `Wrap` and never written down.
- Cookies are always `HttpOnly` and `SameSite=Lax`; `Secure` follows the server's
  TLS state, since setting it without TLS stops the browser returning the cookie
  at all.
- Credentials are compared as SHA-256 digests so both sides are fixed-length —
  `subtle.ConstantTimeCompare` returns early on a length mismatch and would
  otherwise leak the password's length.
- `Wrap` with empty credentials returns the handler unchanged: authentication is
  off by default and must not cost a wrapper.

`reqlog`:

- The `ResponseWriter` wrapper **must** implement `Unwrap`. `web.UI.ServeEvents`
  sets the SSE write deadline through an `http.ResponseController`, which reaches
  the real writer by walking `Unwrap`; without it the deadline silently does not
  apply.
- It must also implement `Flush` explicitly: `ServeEvents` type-asserts the writer
  to `http.Flusher` and returns 500 if that fails, and an embedded
  `ResponseWriter` does not promote `Flush`.
- Byte sizes are **base 1000**, matching `web.humanBytes`. A log that contradicts
  the screen is worse than either convention.
- The path is logged as `r.URL.EscapedPath()`, never `r.URL.Path`: `Path` is
  already percent-decoded, so logging it raw lets a caller embed a newline and
  forge entries in a line-oriented log.

`cli`:

- Precedence is default → env → flag. Defaults are read from the target struct at
  registration, so callers populate it first.
- Flags are registered explicitly rather than derived from struct tags: with this
  few options the registration list is the clearest documentation of the CLI
  surface. It and the `--help` output in `README.md` are one contract.

`testutil`:

- **`FreePort` probes UDP as well as TCP, and every caller gets both.** anacrolix
  binds TCP *and* UDP on the listen port, so a TCP-only check hands out ports whose
  UDP half is taken and `Configure` fails with "address already in use" — an
  intermittent failure that blames whichever test drew the port. A plain HTTP
  caller does not need the UDP probe, but one helper that is always right beats two
  that differ in a way nobody remembers.
- The UDP half is probed while the TCP listener is still held, then both are
  released, redrawing up to 20 times. It remains a TOCTOU, so a caller binding
  through the engine retries on top of it.

## Work Guidance

- Prefer deleting a dependency over wrapping it, but only where the replacement is
  small enough to read in one sitting.
- Security-relevant behaviour needs a test that fails against the old behaviour,
  not just one that passes against the new.

## Verification

- `go test -race ./internal/...`
- `go mod graph | grep -c jpillora` must print `0` (also a CI gate).
