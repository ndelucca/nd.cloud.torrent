# server

## Purpose

The process shell and the HTTP surface: flags, config, the middleware chain, the
route dispatcher, the `/api/*` command endpoints, `/api/state`, host stats, and
the two background loops. Rendering, file serving and the remote fetch are
delegated to `web`, `files`, `fetch` and `sysstat`.

## Ownership

- `server.go` — `Options` (CLI flags), the `Server` runtime, `New`, `Run`, `applyConfig`/`saveConfig`/`reconfigure`, `renderStats`, the `routes()` table and the middleware chain (`requireSameOrigin`, `securityHeaders`, `gzip`)
- `state.go` — `sampledStats` (the host sample), the `stateDocument` wire types, and `GET /api/state`
- `server_api.go` — the `/api/*` handlers (`handleAdd`, `handleTorrentFile`, `handleConfigure`, `handleStart`/`handleStop`/`handleDelete`), `apiHandler`/`apiRoute`/`finishAPI`, `apiError`/`classify`/`sentence`, `checkSameOrigin`
- `server_api_forms.go` — form-encoded and multipart request handling for the htmx UI
- `open.go` — `openBrowser`, replacing the abandoned skratchdot/open-golang
- Not owned here: rendering and the SSE stream (`web`), the download tree and file serving (`files`), the remote `.torrent` fetch (`fetch`), host sampling (`sysstat`), authentication and request logging (`internal/auth`, `internal/reqlog`)

## Local Contracts

Routing and middleware:

- **Routing is a `ServeMux` pattern table in `routes()`.** Order is irrelevant — most specific wins — so the old warning about order-sensitive prefix dispatch is gone rather than restated. `GET /{$}` is the exact root, which is what the old switch faked by placing `r.URL.Path == "/"` above the prefix arms; `GET` patterns match `HEAD`; and a wrong method on a declared path yields 405 with an `Allow` header, which replaced three hand-written method guards.
- **The embedded assets are mounted at `/css/`, `/js/` and `/cloud-favicon.png`, not behind a catch-all `/`.** A catch-all matches every unrouted path, which means `ServeMux` can never answer 405 — a `GET /api/add` would reach the file server and 404. The cost is a line in `routes()` when a new asset directory appears.
- The handler chain is, outermost first: `reqlog` (if `--log`) → `securityHeaders` → `auth` (if `--auth`) → `gzip` → `route`.
- **Authentication sits inside the security headers**, so a 401 — the response an unauthenticated caller sees most — still carries `nosniff`, `DENY` and `no-referrer`. `TestUnauthorizedKeepsSecurityHeaders` fails when the two are swapped.
- Authentication also sits outside gzip, but do not credit that with keeping the 401 uncompressed: `gzhttp` does not compress below `DefaultMinSize` (1 KiB) and the challenge body is a few dozen bytes, so moving `auth` inside `gzip` produces a byte-identical response. This was verified by making the swap and watching `TestUnauthorizedIsNotCompressed` still pass; that test asserts the property, not the ordering, and says so.
- The SSE stream must be excluded from gzip. `gzhttp` buffers until 1 KiB before deciding whether to compress, so without the `text/event-stream` exception the first event never reaches the browser. `TestEventsArriveImmediately` pins this.
- Any middleware wrapping the `ResponseWriter` must implement `Unwrap`, and any a streaming path passes through must implement `Flush`. See `web/CLAUDE.md` for why a missing `Unwrap` fails silently.

Authorization:

- **`requireSameOrigin` wraps the whole mux and gates by method: anything that is not GET or HEAD must be same-origin.** Bodies may be `text/plain`, form-encoded or multipart; browsers send the first two cross-origin without a preflight, which is what makes the check necessary. It was previously called from two places — inside `api()` and again in a `serveDownload` DELETE branch — so the invariant held by convention and this doc had to warn that a mutating route bypassing it was how that became a bug. As middleware it covers such a route *before* it is written, and both call sites plus `serveDownload` itself were deleted.
- **This package is still the only place authorization is decided.** `files.Handler` performs no checks and will delete for anyone who reaches it.
- One consequence worth knowing: a cross-origin `/api/*` call from htmx now gets a hard 403 rather than a 200 fragment explaining the rejection, so htmx will not swap it. That is better security ergonomics and worse UX for a request that should not be happening.

The API:

- The render loop is kicked after **every** API call, not only successful ones. An action can apply partially and still report an error — an upload of five torrents where two are malformed adds three and returns 400 — and gating the kick on success left those three invisible until the next tick. `kick` is coalesced and floored, so the cost of an unnecessary one is at most a single extra render.
- API handlers take only `*http.Request` and return `error`: nil renders `200 OK`. Non-nil goes to `classify`.
- **`classify` decides both the status and the message, and its axis is "did what the caller sent cause this?"** — not which package the error came from.
  - *Input* (magnet URI, remote URL, `.torrent` bytes, a config value): the wrapped detail is the only useful information and is bounded parser prose, so it is shown. → 400/404/409.
  - *Operational* (disk, bind, upstream, closed): the wrapped detail is a syscall string and a filesystem-layout oracle, so a fixed message is shown and the chain goes to the log. → 500/502/503.
  - **The default is 500.** It was 400, which reported a disk-full or permission failure to the user as their own mistake — exactly what the function existed to prevent. `engine.ErrInvalidInput` is what keeps genuine caller mistakes on the 400 side of that default.
- **Error strings are ordinary lowercase Go, everywhere.** `classify` owns presentation: `sentence` capitalises what it decides to show. That is what let `-ST1005` come out of `staticcheck.conf` — the suppression existed because error strings doubled as UI copy, and they no longer do.
- `web.WriteAPIResult` takes the *message*, not the error. Deciding what a failure says — and what it must not say — is the server's job.
- **Each action is its own route with its own handler**, typed `apiHandler` so the contract is compiler-checked rather than documented. `apiRoute` wraps one into an `http.Handler`, applying the kick, the htmx fragment rendering and the status mapping in exactly one place.
- The torrent verbs are `POST /api/torrents/{hash}/start`, `.../stop` and `DELETE /api/torrents/{hash}`. They were one `torrent` action dispatching on an `action` *form field* inside a nested switch — three routes wearing a trenchcoat, and the reason the body had to be drained before the encoding was known. With the hash in the path they have no body at all, which deleted `formValues` and let `parseConfig` take `url.Values` from `r.ParseForm` instead of re-parsing bytes.
- `add` takes a bare string or a `uri` form field; `configure` takes a form; `torrentfile` takes raw bytes or multipart. Adding a second encoding to any of them means a second parser to keep in step with the same struct.
- **`/api/configure` holds `configMu` across read-merge-apply.** The engine's `configureMu` serializes the apply but not the read the merge starts from, so two concurrent saves each began from the same config and the second silently undid the first. `TestConcurrentConfigureKeepsBothFields` pins it.
- When `HX-Request` is set, API responses are HTML fragments with status 200 — htmx does not swap non-2xx. Status codes stay intact for every other client.
- The method is enforced by the route pattern, not by a guard inside a handler. Path parameters are read with `r.PathValue`.
- Multipart uploads are capped with `http.MaxBytesReader`. `ParseMultipartForm` bounds only what is buffered in RAM; the rest spills to temp files with no limit.

State:

- **The server stores one thing: the latest host sample (`sampledStats`).** Everything else the UI and `/api/state` show is derived and read from its owner at the moment it is needed — torrents from `engine.GetTorrents`, the tree from `files.List`, the config from `engine.Config`, the viewer count from `web.UI.Watchers`. Do not reintroduce a shared snapshot: the previous one was written by the poll loop and read by nothing but the JSON encoder, so because polling is gated on `watchers() > 0`, `/api/state` served `null` torrents whenever no browser was connected. `TestStateIsLiveWithoutWatchers` pins this.
- The config lives in the engine and nowhere else. The server persists it to `ConfigPath` but keeps no copy — two copies can disagree, and the one the settings form renders would be the stale one.
- `stateDocument`/`statsDocument` are the JSON contract of `/api/state`, declared explicitly so that rearranging the server's own fields cannot silently change the wire format. Exported field names are the contract; renaming one breaks any script consuming it. `Config` is deliberately absent — it is the engine's, and republishing it here is what created the second copy.
- `/api/state` costs one bounded directory walk per request (`files.Limit`), the same one the poll loop does each second while anyone is watching. That is the price of a document that is correct for a caller who is not a browser.
- The server owns *when* the host is sampled, not the sample's shape: it stores the latest `sysstat.Stats` and passes it through to both `/api/state` and the UI unchanged. It keeps no copy of its own — that copy existed once and had to be updated in lockstep by hand.

Lifecycle:

- Two background goroutines run until the `Run` context is cancelled: torrent/file polling (1s) and stats sampling (5s). Polling is gated on `watchers() > 0` **because of `files.List`** — the walk costs up to `files.Limit` stat calls per second and is waste with nobody connected — and because rendering for nobody is waste. Torrent freshness does not ride on that gate: the engine samples on its own cadence and `GetTorrents` is a pure read. When engine reads sampled, this gate silently doubled as the sampling schedule.
- **The host is sampled unconditionally; only the *render* is gated on watchers.** `cpu.Percent` measures since the previous call anywhere in the process, so gating the sample meant the first one after an idle spell reported the average since the last browser disconnected — possibly hours — while `Set` claimed it was trustworthy. Keeping the sample on a fixed cadence is what makes `statsInterval` actually be the measurement window. `Run` joins both before returning, so no engine call is in flight when `Close` releases the engine.
- Shutdown order is load-bearing: cancel the context → `ui.Close()` → `srv.Shutdown` → join the loops → (`main`) `engine.Close`. **`ui.Close` must come before `srv.Shutdown`.** `Shutdown` waits for connections to become idle and does not cancel request contexts, so an `/events` handler parked in its select is never released by it — with one browser connected that burned the entire shutdown budget and exited non-zero. `TestRunShutsDownPromptlyWithSSEClients` pins it end to end, `web.TestHubCloseReleasesSubscribers` pins the mechanism.
- `Run` returns nil for any *completed* shutdown, including one that overran its drain budget: a requested stop is not a failed run, and `main` calls `log.Fatal` on a non-nil error. Only a genuine serving failure (bind, TLS) returns one. If the drain does overrun, `srv.Close` stops waiting.
- `Run` is one-shot — the hub latches closed, so a second call would serve no events. `Close` releases the engine.
- `reconfigure` is `applyConfig` then `saveConfig`. `applyConfig` absolutizes the download directory and hands the config to the engine; `saveConfig` persists it. The engine restart happens first, so a failed restart persists nothing.
- **Startup applies but never saves.** `New` calls `applyConfig` alone. Rewriting the config on every boot was a chance to corrupt it that bought nothing, and it made `New`'s own doc comment false. `TestNewDoesNotWriteConfig` and `TestNewWithNoConfigCreatesNone` pin it.
- **`saveConfig` writes a sibling temp file and renames.** Write-in-place could be interrupted by a crash, a full disk or a container stop, leaving a truncated file that `loadConfig` then rejects as "Malformed configuration" — the server refused to start until someone deleted it by hand. The temp file is created in the target's directory because rename is only atomic within a filesystem, and it is `Sync`ed before the rename so the metadata cannot land ahead of the bytes.
- **Port validity is `engine.Config.Validate`'s call and nowhere else.** `loadConfig` used to silently clamp an out-of-range port to the default while `Validate` rejected the identical value. Since `loadConfig` unmarshals over a defaults struct, an absent port already keeps the default — the clamp could only fire on a value someone explicitly wrote. Two policies for one rule is how they end up disagreeing.
- TLS requires both `CertPath` and `KeyPath` or startup fails.

## Work Guidance

- A new CLI option is a field on `Options` plus a registration line in `main`, using `internal/cli`. Defaults live in `DefaultOptions`, not in `main`. `Options` carries no struct tags — the flag names, shorthands and env vars are declared at the registration site.
- Anything that produces HTML belongs in `web`, not here. `grep -rn "html/template" server/*.go` returning nothing is the check.
- `statsInterval` must stay fixed: `cpu.Percent(0, …)` measures since the previous call, so the interval *is* the measurement window.

- `TestKnownRoutesResolve` asserts every `web.KnownRoutes` entry reaches a handler on the mux. Together with `web`'s template scan it is the only thing tying an `hx-get` attribute to a route that answers it.

## Verification

- `go build ./...`
- `go test -race ./...`
- `curl -s localhost:3000/api/state | jq .` must return the full document with no browser connected.

## Child DOX Index

No children.
