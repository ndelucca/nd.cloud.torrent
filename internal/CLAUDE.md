# internal

## Purpose

Small stdlib-only replacements for third-party helpers that cost far more than they earned. Each package here exists because the shipped code used a sliver of a dependency that dragged in several modules of its own.

## Ownership

- `auth/` — `Wrap`: basic-auth login plus an opaque session cookie. Replaced `jpillora/cookieauth`.
- `cli/` — `App`: flag registration with env fallbacks and the help screen. Replaced `jpillora/opts`.
- `reqlog/` — `Wrap`: one log line per request. Replaced `jpillora/requestlog`.

## Local Contracts

- **Stdlib only.** These packages exist to remove dependencies; adding one to them defeats the point. If a package here starts needing a third-party module, that is the signal to reconsider the replacement, not to add the import.
- Nothing here may import `server` or `engine`. The dependency direction is `main` → {`server`, `internal/cli`} and `server` → {`internal/auth`, `internal/reqlog`}.

`auth`:

- The cookie value is a **signed token** — expiry ‖ nonce ‖ HMAC-SHA256 under a per-process key — and must never be derived from the credentials. The previous implementation stored a scrypt hash of `user:password`, which made cookie theft equivalent to password theft.
- The expiry is decided by the server and verified on every request. It travels in the cookie but is *authenticated*, so a client cannot extend its own session by editing it — `TestTamperedExpiryIsRejected` fails against any implementation that reads the expiry without checking the MAC. A cookie's `Expires` attribute is a hint to the browser and is not an access control.
- **There is deliberately no session table.** Holding one meant every request carrying an `Authorization` header minted a fresh 32-byte entry with a fortnight TTL, with no check for an existing session: a scripted client (an uptime probe, curl in a loop) inflated the map without bound, and the sweep on the login path walked it under the lock, so the cost grew with the abuse. A signed token makes that structurally impossible rather than merely bounded.
- **The trade, stated rather than discovered: an individual session cannot be revoked server-side.** Nothing revoked one except the refresh path, which is now a no-op — the pre-refresh token stays valid until its own expiry, and `TestSessionRefreshRotates` asserts exactly that. A restart still invalidates everything, since the key is per process.
- Cookies are always `HttpOnly` and `SameSite=Lax`; `Secure` follows the server's TLS state, since setting it without TLS stops the browser returning the cookie at all.
- Credentials are compared as SHA-256 digests so both sides are equal fixed-length values — `subtle.ConstantTimeCompare` returns early on a length mismatch and would otherwise leak the password's length.
- `Wrap` with empty credentials returns the handler unchanged. Authentication is off by default and must not cost a wrapper.
- Sessions are process-local and deliberately do not survive a restart: the signing key is generated in `Wrap` and never written down.

`reqlog`:

- The `ResponseWriter` wrapper **must** implement `Unwrap`. `serveEvents` sets the SSE write deadline through an `http.ResponseController`, which reaches the real writer by walking `Unwrap`; without it the deadline silently does not apply, and the caller has no way to react because it discards that error. `TestWriteDeadlineReachesRealWriter` pins this.
- It must also implement `Flush` explicitly. `serveEvents` type-asserts the writer to `http.Flusher` and returns 500 if that fails, and an embedded `ResponseWriter` does not promote `Flush`.
- The path is logged as `r.URL.EscapedPath()`, never `r.URL.Path`. `Path` is already percent-decoded, so logging it raw let any caller embed a newline and forge entries in a line-oriented log. `TestLogLineIsOneLine` pins it.

`cli`:

- Precedence is default → env → flag. Defaults are read from the target struct at registration, so callers populate it first.
- Flags are registered explicitly, not derived from struct tags. With this few options the registration list is the clearest documentation of the CLI surface.
- The flag set and the `--help` output in `README.md` are one contract; changing either means changing both.

## Work Guidance

- Prefer deleting a dependency over wrapping it, but only where the replacement is small enough to read in one sitting. `gzhttp` and `gopsutil` are correctly still dependencies: their value is the platform and protocol detail they encapsulate, not the lines they save.
- Security-relevant behaviour needs a test that fails against the old behaviour, not just one that passes against the new. Both regression tests here (`Unwrap`, expired sessions) were verified to fail when the fix is reverted.

## Verification

- `go test -race ./internal/...`
- `go vet ./...` and `gofmt -l .`
- `go mod graph | grep -c jpillora` must print `0`.

## Child DOX Index

No children.
