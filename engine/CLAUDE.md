# engine

## Purpose

Wraps `anacrolix/torrent` in a small, server-friendly facade: one `*torrent.Client`, a mutable `Config`, and a cache of `*Torrent` view models keyed by hex infohash.

## Ownership

- `engine.go` — `Engine` type: client lifecycle (`Configure`), adding torrents (`NewMagnet`, `NewTorrentFile`), the `ts` cache (`GetTorrents`, `upsertLocked`), and start/stop/delete for torrents and files
- `torrent.go` — `Torrent` and `File` view models, `update` (copies live state out of the underlying torrent) and `clone` (deep copy for callers)
- `config.go` — the `Config` struct persisted by the server as JSON, plus `Validate`

## Local Contracts

- `Engine` must not import `server` or `static`; it is the bottom of the dependency chain
- Public methods take a hex infohash string; `str2ih` checks the length *before* decoding — `hex.Decode` is bounded by `len(src)`, not `len(dst)`, so an over-long input writes past the array and panics
- `Configure` validates and builds the replacement client *before* closing the old one, then re-adds every cached torrent from its retained spec. A rejected config leaves the engine untouched and still running.
- `Torrent`/`File` fields are read by the server and marshalled straight to the browser; treat exported field names as part of the UI contract
- Stopping is destructive: `StopTorrent` drops the underlying torrent rather than pausing it, and clears `t.t`. `StartTorrent` re-adds from the retained `spec`, so start-after-stop works.
- `GetTorrents` returns a deep copy; the internal `ts` map and its `*Torrent` values never escape the engine
- Errors are sentinels (`ErrMissingTorrent`, `ErrAlreadyStarted`, …) wrapped with `%w`; the server maps them to HTTP status codes
- `StopFile` is deliberately unimplemented and returns `ErrUnsupported`; there is no per-file pause that composes with `DownloadAll`
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
