# server

## Purpose

The process shell and every HTTP surface: flag/config handling, the shared state snapshot, server-side rendering of the web UI, the SSE stream that drives it, the `/api/*` command endpoints, download file serving, and system stats.

## Ownership

- `server.go` — `Options` (CLI flags), `Server` runtime, `New`, `Run`, `reconfigure`, the `route` dispatcher and the middleware chain
- `state.go` — `sampledStats` (the host sample), the `stateDocument` wire types, and `GET /api/state`
- `render_torrents.go` / `render_downloads.go` — the per-region view models and emission rules
- `fragments.go` — `servePage` and the `hx-get` fragments (`/fragments/downloads`, per-torrent file tables)
- `server_api_forms.go` — form-encoded and multipart request handling for the htmx UI
- `templates/` — `page`, `stats`, `torrent-list`/`torrent-row`/`torrent-files`, `downloads`/`fsnode`, `omni`/`config`
- `render.go` — `parseTemplates`, the template funcs, and `renderer`: per-region change detection and SSE framing
- `events.go` — `hub` (fan-out with backpressure) and `serveEvents`, the `/events` SSE endpoint
- `open.go` — `openBrowser`, replacing the abandoned skratchdot/open-golang
- Authentication and request logging are not owned here: `handler` wires in `internal/auth` and `internal/reqlog`
- `server_api.go` — `/api/*` actions: `add`, `torrentfile`, `configure`, `torrent`
- `server_files.go` — `fsNode` tree of the download directory, static/download file serving, archive downloads
- `server_stats.go` — `SystemStats` and `sampleSystemStats`, a pure sampler over `gopsutil/v4`

## Local Contracts

- Routing in `route` is prefix-based and order-sensitive: `/events` → `/api/state` → `/` (the page) → `/fragments/` → `/api/` → `/download/` → static files as fallback. A prefix route must not swallow paths that merely begin with the same characters (`/nextdoor` is not `/next`); `TestRouting` pins this.

Server-rendered / SSE path:

- Templates are addressed **only** by their `{{define}}` name. `template.ParseFS` names by base filename, so two files named `row.html` in different directories silently collide, and requesting a name no file provides yields an empty template that renders nothing without erroring.
- `parseTemplates` returns its error rather than using `template.Must`: the recursive tree template can fail the contextual autoescaper with `ErrOutputContext` at *parse* time, and a package-level `Must` would turn a template edit into a startup panic.
- **Every fragment must be wrapped in an element.** Verified in Chromium 150 with htmx 2.0.10 + idiomorph 0.7.4: a bare-text payload swapped with `hx-swap="morph:…"` lands as an *empty* target, with no error anywhere. `checkFragment` rejects it at render time; `TestFragmentsAreWrappedInElements` runs it over every shipped template.
- Change detection compares rendered bytes, not source data — comparing data means maintaining an `Equal` per view model whose failure mode is a silently stale UI. Rendered bodies are retained (not just hashed) because a client connecting mid-tick must get every region's current body immediately.
- Regions are rendered **once per tick** and the same `[]byte` is fanned out; never render per client.
- `hub.broadcast` never blocks. A subscriber whose buffer is full is disconnected rather than having the frame dropped: frames carry only what changed, so a dropped frame leaves that browser permanently stale, while a disconnect is self-correcting via EventSource's reconnect plus the snapshot replay.
- `hub.close` reuses that disconnect mechanism for shutdown, but means something different and must stay distinct. It also **evicts** (the reader may already be gone, so nobody will call `unsubscribe`) and it **latches**, so a request arriving after it subscribes to an already-released subscriber. Nothing self-corrects here — the server is going away — and there is deliberately no farewell event: htmx owns the `EventSource`, so telling the page to stop retrying means driving unexported internals, whose failure mode is a permanently dead UI.
- The SSE stream must be excluded from gzip. `gzhttp` buffers until 1 KiB before deciding whether to compress, so without the `text/event-stream` exception the first event never reaches the browser. `TestEventsArriveImmediately` pins this.
- When a region disappears, emit one final empty event for its name before dropping it (`renderer.forget`). htmx's SSE extension unregisters per-element listeners lazily from inside the listener, so a name that simply stops being emitted leaks the listener and its detached DOM subtree.
- Do not emit an item's first event in the same instant as the membership event that creates its element; leave a tick. Observed in-browser: at 300 ms the item event was missed, at 600 ms it landed.
- The poll loop is gated on `watchers() > 0` — `listFiles` costs up to `fileNumberLimit` stat calls per second and is pure waste with nobody connected.
- **The server stores one thing: the latest host sample (`sampledStats`).** Everything else the UI and `/api/state` show is derived and read from its owner at the moment it is needed — torrents from `engine.GetTorrents`, the tree from `listFiles`, the config from `engine.Config`, the viewer count from `hub.count`. Do not reintroduce a shared snapshot: the previous one was written by the poll loop and read by nothing but the JSON encoder, so because polling is gated on `watchers() > 0`, `/api/state` served `null` torrents whenever no browser was connected. `TestStateIsLiveWithoutWatchers` pins this.
- The config lives in the engine and nowhere else. The server persists it to `ConfigPath` but keeps no copy — two copies can disagree, and the one the settings form renders would be the stale one.
- `stateDocument`/`statsDocument` are the JSON contract of `/api/state`, declared explicitly so that rearranging the server's own fields cannot silently change the wire format. Exported field names are the contract; renaming one breaks any script consuming it. `Config` is deliberately absent — it is the engine's, and republishing it here is what created the second copy.
- `/api/state` costs one bounded directory walk per request (`fileNumberLimit`), the same one the poll loop does each second while anyone is watching. That is the price of a document that is correct for a caller who is not a browser.
- API handlers take only `*http.Request` and return `error`: nil renders `200 OK`. Non-nil is mapped to a status by `statusFor` (engine sentinels → 404/409/501/503, `apiError` carries its own). Error strings are user-visible.
- All `/api/*` writes and `DELETE /download/` require a same-origin request (`checkSameOrigin`). Bodies may be `text/plain`, form-encoded or multipart; browsers send the first two cross-origin without a preflight, which is what makes the check necessary.
- Each action accepts exactly one encoding, and adding a second to any of them means a second parser to keep in step with the same struct. `add` takes a bare string, `configure` and `torrent` take a form, `torrentfile` takes raw bytes or multipart.
- When `HX-Request` is set, API responses are HTML fragments with status 200 — htmx does not swap non-2xx. Status codes stay intact for every other client.
- **Any path rendered into a URL attribute must go through the `urlpath` template func.** `html/template` only normalizes attributes it recognises as URLs (`href`, `src`); an htmx attribute like `hx-delete` is plain text to it, so a file named `a#b.mkv` produced a request for `/download/a` and deleted a *different* file with a 200. File names come from torrents, so this is attacker-reachable.
- Rendering is serialized by `renderMu`: `pollLoop` and `statsLoop` both render, and unsynchronized they can broadcast samples out of order. `seenTorrents` is covered by it.
- The SSE stream sets a per-write deadline. There is no server-wide `WriteTimeout` (the stream and large downloads are both long-lived), and a blocked `Write` is not unblocked by request-context cancellation — without it a dead client keeps a subscriber slot and the poll loop walking the download directory forever.
- That deadline is set through an `http.ResponseController`, so **every middleware wrapping the `ResponseWriter` must implement `Unwrap`**, and any that a streaming path passes through must implement `Flush`. `serveEvents` type-asserts `http.Flusher` and 500s if it fails, while a missing `Unwrap` fails silently: `SetWriteDeadline` returns `ErrNotSupported` and the stream runs with no timeout. This is exactly what `--log` did before `internal/reqlog`.
- The handler chain is, outermost first: `reqlog` (if `--log`) → `securityHeaders` → `auth` (if `--auth`) → `gzip` → `route`. Authentication sits inside the security headers and outside gzip so a 401 is never compressed and never misses its headers.
- Multipart uploads are capped with `http.MaxBytesReader`. `ParseMultipartForm` bounds only what is buffered in RAM; the rest spills to temp files with no limit.
- `/download/` paths must go through `resolveWithin`, which uses `filepath.Rel` plus symlink resolution — a prefix check has no separator boundary.
- `/api/add` fetches an `http(s)` URL through `guardedDialContext`, which refuses loopback, private and link-local addresses. Filtering happens at dial time, so it also covers redirects and DNS rebinding.
- All API calls must be `POST`; the action is the path suffix after `/api/`
- `reconfigure` absolutizes the download directory, applies it to the engine, then writes `ConfigPath` (0600) — the engine restart happens before the file is persisted, so a failed restart persists nothing
- Two background goroutines run until the `Run` context is cancelled: torrent/file polling (1s) and stats sampling (5s). Polling is gated on `watchers() > 0`. `Run` joins both before returning, so no engine call is in flight when `Close` releases the engine.
- Shutdown order is load-bearing: cancel the context → `hub.close()` → `srv.Shutdown` → join the loops → (`main`) `engine.Close`. **`hub.close` must come before `srv.Shutdown`.** `Shutdown` waits for connections to become idle and does not cancel request contexts, so an `/events` handler parked in its select is never released by it — with one browser connected that burned the entire shutdown budget and exited non-zero. `TestRunShutsDownPromptlyWithSSEClients` pins it end to end, `TestHubCloseReleasesSubscribers` pins the mechanism.
- `Run` returns nil for any *completed* shutdown, including one that overran its drain budget: a requested stop is not a failed run, and `main` calls `log.Fatal` on a non-nil error. Only a genuine serving failure (bind, TLS) returns one. If the drain does overrun, `srv.Close` stops waiting.
- `Run` is one-shot — the hub latches closed, so a second call would serve no events. `Close` releases the engine.
- TLS requires both `CertPath` and `KeyPath` or startup fails

## Work Guidance

- A new CLI option is a field on `Options` plus a registration line in `main`, using `internal/cli`. Defaults live in `DefaultOptions`, not in `main`. `Options` carries no struct tags — the flag names, shorthands and env vars are declared at the registration site.
- `fileNumberLimit` caps the listed download tree — keep the traversal bounded

## Verification

- `go build ./...`
- `go test -race ./...`
- Manual: `curl -sN localhost:3000/events` must show named events arriving, and must fall silent on an idle server — continuous output means change detection is broken.
- `curl -s localhost:3000/api/state | jq .` must still return the full state document.

## Child DOX Index

No children.
