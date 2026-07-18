# engine

## Purpose

Wraps `anacrolix/torrent` in a small, server-friendly facade: one `*torrent.Client`, a mutable `Config`, and a cache of `*Torrent` view models keyed by hex infohash.

## Ownership

- `engine.go` — `Engine` type: client lifecycle (`Configure`), adding torrents (`NewMagnet`, `NewTorrent`), the `ts` cache (`GetTorrents`, `upsertTorrent`), and start/stop/delete for torrents and files
- `torrent.go` — `Torrent` and `File` view models plus `Update`, which copies live state out of the underlying torrent
- `config.go` — the `Config` struct persisted by the server as JSON

## Local Contracts

- `Engine` must not import `server` or `static`; it is the bottom of the dependency chain
- Public methods take a hex infohash string; `str2ih` validates it is 20 decoded bytes
- `Configure` closes and replaces the existing client — it is a full restart, not a patch. Callers must expect in-flight torrents to be dropped.
- `Torrent`/`File` fields are read by the server and marshalled straight to the browser; treat exported field names as part of the UI contract
- Stopping is destructive: `StopTorrent` drops the underlying torrent rather than pausing it
- `StopFile` is deliberately unimplemented and returns an error

## Work Guidance

- Guard shared state with `e.mut`; `GetTorrents` is polled once per second by the server
- Return `error` for invalid input rather than panicking — the server surfaces these directly as HTTP 400 text

## Verification

- `go build ./...`
- `go test ./...`

## Child DOX Index

No children.
