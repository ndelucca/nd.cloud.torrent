# fetch

## Purpose

Downloads a `.torrent` file from a URL the user supplied, refusing to reach into
the host's own network while doing it.

## Ownership

- `fetch.go` — `Torrent`, the sentinel errors, `guardedDialContext`, `allowedIPs` and the address table in `reservedPrefixes`/`isDisallowedIP`

## Local Contracts

- **Stdlib only, and no knowledge of HTTP status codes or request state.** The package returns sentinels (`ErrInvalidURL`, `ErrUpstream`, `ErrBlocked`); `server.classify` maps them. It previously returned an `apiError` carrying a status, which is what kept it welded to the handler layer.
- **The address check happens at dial time, not on the URL.** A hostname says nothing about where it resolves, so a pre-check leaves both redirects and DNS rebinding open. `guardedDialContext` is the only correct place for it.
- `isDisallowedIP` refuses loopback, private (v4 and v6 ULA), link-local, unspecified and multicast via the stdlib predicates, plus a `reservedPrefixes` table for the ranges those miss: CGNAT `100.64.0.0/10` (the internal network on many hosted setups), `192.0.0.0/24`, `198.18.0.0/15`, `240.0.0.0/4` (which is also how `255.255.255.255` is caught — `IsMulticast` tests for `0xe0`, so `0xff` slips past it), and NAT64 `64:ff9b::/96` / 6to4 `2002::/16`, both of which encode an arbitrary v4 target. Addresses are `Unmap`ped first, or `::ffff:100.64.0.1` would miss every v4 prefix. The link-local case is not theoretical: `169.254.169.254` is the cloud metadata service. TEST-NET stays allowed on purpose — blocking it buys nothing and costs the only routable-but-dead address a test can point at.
- **`ErrBlocked` means we refused, never "it did not answer".** The dial loop partitions with `allowedIPs` first and returns the last real dial error when every allowed candidate fails. Returning `ErrBlocked` for any dial failure told a user whose host was simply down that we refuse to connect to a non-public address — a lie, in a string shown to them verbatim, on the most common failure there is.
- Bodies are capped at `MaxSize`, the request is bounded by `timeout`, and redirects by `maxRedirects`. All three are needed: this endpoint is reachable by anyone who can reach the UI.
- **An oversized body is an error, not a truncation.** The read takes `MaxSize+1` so "this is the whole file" and "there is more" stay distinguishable. Silently truncating handed the torrent parser a valid-looking prefix, which failed as "Invalid torrent file" and sent the user looking in the wrong place. `TestTorrentCapsTheBody` was inverted for this and says so.
- Error strings are ordinary lowercase Go. `server.classify` decides what the user sees: `ErrInvalidURL` and `ErrBlocked` are the caller's doing and show their detail, while `ErrUpstream` gets a fixed message because its wrapped text is a dial or syscall string.
- **The zero `Client` is guarded.** `Client.Dial` defaults to `guardedDialContext` when nil, so the safe behaviour is the one you get by forgetting to configure anything. Never invert that: a nil `Dial` must not come to mean "unrestricted".
- `Dial` exists only because every listener a test can bind is on loopback, which the guard refuses by design — without it the only outcome reachable in a unit test would be failure. Production code uses the package-level `Torrent`, which is the zero `Client`. Any test asserting the guard itself must use the zero `Client` too; `TestZeroClientIsGuarded` pins that the default direction is refusal.

## Work Guidance

- Treat any change to `isDisallowedIP` as security-relevant: add the address to the table in `TestIsDisallowedIP` first and watch it fail.
- Do not add a "just this once" bypass for a private address. The download directory is the only thing this server should be able to write, and this is the only place it talks to an arbitrary host.

## Verification

- `go test -race ./fetch/`
- The guard is also covered end to end from the server: `TestSSRFGuard` posts a loopback URL and a link-local one to `/api/add`.

## Child DOX Index

No children.
