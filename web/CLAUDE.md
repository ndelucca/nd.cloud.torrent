# web

## Purpose

Renders the user interface and pushes it to browsers: the templates, the view
models, the SSE hub, and every handler that produces HTML.

## Ownership

- `ui.go` — `UI`, `Deps`, `New`, `Watchers`, `Close`
- `render.go` — `parseTemplates`, the template funcs, `urlPath`, the byte formatters, and `renderer`: per-region change detection and SSE framing
- `events.go` — `hub` (fan-out with backpressure, `close`) and `ServeEvents`, the `/events` endpoint
- `stats.go` — `StatsData`, `statsView`, `RenderStats`
- `torrents.go` — `torrentView`, `RenderTorrents`, and the two-tier event scheme
- `downloads.go` — `fsView`, `treeSignature`, `RenderDownloads`
- `fragments.go` — `ServePage`, `ServeFragment` (`/fragments/downloads`, per-torrent file tables) and `WriteAPIResult`
- `templates/` — `page`, `stats`, `torrent-list`/`torrent-row`/`torrent-files`, `downloads`/`fsnode`, `omni`/`config`

## Local Contracts

Boundary:

- **`web` must never import `server`.** The standing temptation is to reach for server state; the moment that edge exists the split is undone. Everything this package needs from the outside arrives through the closures in `Deps`, over value types (`engine.Torrent`, `engine.Config`, `files.Node`). That is what lets the render tests construct a `UI` with three literals instead of a torrent client, a config file and two bound ports.
- `WriteAPIResult` is the whole of the API layer's dependency on the template set. It takes an `error` and calls `Error()` — it never inspects the type, so status policy (`apiError`, `statusFor`) stays in `server`.
- `StatsData` is deliberately not the server's `SystemStats`. That type carries the JSON tags that are the `/api/state` wire contract, which is the server's; this one is a view model. The dozen field copies at the single mapping site (`server.renderStats`) are the price of neither format being hostage to the other.
- `templates/` lives here because `//go:embed` only reaches inside its own package directory. Moving the HTML means moving the embed with it.

Templates:

- Templates are addressed **only** by their `{{define}}` name. `template.ParseFS` names by base filename, so two files named `row.html` in different directories silently collide, and requesting a name no file provides yields an empty template that renders nothing without erroring.
- `parseTemplates` returns its error rather than using `template.Must`: the recursive tree template can fail the contextual autoescaper with `ErrOutputContext` at *parse* time, and a package-level `Must` would turn a template edit into a startup panic. `web.New` propagates it.
- **Any path rendered into a URL attribute must go through the `urlpath` template func.** `html/template` only normalizes attributes it recognises as URLs (`href`, `src`); an htmx attribute like `hx-delete` is plain text to it, so a file named `a#b.mkv` produced a request for `/download/a` and deleted a *different* file with a 200. File names come from torrents, so this is attacker-reachable.
- Arithmetic happens in the view model, never the template. `html/template` has none, and doing it inline invites `100*used/total`, whose divide-by-zero produces `+Inf` before the first disk sample lands.

Rendering and the SSE stream:

- **Every fragment must be wrapped in an element.** Verified in Chromium 150 with htmx 2.0.10 + idiomorph 0.7.4: a bare-text payload swapped with `hx-swap="morph:…"` lands as an *empty* target, with no error anywhere. `checkFragment` rejects it at render time; `TestFragmentsAreWrappedInElements` runs it over every shipped template.
- Change detection compares rendered bytes, not source data — comparing data means maintaining an `Equal` per view model whose failure mode is a silently stale UI. Rendered bodies are retained (not just hashed) because a client connecting mid-tick must get every region's current body immediately.
- Regions are rendered **once per tick** and the same `[]byte` is fanned out; never render per client.
- Rendering is serialized by `UI.mu`: the server's poll and stats loops both call in, and unsynchronized they can broadcast samples in the opposite order to the one they were taken in, leaving browsers on the older one. `seen` is covered by it.
- When a region disappears, emit one final empty event for its name before dropping it (`renderer.forget`). htmx's SSE extension unregisters per-element listeners lazily from inside the listener, so a name that simply stops being emitted leaks the listener and its detached DOM subtree.
- Do not emit an item's first event in the same instant as the membership event that creates its element; leave a tick. Observed in-browser: at 300 ms the item event was missed, at 600 ms it landed.
- Removals are derived from the tracked infohash set, **not** by scanning region names for the `torrent-` prefix: that prefix also matches the membership region `torrent-list`.
- `hub.broadcast` never blocks. A subscriber whose buffer is full is disconnected rather than having the frame dropped: frames carry only what changed, so a dropped frame leaves that browser permanently stale, while a disconnect is self-correcting via EventSource's reconnect plus the snapshot replay.
- `hub.close` reuses that disconnect mechanism for shutdown, but means something different and must stay distinct. It also **evicts** (the reader may already be gone, so nobody will call `unsubscribe`) and it **latches**, so a request arriving after it subscribes to an already-released subscriber. Nothing self-corrects here — the server is going away — and there is deliberately no farewell event: htmx owns the `EventSource`, so telling the page to stop retrying means driving unexported internals, whose failure mode is a permanently dead UI.
- The SSE stream sets a per-write deadline through an `http.ResponseController`, so **every middleware wrapping the `ResponseWriter` must implement `Unwrap`**, and any a streaming path passes through must implement `Flush`. `ServeEvents` type-asserts `http.Flusher` and 500s if it fails, while a missing `Unwrap` fails silently: `SetWriteDeadline` returns `ErrNotSupported` and the stream runs with no timeout. This is exactly what `--log` did before `internal/reqlog`.
- The stream must be excluded from gzip. That exception lives in the server's middleware chain, but the reason is here: `gzhttp` buffers until 1 KiB before deciding whether to compress, and an SSE frame is usually smaller, so the first event would never arrive.

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
