# web

## Purpose

Renders the user interface and pushes it to browsers: the templates, the view
models, the SSE hub, and every handler that produces HTML.

## Ownership

- `ui.go` — `UI`, `Deps`, `New`, `Watchers`, `Close`
- `render.go` — `parseTemplates`, the template funcs, `urlPath`, the byte formatters, and `renderer`: `execute`, per-region change detection and SSE framing
- `regions.go` — the SSE region names and `KnownRoutes`, the fragment paths the templates ask for
- `events.go` — `hub` (fan-out with backpressure, `close`) and `ServeEvents`, the `/events` endpoint
- `stats.go` — `StatsData`, `statsView` (which embeds `sysstat.Stats`), `RenderStats`
- `torrents.go` — `torrentView`, `fileView`, `RenderTorrents`, and the two-tier event scheme
- `downloads.go` — `downloadsView`, `fsView`, `treeSignature`, `RenderDownloads`
- `fragments.go` — `ServePage`, `ServeDownloads`, `ServeTorrentFiles` and `WriteAPIResult`
- `templates/` — `page`, `stats`, `torrent-list`/`torrent-row`/`torrent-files`, `downloads`/`fsnode`, `omni`/`config`

## Local Contracts

Boundary:

- **`web` must never import `server`.** The standing temptation is to reach for server state; the moment that edge exists the split is undone. Everything this package needs from the outside arrives through the closures in `Deps`, over value types (`engine.Torrent`, `engine.Config`, `files.Node`). That is what lets the render tests construct a `UI` with three literals instead of a torrent client, a config file and two bound ports.
- `WriteAPIResult` is the whole of the API layer's dependency on the template set. It takes the **message**, not an error: what a failure says — and what it must not say, since operational failures carry syscall strings and filesystem paths — is decided by `server.classify`. This package does not see errors at all.
- `StatsData` carries `sysstat.Stats` through untouched rather than copying it into a view shape. An earlier version copied it field by field to keep the JSON tags out of this package, which meant a dozen assignments kept in lockstep with a struct elsewhere — failing silently, as a stat rendered zero. The tags living in `sysstat` costs this package nothing.
- `templates/` lives here because `//go:embed` only reaches inside its own package directory. Moving the HTML means moving the embed with it.

Templates:

- Templates are addressed **only** by their `{{define}}` name. `template.ParseFS` names by base filename, so two files named `row.html` in different directories silently collide, and requesting a name no file provides yields an empty template that renders nothing without erroring.
- `parseTemplates` returns its error rather than using `template.Must`: the recursive tree template can fail the contextual autoescaper with `ErrOutputContext` at *parse* time, and a package-level `Must` would turn a template edit into a startup panic. `web.New` propagates it.
- **Any path rendered into a URL attribute must go through the `urlpath` template func.** `html/template` only normalizes attributes it recognises as URLs (`href`, `src`); an htmx attribute like `hx-delete` is plain text to it, so a file named `a#b.mkv` produced a request for `/download/a` and deleted a *different* file with a 200. File names come from torrents, so this is attacker-reachable.
- Arithmetic and comparisons happen in the view model, never the template. `fileView.Complete` and `.InProgress` exist because the file table used to test `eq .Percent 100.0` and `and (gt .Percent 0.0) (lt .Percent 100.0)` — float equality against a truncated percentage, so a file at 99.999% rendered as "100.00%" and had to *not* be ticked. `torrentView.Idle` replaced a second copy of the download rate that existed only so a template could compare it to `0.0`. `html/template` has none, and doing it inline invites `100*used/total`, whose divide-by-zero produces `+Inf` before the first disk sample lands.

Rendering and the SSE stream:

- **Every fragment must be wrapped in an element.** Verified in Chromium 150 with htmx 2.0.10 + idiomorph 0.7.4: a bare-text payload swapped with `hx-swap="morph:…"` lands as an *empty* target, with no error anywhere. `checkFragment` rejects it at render time; `TestFragmentsAreWrappedInElements` runs it over every shipped template.
- **`renderer.execute` is the only place a template runs.** `ServePage`, the two pulled fragments and `WriteAPIResult` used to reach through to `r.tmpl.ExecuteTemplate`, which meant `checkFragment` did not run for them — a bare-text `downloads` or `torrent-files` fragment would have shipped with no error anywhere, unnoticed only because those two use `innerHTML` rather than a morph swap.
- **All region names and fragment paths live in `regions.go`.** They are the wire protocol between the templates, the renderer and the browser, and were previously two consts, one bare literal used twice, and several inline strings — so a rename could be applied to three of five places and still compile. `TestTemplateURLsAreDeclared` and `TestSSESwapNamesAreEmitted` check both directions (a template asking for an undeclared path, and a region emitted that nothing listens for); `server.TestKnownRoutesResolve` closes the loop by asserting each `KnownRoutes` entry reaches a handler.
- The renderer keeps one map, not two. Framing is a pure function of `(event, body)` and the event is the key, so a separate body map held no information the framed one did not.
- Change detection compares rendered bytes, not source data — comparing data means maintaining an `Equal` per view model whose failure mode is a silently stale UI. Rendered bodies are retained (not just hashed) because a client connecting mid-tick must get every region's current body immediately.
- Regions are rendered **once per tick** and the same `[]byte` is fanned out; never render per client.
- Rendering is serialized by `UI.mu`: the server's poll and stats loops both call in, and unsynchronized they can broadcast samples in the opposite order to the one they were taken in, leaving browsers on the older one. `seen` is covered by it.
- **`seen` is what the browsers have been told, so it may only advance once they have been told.** It is assigned at the end of `RenderTorrents`, never from a defer: a defer also fires on the early return taken when the skeleton fails to render — a tick where nothing was sent — which marked that tick delivered, skipped its forget events, and left the next tick with an empty removal set. The deleted torrents' regions then stayed cached forever and were replayed to every new subscriber. `TestForgetSurvivesARenderFailure` pins it.
- When a region disappears, emit one final empty event for its name before dropping it (`renderer.forget`). htmx's SSE extension unregisters per-element listeners lazily from inside the listener, so a name that simply stops being emitted leaks the listener and its detached DOM subtree.
- Do not emit an item's first event in the same instant as the membership event that creates its element; leave a tick. Observed in-browser: at 300 ms the item event was missed, at 600 ms it landed.
- Removals are derived from the tracked infohash set, **not** by scanning region names for the `torrent-` prefix: that prefix also matches the membership region `torrent-list`.
- `hub.broadcast` never blocks. A subscriber whose buffer is full is disconnected rather than having the frame dropped: frames carry only what changed, so a dropped frame leaves that browser permanently stale, while a disconnect is self-correcting via EventSource's reconnect plus the snapshot replay.
- `hub.close` reuses that disconnect mechanism for shutdown, but means something different and must stay distinct. It also **evicts** (the reader may already be gone, so nobody will call `unsubscribe`) and it **latches**, so a request arriving after it subscribes to an already-released subscriber. Nothing self-corrects here — the server is going away — and there is deliberately no farewell event: htmx owns the `EventSource`, so telling the page to stop retrying means driving unexported internals, whose failure mode is a permanently dead UI.
- The SSE stream sets a per-write deadline through an `http.ResponseController`, so **every middleware wrapping the `ResponseWriter` must implement `Unwrap`**, and any a streaming path passes through must implement `Flush`. `ServeEvents` type-asserts `http.Flusher` and 500s if it fails, while a missing `Unwrap` fails silently: `SetWriteDeadline` returns `ErrNotSupported` and the stream runs with no timeout. This is exactly what `--log` did before `internal/reqlog`.
- The stream must be excluded from gzip. That exception lives in the server's middleware chain, but the reason is here: `gzhttp` buffers until 1 KiB before deciding whether to compress, and an SSE frame is usually smaller, so the first event would never arrive.

- **The fragment handlers do no method checking and no path parsing.** Both belong to the server's route table: doing them here produced a hand-rolled prefix-and-suffix match that read `torrent/a/b/files` as the infohash `a/b`. `ServeTorrentFiles` takes its hash from `r.PathValue`, so it is one path segment by construction.

## Work Guidance

- A new region is a `{{define}}`, a view model, and a `Render*` method that takes what it needs as an argument. Do not give `UI` a field to hold state between ticks unless it is covered by `UI.mu`.
- Anything expensive or bulky whose visibility the server cannot know belongs behind an `hx-get` fragment, not in the stream — per-torrent file tables and the download tree are the two existing cases.
- Keep the client-side rules in `static/CLAUDE.md` in mind when changing a template: Alpine state must live outside swap targets, and server data must never be interpolated into an `x-data` expression.

## Verification

- `go test -race ./web/`
- `grep -rn "html/template" ../server/*.go` must return nothing — that is the check that the boundary is real.
- Manual: `curl -sN localhost:3000/events` must show named events arriving and then **fall silent** on an idle server. Continuous output means change detection is broken.
- Manual, in a browser: expand a torrent's Files panel and a download folder, wait a minute, and confirm both stay open.

## Child DOX Index

No children. `templates/` is owned by this doc.
