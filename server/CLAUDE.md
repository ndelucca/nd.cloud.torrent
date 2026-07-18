# server

## Purpose

The process shell and every HTTP surface: flag/config handling, the velox-synced state object, the JSON-ish API, download file serving, torrent search, and system stats.

## Ownership

- `server.go` — `Server` struct (CLI flags + shared `state`), `Run`, `reconfigure`, and the `handle` router
- `server_api.go` — `/api/*` actions: `url`, `magnet`, `torrentfile`, `configure`, `torrent`, `file`
- `server_files.go` — `fsNode` tree of the download directory, static/download file serving, archive downloads
- `server_search.go` — search provider config: embedded default plus a periodic fetch of a remote scraper config
- `server_stats.go` — `stats` struct sampled from `gopsutil` (CPU, disk, memory, goroutines)

## Local Contracts

- Routing in `handle` is prefix-based and order-sensitive: `/js/velox.js` → `/sync` → `/search` → `/api/` → static files as fallback. Anything unmatched is treated as a static file request.
- `s.state` is the single source of truth pushed to browsers. Every mutation must be followed by `s.state.Push()`, and concurrent writers must hold `s.state.Mutex`.
- Exported field names inside `state` are the wire format consumed by the AngularJS app — renaming one breaks the UI
- API handlers take only `*http.Request` and return `error`: nil renders `200 OK`, non-nil renders `400` with the raw error text. Error strings are user-visible.
- All API calls must be `POST`; the action is the path suffix after `/api/`
- `reconfigure` absolutizes the download directory, applies it to the engine, then writes `ConfigPath` — the engine restart happens before the file is persisted
- Two background goroutines run for the process lifetime: torrent/file polling every 1s and stats sampling every 5s
- HTTP/2 is disabled via an empty `TLSNextProto` because velox misbehaves under it. Do not re-enable it.
- TLS requires both `CertPath` and `KeyPath` or startup fails

## Work Guidance

- New CLI options are struct tags on `Server`; `jpillora/opts` derives flags, `help`, and `env` from them
- `server_search.go` reaches out to a hardcoded gist URL on a backoff loop; failures are intentionally non-fatal
- `fileNumberLimit` caps the listed download tree — keep the traversal bounded

## Verification

- `go build ./...`
- `go test ./...`
- Manual: `go run . --port 3000` and confirm the UI connects (lightning icon turns green, meaning `/sync` is live)

## Child DOX Index

No children.
