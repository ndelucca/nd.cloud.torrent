# server

## Purpose

The process shell and the HTTP surface: flags, the middleware chain, the route dispatcher, the
`/api/*` command endpoints, `/api/state`, host stats, and the two background loops. Rendering, file
serving and the remote fetch are delegated to `web`, `files`, `fetch` and `sysstat`.

## Ownership

- `server.go` — package doc, interval consts, `Options`/`DefaultOptions`, the `Server` runtime,
  `downloadDir`, `New`, `Close`
- `run.go` — `Run` and its shutdown sequence
- `loops.go` — `kick`, `pollLoop`, `statsLoop`, `watchers`, `renderStats`
- `routes.go` — the `routes()` table and the middleware chain (`handler`, `gzip`,
  `requireSameOrigin`, `securityHeaders`)
- `config.go` — `applyConfig` and `reconfigure`, the two that face the engine
- `api.go` — the `/api/*` handlers (`handleAdd`, `handleTorrentFile`, `handleConfigure`,
  `handleStart`/`handleStop`/`handleDelete`) plus `apiHandler`, `apiRoute`, `finishAPI`, `readBody`
- `errors.go` — `apiError`, `badRequest`, `classify`, `sentence`, `checkSameOrigin`
- `forms.go` — `addURI`, `addUploadedTorrents`, `parseConfig`: form and multipart handling for htmx
- `state.go` — `sampledStats`, the `stateDocument`/`statsDocument` wire types, `GET /api/state`
- `open.go` — `openBrowser`
- Not owned here: rendering and SSE (`web`), the download tree and file serving (`files`), the remote
  `.torrent` fetch (`fetch`), host sampling (`sysstat`), auth and request logging (`internal/`)

## Local Contracts

Routing and middleware:

- **Routing is a `ServeMux` pattern table in `routes()`.** Order is irrelevant — most specific wins.
  `GET /{$}` is the exact root, `GET` patterns also match `HEAD`, and a wrong method on a declared
  path yields 405 with an `Allow` header, so handlers carry no method guards and path parameters are
  read with `r.PathValue`.
- **Embedded assets are mounted at `/css/`, `/js/` and `/cloud-favicon.png`, not behind a catch-all
  `/`.** A catch-all matches every unrouted path, so `ServeMux` could never answer 405 — a
  `GET /api/add` would reach the file server and 404. The cost is a line in `routes()` per new asset
  directory.
- Chain, outermost first: `reqlog` (if `--log`) → `securityHeaders` → `auth` (if `--auth`) → `gzip` →
  `requireSameOrigin` → the mux.
- **Authentication sits inside the security headers**, so a 401 still carries `nosniff`, `DENY` and
  `no-referrer`.
- **The SSE stream must be excluded from gzip.** `gzhttp` buffers until 1 KiB before deciding whether
  to compress and an SSE frame is smaller, so without the `text/event-stream` exception the first
  event never reaches the browser.
- Any middleware wrapping the `ResponseWriter` must implement `Unwrap`, and any a streaming path
  passes through must implement `Flush`. See `web/CLAUDE.md` for why a missing `Unwrap` fails
  silently.

Authorization:

- **`requireSameOrigin` wraps the whole mux and gates by method: anything that is not GET or HEAD
  must be same-origin.** Bodies may be `text/plain`, form-encoded or multipart, and browsers send the
  first two cross-origin without a preflight. As middleware it covers a mutating route *before* it is
  written. A rejected `/api/*` call gets a hard 403, not a fragment, so htmx will not swap it.
- **This package is the only place authorization is decided.** Nothing in `files` performs a check;
  `files.Remove` deletes for anyone who calls it.
- **`DELETE /download/{path...}` is its own route through `apiRoute`**, ahead of the method-less
  `/download/` prefix — most-specific-wins, and a method-bearing pattern beats one without, so GET
  and HEAD still reach `files.Handler`. It exists so a delete gets the render kick, `classify`'s
  status mapping and an `api-ok`/`api-error` fragment. `files.Handler` answered it with a 200 and an
  empty body on success, which htmx swapped into the tree panel and blanked it, and a 500 on
  failure, which htmx does not swap at all.

The API:

- Handlers have the type `apiHandler func(http.ResponseWriter, *http.Request) error`. The
  `ResponseWriter` is there for the multipart path, which needs it for `http.MaxBytesReader`. nil
  renders `200 OK`; non-nil goes to `classify`.
- **Each action is its own route with its own handler.** `apiRoute` adapts one to an `http.Handler`;
  `finishAPI` applies the kick, the htmx fragment rendering and the status mapping in one place.
- The render loop is kicked after **every** API call, not only successful ones: an action can apply
  partially and still report an error — five uploaded torrents where two are malformed adds three and
  returns 400 — and gating on success leaves those three invisible until the next tick. `kick` is
  coalesced and floored, so a needless one costs at most one extra render.
- **`classify` decides both status and message, and its axis is "did what the caller sent cause
  this?"** — not which package produced the error.
  - *Input* (magnet URI, remote URL, `.torrent` bytes, config value): the wrapped detail is the only
    useful information and is bounded parser prose, so it is shown. → 400/404/409.
  - *Operational* (disk, bind, upstream, closed): the detail is a syscall string and a
    filesystem-layout oracle, so a fixed message is shown and the chain is logged. → 500/502/503.
  - *Path* (`files.ErrOutsideRoot`, `fs.ErrNotExist`): caller-caused, so 404 — but with a **fixed**
    message unlike the other input cases. `ErrOutsideRoot`'s own text and the path `EvalSymlinks`
    attaches are layout oracles, so a refused traversal must not be tellable from a missing file.
  - **The default is 500**, so an unclassified failure is never reported to the user as their own
    mistake. `engine.ErrInvalidInput` keeps genuine caller mistakes on the 400 side of it.
  - `engine.ErrRestartRequired` maps to **200**: the request succeeded and there is something worth
    saying. `finishAPI` therefore decides success from the *status*, not from `err != nil`, and
    `web.WriteAPIResult` takes an explicit `ok` rather than inferring it from an empty message.
- **Error strings stay ordinary lowercase Go.** `classify` owns presentation; `sentence` capitalises
  what it shows. `web.WriteAPIResult` takes the *message*, not the error — deciding what a failure
  says, and what it must not say, is the server's job.
- Torrent verbs are `POST /api/torrents/{hash}/start`, `.../stop` and `DELETE /api/torrents/{hash}`;
  the hash is a path parameter, so none of them takes a body.
- `add` takes a bare string or a `uri` form field, `configure` a form, `torrentfile` raw bytes or
  multipart. A second encoding on any of them means a second parser to keep in step with one struct.
- **`/api/configure` holds `configMu` across read-merge-apply-persist**, and merges over
  `s.desired`, not over `engine.Config()`. The engine serializes its own apply but not the read the
  merge starts from, so without one lock two concurrent saves each begin from the same config and the
  second silently undoes the first. The merge base matters just as much: most settings need a
  restart, so the *live* config does not advance when one is saved, and merging over it would drop
  every pending change on the next save.
- When `HX-Request` is set, responses are HTML fragments with status 200 — htmx does not swap
  non-2xx. Status codes stay intact for every other client.
- Multipart uploads are capped with `http.MaxBytesReader` (`maxUploadBody`). `ParseMultipartForm`
  bounds only what is buffered in RAM; the rest spills to temp files with no limit.

State:

- **The server stores one thing: the latest host sample (`sampledStats`).** Everything else the UI
  and `/api/state` show is read from its owner when needed — torrents from `engine.GetTorrents`, the
  tree from `files.List`, the config from `engine.Config`, viewers from `web.UI.Watchers`. Do not
  reintroduce a shared snapshot: written by the watcher-gated poll loop, it makes `/api/state` serve
  `null` torrents whenever no browser is connected.
- **There are two configurations and they are different facts, not two copies of one.**
  `engine.Config()` is what is *running*; `s.desired` (guarded by `configMu`) is what the user has
  *asked for*, which is what the file holds and what the settings form renders. Most settings are
  fixed for the lifetime of a torrent client, so after saving one the two legitimately differ until a
  restart. Rendering the live config in the form would show the old value straight back after a save
  that did work.
- `desired` is updated only *after* `configfile.Save` succeeds, so it never claims something the file
  does not hold.
- `stateDocument`/`statsDocument` are the JSON contract of `/api/state`, declared explicitly so
  rearranging the server's own fields cannot change the wire format. Exported field names are the
  contract. `Config` is deliberately absent — it is the engine's.
- `/api/state` costs one bounded directory walk per request (`files.Limit`), the same one the poll
  loop does each second while anyone is watching. That buys a document correct for a non-browser
  caller.
- The server owns *when* the host is sampled, not the sample's shape: `sysstat.Stats` passes through
  unchanged.

Lifecycle:

- Two goroutines run until the `Run` context is cancelled. **The render loop has no clock of its
  own: it waits on `engine.Sampled()`**, so a render always follows a fresh sample instead of
  drifting against one — two independent 1 Hz timers meant a render could show a sample up to a
  second stale. Only the stats loop keeps a ticker (`statsInterval`, 5s), and it must: `statsInterval`
  is the `cpu.Percent` measurement window, a different quantity from the engine's.
  Rendering is gated on `watchers() > 0` **because of `files.List`** — up to `files.Limit` stat calls
  per second — and because rendering for nobody is waste. Torrent freshness does not ride on that
  gate: the engine samples on its own cadence and `GetTorrents` is a pure read.
- **The host is sampled unconditionally; only the *render* is gated on watchers.** `cpu.Percent(0, …)`
  measures since the previous call anywhere in the process, so the interval *is* the measurement
  window — which is also why `statsInterval` must stay fixed. Gating the sample would make the first
  reading after an idle spell an average over however long nobody was connected, reported as
  trustworthy.
- Shutdown order is load-bearing: cancel the context → `ui.Close()` → `srv.Shutdown` → join the loops
  → (`main`) `engine.Close`. **`ui.Close` must come first.** `Shutdown` waits for connections to go
  idle and does not cancel request contexts, so an `/events` handler parked in its select is never
  released by it and one connected browser burns the whole budget. Joining the loops means no engine
  call is in flight when `Close` releases the engine.
- `Run` returns nil for any *completed* shutdown, including one that overran its drain budget — a
  requested stop is not a failed run, and `main` calls `log.Fatal` on a non-nil error. Only a genuine
  serving failure (bind, TLS) returns one; if the drain overruns, `srv.Close` stops waiting.
- `Run` is one-shot: the hub latches closed, so a second call would serve no events.
- `reconfigure` is `applyConfig`, then `configfile.Save`, then update `desired`; callers hold
  `configMu`. `applyConfig` absolutizes the download directory and hands the config to the engine. A
  rejected config persists nothing — **except `engine.ErrRestartRequired`**, which is saved and
  passed up, because refusing to save a setting the engine cannot apply live would leave no way to
  change the listen port at all short of editing the file by hand.
- **Startup applies but never saves.** `New` calls `applyConfig` alone; rewriting the config on every
  boot is a chance to corrupt it that buys nothing.
- **Reading and writing the config file belongs to `configfile`**, atomic write included. This
  package decides *when* to load and save, not how.
- **`configMu` stays here, not in `configfile` or `engine`.** It serializes a four-step transaction —
  read the desired config, merge a form over it, apply, persist — and a lock belongs with the widest
  thing it serializes. `configfile` never reads the engine so it cannot cover the read; the engine
  covers only the apply, and "overlay a form onto the desired config" is an HTTP-layer transaction.
- **Port validity is `engine.Config.Validate`'s call and nowhere else.** `configfile.Load` does not
  clamp: it unmarshals over a defaults struct, so an absent port already keeps the default and a
  clamp could only fire on a value someone explicitly wrote. Two policies for one rule end up
  disagreeing.
- TLS requires both `CertPath` and `KeyPath` or startup fails.

## Work Guidance

- A new CLI option is a field on `Options` plus a registration line in `main`, via `internal/cli`.
  Defaults live in `DefaultOptions`. `Options` carries no struct tags — flag names, shorthands and
  env vars are declared at the registration site.
- Anything that produces HTML belongs in `web`. `grep -rn "html/template" server/*.go` returning
  nothing is the check.
- `web.KnownRoutes` and `web.StaticAssets` are asserted against the real mux here. With `web`'s
  template scan they are the only thing tying an `hx-get` attribute to a route that answers it, so
  keep both lists in step when a route or asset moves.

## Verification

- `go build ./...`
- `go test -race ./...`
- `curl -s localhost:3000/api/state | jq .` must return the full document with no browser connected.

## Child DOX Index

No children.
