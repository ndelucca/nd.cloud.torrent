# engine

## Purpose

Wraps `anacrolix/torrent` in a small, server-friendly facade: one `*torrent.Client`, a mutable `Config`, and a cache of `*Torrent` view models keyed by hex infohash.

## Ownership

- `engine.go` — `Engine` type: client lifecycle (`Configure`), adding torrents (`NewMagnet`, `NewTorrentFile`), the `ts` cache (`GetTorrents`, `upsertLocked`), and start/stop/delete for torrents
- `torrent.go` — `Torrent` and `File` view models, `update` (copies live state out of the underlying torrent) and `clone` (deep copy for callers)
- `config.go` — the `Config` struct persisted by the server as JSON, plus `Validate`

## Local Contracts

- `Engine` must not import `server` or `static`; it is the bottom of the dependency chain
- Public methods take a hex infohash string; `str2ih` checks the length *before* decoding — `hex.Decode` is bounded by `len(src)`, not `len(dst)`, so an over-long input writes past the array and panics
- `Configure` is serialized end to end by `configureMu`. `mu` alone is not enough: the same-port path releases `mu` between dropping the old client and installing the replacement, and a second caller in that window saw `client == nil`, took the non-retrying branch and stole the port.
- `Configure` picks its teardown order by whether the listen port changes. Different port: build the replacement first, so a failure leaves the running client untouched. Same port: the old client holds the port, so it must be closed and waited on first — building first fails with "address already in use" every time, which is the common case since any other settings change keeps the port. Waiting on `Closed()` is necessary but not sufficient (the kernel releases the socket slightly later), hence the bounded rebind retry.
- Re-added magnets whose metadata has not arrived get a fresh `watchInfo`. Their original watcher is parked on the old handle and correctly declines to act, so without this they would never auto-start.
- `Torrent`/`File` fields are read by the server and marshalled straight to the browser; treat exported field names as part of the UI contract
- Stopping is destructive: `StopTorrent` drops the underlying torrent rather than pausing it, and clears `t.t`. `StartTorrent` re-adds from the retained `spec`, so start-after-stop works.
- `GetTorrents` returns a deep copy; the internal `ts` map and its `*Torrent` values never escape the engine
- Errors are sentinels (`ErrMissingTorrent`, `ErrAlreadyStarted`, …) wrapped with `%w`; the server maps them to HTTP status codes
- Start/stop is per torrent, never per file. `anacrolix/torrent` has no per-file pause that composes with `DownloadAll`, and the engine tracks no per-file priorities, so a per-file API could only ever be a lie. `File` is a read-only progress view.
- Torrent parsing lives here, not in `server`: `anacrolix/torrent` types must not appear in exported signatures

## Work Guidance

- Every exported method takes `e.mu`; helpers suffixed `Locked` assume the caller already holds it
- `Close` cancels the metadata watchers and releases the client; the server calls it on shutdown
- Return `error` for invalid input rather than panicking — the server surfaces these directly as HTTP 400 text

## Verification

- `go build ./...`
- `go test -race ./...`

## Child DOX Index

No children.
