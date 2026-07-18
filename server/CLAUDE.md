# server

## Purpose

The process shell and every HTTP surface: flag/config handling, the velox-synced state object, the JSON-ish API, download file serving, torrent search, and system stats.

## Ownership

- `server.go` — `Options` (CLI flags), `Server` runtime, `New`, `Run`, `reconfigure`, the `route` dispatcher and the middleware chain
- `state.go` — the named `State` type, its `Update`/`Read` accessors and the `Stats` block
- `open.go` — `openBrowser`, replacing the abandoned skratchdot/open-golang
- `server_api.go` — `/api/*` actions: `url`, `magnet`, `torrentfile`, `configure`, `torrent`, `file`
- `server_files.go` — `fsNode` tree of the download directory, static/download file serving, archive downloads
- `server_search.go` — search providers: `search-config.json` embedded via `go:embed`, plus an opt-in periodic fetch of a remote scraper config
- `server_stats.go` — `SystemStats` and `sampleSystemStats`, a pure sampler over `gopsutil/v4`

## Local Contracts

- Routing in `route` is prefix-based and order-sensitive: `/js/velox.js` → `/sync` → `/search`(+`/search/`) → `/api/` → `/download/` → static files as fallback.
- `s.state` is the single source of truth pushed to browsers. Mutate it **only** through `State.Update`, which takes the lock and pushes; read through `State.Read`.
- Wire it with `velox.SyncHandler(&s.state)`, never `velox.Sync`: the latter builds a detached `State`, leaving the embedded one without a `Data` function so every `Push` is a silent no-op.
- Exported field names inside `state` are the wire format consumed by the AngularJS app — renaming one breaks the UI
- API handlers take only `*http.Request` and return `error`: nil renders `200 OK`. Non-nil is mapped to a status by `statusFor` (engine sentinels → 404/409/501/503, `apiError` carries its own). Error strings are user-visible.
- All `/api/*` writes and `DELETE /download/` require a same-origin request (`checkSameOrigin`); the bodies are `text/plain`, which browsers send cross-origin without a preflight.
- `/download/` paths must go through `resolveWithin`, which uses `filepath.Rel` plus symlink resolution — a prefix check has no separator boundary.
- `/api/url` fetches through `guardedDialContext`, which refuses loopback, private and link-local addresses.
- All API calls must be `POST`; the action is the path suffix after `/api/`
- `reconfigure` absolutizes the download directory, applies it to the engine, then writes `ConfigPath` (0600) — the engine restart happens before the file is persisted, so a failed restart persists nothing
- Three background goroutines run until the `Run` context is cancelled: torrent/file polling (1s), stats sampling (5s) and the optional search-config fetch
- `Run` shuts down gracefully on context cancellation; `Close` releases the engine
- HTTP/2 is disabled via an empty `TLSNextProto` because velox misbehaves under it. Do not re-enable it.
- TLS requires both `CertPath` and `KeyPath` or startup fails

## Work Guidance

- New CLI options are struct tags on `Options`; `jpillora/opts` derives flags, `help`, and `env` from them. Defaults live in `DefaultOptions`, not in `main`.
- The remote search-config fetch is opt-in via `--search-config-url`; it is off by default because that document dictates which hosts the server will contact. Pass the scraper a **copy** of the config bytes: its selector unmarshaler mutates the buffer.
- The scraper handler is wrapped in `readOnly` (it treats POST at its root as "replace my whole config") and `safeSearchParams` (it only escapes params that appear after `?` in a provider URL)
- `fileNumberLimit` caps the listed download tree — keep the traversal bounded

## Verification

- `go build ./...`
- `go test -race ./...`
- Manual: `go run . --port 3000` and confirm the UI connects (lightning icon turns green). To prove pushes are live rather than a single snapshot:
  `curl -sN -H 'Accept: text/event-stream' 'localhost:3000/sync?v=eventsource'` must show an increasing `version`.

## Child DOX Index

No children.
