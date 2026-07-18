# fetch

## Purpose

Downloads a `.torrent` file from a URL the user supplied, refusing to reach into
the host's own network while doing it.

## Ownership

- `fetch.go` — `Torrent`, the sentinel errors, `guardedDialContext` and the address table in `isDisallowedIP`

## Local Contracts

- **Stdlib only, and no knowledge of HTTP status codes or request state.** The package returns sentinels (`ErrInvalidURL`, `ErrUpstream`, `ErrBlocked`); `server.statusFor` maps them. It previously returned an `apiError` carrying a status, which is what kept it welded to the handler layer.
- **The address check happens at dial time, not on the URL.** A hostname says nothing about where it resolves, so a pre-check leaves both redirects and DNS rebinding open. `guardedDialContext` is the only correct place for it.
- `isDisallowedIP` refuses loopback, private (v4 and v6 ULA), link-local, unspecified and multicast. The link-local case is not theoretical: `169.254.169.254` is the cloud metadata service.
- Bodies are capped at `MaxSize`, the request is bounded by `timeout`, and redirects by `maxRedirects`. All three are needed: this endpoint is reachable by anyone who can reach the UI.
- Error strings are surfaced to the user verbatim by the server, so they read as UI copy. This is the repo-wide convention that `staticcheck.conf` disables ST1005 for.
- `dialContext` is a package variable **only** so tests can reach a target. Every listener a test can bind is on loopback, which the guard refuses by design, so without the seam the only testable outcome would be failure. Nothing outside `_test.go` may reassign it, and any test asserting the guard itself must not use `allowLoopback`.

## Work Guidance

- Treat any change to `isDisallowedIP` as security-relevant: add the address to the table in `TestIsDisallowedIP` first and watch it fail.
- Do not add a "just this once" bypass for a private address. The download directory is the only thing this server should be able to write, and this is the only place it talks to an arbitrary host.

## Verification

- `go test -race ./fetch/`
- The guard is also covered end to end from the server: `TestSSRFGuard` posts a loopback URL and a link-local one to `/api/add`.

## Child DOX Index

No children.
