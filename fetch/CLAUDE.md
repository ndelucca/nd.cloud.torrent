# fetch

## Purpose

Downloads a `.torrent` from a user-supplied URL, refusing to reach into the
host's own network while doing it.

## Ownership

- `fetch.go` — `Client`, `Torrent`, `MaxSize`, the sentinel errors,
  `guardedDialContext`, `allowedIPs`, `isDisallowedIP`, `reservedPrefixes`

## Local Contracts

- **Stdlib only, with no knowledge of status codes or request state.** The package
  returns sentinels (`ErrInvalidURL`, `ErrUpstream`, `ErrBlocked`) and
  `server.classify` maps them: the first two are the caller's doing and show their
  detail, `ErrUpstream` gets a fixed message because its wrapped text is a dial or
  syscall string.
- **The address check happens at dial time, not on the URL.** A hostname says
  nothing about where it resolves, so a pre-check leaves redirects and DNS
  rebinding open. `guardedDialContext` is the only correct place for it.
- `isDisallowedIP` refuses loopback, private (v4 and v6 ULA), link-local,
  unspecified and multicast via the stdlib predicates, plus `reservedPrefixes` for
  what those miss: `100.64.0.0/10` (CGNAT — the internal network on many hosted
  setups), `192.0.0.0/24`, `198.18.0.0/15`, `240.0.0.0/4` (also how
  `255.255.255.255` is caught, since `IsMulticast` tests for `0xe0` and `0xff`
  slips past), and `64:ff9b::/96` (NAT64) / `2002::/16` (6to4), which each encode
  an arbitrary v4 target. Addresses are `Unmap`ped first, or `::ffff:100.64.0.1`
  would miss every v4 prefix. Link-local is not theoretical: `169.254.169.254` is
  the cloud metadata service. TEST-NET stays allowed on purpose — blocking it buys
  nothing and costs the only routable-but-dead address a test can point at.
- **`ErrBlocked` means we refused, never "it did not answer".** The dial loop
  partitions with `allowedIPs` first and returns the last real dial error when
  every allowed candidate fails, so a user whose host is simply down is not told we
  refuse non-public addresses.
- Bodies are capped by `MaxSize`, the request by `timeout`, redirects by
  `maxRedirects`. All three are needed: anyone who can reach the UI reaches this.
- **An oversized body is an error, not a truncation.** The read takes `MaxSize+1`
  so "whole file" and "there is more" stay distinguishable; truncating hands the
  parser a valid-looking prefix.
- **The zero `Client` is guarded.** `Client.Dial` defaults to `guardedDialContext`
  when nil, so the safe behaviour is what you get by configuring nothing. Never
  invert that. `Dial` exists only because every listener a test can bind is on
  loopback, which the guard refuses by design; production uses the package-level
  `Torrent`, which is the zero `Client`.

## Work Guidance

- Any change to `isDisallowedIP` is security-relevant: add the address to the test
  table first and watch it fail.
- Do not add a "just this once" bypass. This is the only place the server talks to
  an arbitrary host.

## Verification

- `go test -race ./fetch/ ./server/` — the guard is covered here and end to end
  through `/api/add`.
