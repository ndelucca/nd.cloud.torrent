# server

## Purpose

The process shell and the HTTP surface: flags, config, the middleware chain, the
route dispatcher, the `/api/*` command endpoints, `/api/state`, host stats, and
the two background loops. Rendering, file serving and the remote fetch are
delegated to `web`, `files`, `fetch` and `sysstat`.

## Ownership

- `server.go` — `Options` (CLI flags), the `Server` runtime, `New`, `Run`, `reconfigure`, `renderStats`, the `route` dispatcher and the middleware chain
- `state.go` — `sampledStats` (the host sample), the `stateDocument` wire types, and `GET /api/state`
- `server_api.go` — `/api/*` actions (`add`, `torrentfile`, `configure`, `torrent`), `apiError`/`statusFor`, `checkSameOrigin`
- `server_api_forms.go` — form-encoded and multipart request handling for the htmx UI
- `open.go` — `openBrowser`, replacing the abandoned skratchdot/open-golang
- Not owned here: rendering and the SSE stream (`web`), the download tree and file serving (`files`), the remote `.torrent` fetch (`fetch`), host sampling (`sysstat`), authentication and request logging (`internal/auth`, `internal/reqlog`)

## Local Contracts

Routing and middleware:

- Routing in `route` is prefix-based and order-sensitive: `/events` → `/api/state` → `/` (the page) → `/fragments/` → `/api/` → `/download/` → static files as fallback. A prefix route must not swallow paths that merely begin with the same characters (`/nextdoor` is not `/next`); `TestRouting` pins this.
- The handler chain is, outermost first: `reqlog` (if `--log`) → `securityHeaders` → `auth` (if `--auth`) → `gzip` → `route`. Authentication sits inside the security headers and outside gzip so a 401 is never compressed and never misses its headers.
- The SSE stream must be excluded from gzip. `gzhttp` buffers until 1 KiB before deciding whether to compress, so without the `text/event-stream` exception the first event never reaches the browser. `TestEventsArriveImmediately` pins this.
- Any middleware wrapping the `ResponseWriter` must implement `Unwrap`, and any a streaming path passes through must implement `Flush`. See `web/CLAUDE.md` for why a missing `Unwrap` fails silently.

Authorization:

- All `/api/*` writes and `DELETE /download/` require a same-origin request (`checkSameOrigin`). Bodies may be `text/plain`, form-encoded or multipart; browsers send the first two cross-origin without a preflight, which is what makes the check necessary.
- **This package is the only place authorization is decided.** `files.Handler` performs no checks and will delete for anyone who reaches it, so `serveDownload` gates `DELETE` before delegating. Adding a mutating route that bypasses this is how that becomes a bug.

The API:

- API handlers take only `*http.Request` and return `error`: nil renders `200 OK`. Non-nil is mapped to a status by `statusFor` (engine and `fetch` sentinels → 404/409/502/503, `apiError` carries its own). Error strings are user-visible.
- Each action accepts exactly one encoding, and adding a second means a second parser to keep in step with the same struct. `add` takes a bare string (or a `uri` form field), `configure` and `torrent` take a form, `torrentfile` takes raw bytes or multipart.
- When `HX-Request` is set, API responses are HTML fragments with status 200 — htmx does not swap non-2xx. Status codes stay intact for every other client.
- All API calls must be `POST`; the action is the path suffix after `/api/`.
- Multipart uploads are capped with `http.MaxBytesReader`. `ParseMultipartForm` bounds only what is buffered in RAM; the rest spills to temp files with no limit.

State:

- **The server stores one thing: the latest host sample (`sampledStats`).** Everything else the UI and `/api/state` show is derived and read from its owner at the moment it is needed — torrents from `engine.GetTorrents`, the tree from `files.List`, the config from `engine.Config`, the viewer count from `web.UI.Watchers`. Do not reintroduce a shared snapshot: the previous one was written by the poll loop and read by nothing but the JSON encoder, so because polling is gated on `watchers() > 0`, `/api/state` served `null` torrents whenever no browser was connected. `TestStateIsLiveWithoutWatchers` pins this.
- The config lives in the engine and nowhere else. The server persists it to `ConfigPath` but keeps no copy — two copies can disagree, and the one the settings form renders would be the stale one.
- `stateDocument`/`statsDocument` are the JSON contract of `/api/state`, declared explicitly so that rearranging the server's own fields cannot silently change the wire format. Exported field names are the contract; renaming one breaks any script consuming it. `Config` is deliberately absent — it is the engine's, and republishing it here is what created the second copy.
- `/api/state` costs one bounded directory walk per request (`files.Limit`), the same one the poll loop does each second while anyone is watching. That is the price of a document that is correct for a caller who is not a browser.
- The server owns *when* the host is sampled, not the sample's shape: it stores the latest `sysstat.Stats` and passes it through to both `/api/state` and the UI unchanged. It keeps no copy of its own — that copy existed once and had to be updated in lockstep by hand.

Lifecycle:

- Two background goroutines run until the `Run` context is cancelled: torrent/file polling (1s) and stats sampling (5s). Polling is gated on `watchers() > 0` — the walk costs up to `files.Limit` stat calls per second and is pure waste with nobody connected. `Run` joins both before returning, so no engine call is in flight when `Close` releases the engine.
- Shutdown order is load-bearing: cancel the context → `ui.Close()` → `srv.Shutdown` → join the loops → (`main`) `engine.Close`. **`ui.Close` must come before `srv.Shutdown`.** `Shutdown` waits for connections to become idle and does not cancel request contexts, so an `/events` handler parked in its select is never released by it — with one browser connected that burned the entire shutdown budget and exited non-zero. `TestRunShutsDownPromptlyWithSSEClients` pins it end to end, `web.TestHubCloseReleasesSubscribers` pins the mechanism.
- `Run` returns nil for any *completed* shutdown, including one that overran its drain budget: a requested stop is not a failed run, and `main` calls `log.Fatal` on a non-nil error. Only a genuine serving failure (bind, TLS) returns one. If the drain does overrun, `srv.Close` stops waiting.
- `Run` is one-shot — the hub latches closed, so a second call would serve no events. `Close` releases the engine.
- `reconfigure` absolutizes the download directory, applies it to the engine, then writes `ConfigPath` (0600) — the engine restart happens before the file is persisted, so a failed restart persists nothing.
- TLS requires both `CertPath` and `KeyPath` or startup fails.

## Work Guidance

- A new CLI option is a field on `Options` plus a registration line in `main`, using `internal/cli`. Defaults live in `DefaultOptions`, not in `main`. `Options` carries no struct tags — the flag names, shorthands and env vars are declared at the registration site.
- Anything that produces HTML belongs in `web`, not here. `grep -rn "html/template" server/*.go` returning nothing is the check.
- `statsInterval` must stay fixed: `cpu.Percent(0, …)` measures since the previous call, so the interval *is* the measurement window.

## Verification

- `go build ./...`
- `go test -race ./...`
- `curl -s localhost:3000/api/state | jq .` must return the full document with no browser connected.

## Child DOX Index

No children.
