# server

## Purpose

The process shell and every HTTP surface: flag/config handling, the velox-synced state object, the JSON-ish API, download file serving, torrent search, and system stats.

## Ownership

- `server.go` — `Options` (CLI flags), `Server` runtime, `New`, `Run`, `reconfigure`, the `route` dispatcher and the middleware chain
- `state.go` — the named `State` type, its `Update`/`Read` accessors and the `Stats` block
- `render.go` — `parseTemplates`, the template funcs, and `renderer`: per-region change detection and SSE framing
- `events.go` — `hub` (fan-out with backpressure) and `serveEvents`, the `/events` SSE endpoint
- `templates/` — `html/template` fragments, one `{{define}}` per SSE region
- `open.go` — `openBrowser`, replacing the abandoned skratchdot/open-golang
- `server_api.go` — `/api/*` actions: `url`, `magnet`, `torrentfile`, `configure`, `torrent`, `file`
- `server_files.go` — `fsNode` tree of the download directory, static/download file serving, archive downloads
- `server_search.go` — search providers: `search-config.json` embedded via `go:embed`, plus an opt-in periodic fetch of a remote scraper config
- `server_stats.go` — `SystemStats` and `sampleSystemStats`, a pure sampler over `gopsutil/v4`

## Local Contracts

- Routing in `route` is prefix-based and order-sensitive: `/js/velox.js` → `/sync` → `/events` → `/search`(+`/search/`) → `/api/` → `/download/` → static files as fallback.

Server-rendered / SSE path (replacing velox; both run side by side during the migration):

- Templates are addressed **only** by their `{{define}}` name. `template.ParseFS` names by base filename, so two files named `row.html` in different directories silently collide, and requesting a name no file provides yields an empty template that renders nothing without erroring.
- `parseTemplates` returns its error rather than using `template.Must`: the recursive tree template can fail the contextual autoescaper with `ErrOutputContext` at *parse* time, and a package-level `Must` would turn a template edit into a startup panic.
- **Every fragment must be wrapped in an element.** Verified in Chromium 150 with htmx 2.0.10 + idiomorph 0.7.4: a bare-text payload swapped with `hx-swap="morph:…"` lands as an *empty* target, with no error anywhere. `checkFragment` rejects it at render time; `TestFragmentsAreWrappedInElements` runs it over every shipped template.
- Change detection compares rendered bytes, not source data — comparing data means maintaining an `Equal` per view model whose failure mode is a silently stale UI. Rendered bodies are retained (not just hashed) because a client connecting mid-tick must get every region's current body immediately.
- Regions are rendered **once per tick** and the same `[]byte` is fanned out; never render per client.
- `hub.broadcast` never blocks. A subscriber whose buffer is full is disconnected rather than having the frame dropped: frames carry only what changed, so a dropped frame leaves that browser permanently stale, while a disconnect is self-correcting via EventSource's reconnect plus the snapshot replay.
- The SSE stream must be excluded from gzip. `gzhttp` buffers until 1 KiB before deciding whether to compress, so without the `text/event-stream` exception the first event never reaches the browser. `TestEventsArriveImmediately` pins this.
- When a region disappears, emit one final empty event for its name before dropping it (`renderer.forget`). htmx's SSE extension unregisters per-element listeners lazily from inside the listener, so a name that simply stops being emitted leaks the listener and its detached DOM subtree.
- Do not emit an item's first event in the same instant as the membership event that creates its element; leave a tick. Observed in-browser: at 300 ms the item event was missed, at 600 ms it landed.
- The poll loop is gated on `watchers() > 0` — `listFiles` costs up to `fileNumberLimit` stat calls per second and is pure waste with nobody connected.
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
- Manual, velox path: `go run . --port 3000` and confirm the UI connects (lightning icon turns green). To prove pushes are live rather than a single snapshot:
  `curl -sN -H 'Accept: text/event-stream' 'localhost:3000/sync'` must show an increasing `version`.
- Manual, SSE path: `curl -sN localhost:3000/events` must show named events arriving, and must fall silent on an idle server — continuous output means change detection is broken.

## Child DOX Index

No children.
