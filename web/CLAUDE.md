# web

## Purpose

Renders the user interface and pushes it to browsers: the templates, the view
models, the SSE hub, and every handler that produces HTML.

## Ownership

- `ui.go` — `UI`, `Deps`, `New`, `Watchers`, `Close`
- `render.go` — `parseTemplates`, the template funcs, `urlPath`, the byte formatters, and `renderer`: `execute`, per-region change detection and SSE framing
- `regions.go` — the SSE region names, `KnownRoutes` (the fragment paths the templates ask for) and `StaticAssets` (the embedded files `page.html` loads)
- `events.go` — `hub` (fan-out with backpressure, `close`) and `ServeEvents`, the `/events` endpoint
- `stats.go` — `StatsData`, `statsView` (which embeds `sysstat.Stats`), `RenderStats`
- `torrents.go` — `torrentView`, `fileView`, `RenderTorrents`
- `downloads.go` — `downloadsView`, `fsView`, `treeSignature`, `RenderDownloads`
- `fragments.go` — `ServePage`, `ServeDownloads`, `ServeTorrentFiles` and `WriteAPIResult`
- `templates/` — `page`, `stats`, `torrent-list`/`torrent-row`/`torrent-files`, `downloads`/`fsnode`, `omni`/`config`/`fragment-message`

## Local Contracts

Boundary:

- **`web` must never import `server`.** The standing temptation is to reach for server state; the moment that edge exists the split is undone. Everything this package needs from the outside arrives through the closures in `Deps`, over value types (`engine.Torrent`, `engine.Config`, `files.Node`). That is what lets the render tests construct a `UI` with three literals instead of a torrent client, a config file and two bound ports.
- `WriteAPIResult` is the whole of the API layer's dependency on the template set. It takes the **message**, not an error: what a failure says — and what it must not say, since operational failures carry syscall strings and filesystem paths — is decided by `server.classify`. This package does not see errors at all.
- `StatsData` carries `sysstat.Stats` through untouched rather than copying it into a view shape. An earlier version copied it field by field to keep the JSON tags out of this package, which meant a dozen assignments kept in lockstep with a struct elsewhere — failing silently, as a stat rendered zero. The tags living in `sysstat` costs this package nothing.
- `templates/` lives here because `//go:embed` only reaches inside its own package directory. Moving the HTML means moving the embed with it.

Templates:

- Templates are addressed **only** by their `{{define}}` name. `template.ParseFS` names by base filename, so two files named `row.html` in different directories silently collide, and requesting a name no file provides yields an empty template that renders nothing without erroring.
- `parseTemplates` returns its error rather than using `template.Must`: the recursive tree template can fail the contextual autoescaper with `ErrOutputContext` at *parse* time, and a package-level `Must` would turn a template edit into a startup panic. `web.New` propagates it.
- **Any path rendered into a URL attribute must go through the `urlpath` template func — every URL attribute, with no exceptions for the ones `html/template` recognises.** An htmx attribute like `hx-delete` is plain text to the escaper, so a file named `a#b.mkv` produced a request for `/download/a` and deleted a *different* file with a 200. `href` and `src` are not the safe case they look like: they get the URL *normalizer*, which passes `#`, `?` and `&` through unescaped **by design** (`urlProcessor` in `html/template/url.go`), because it normalizes a URL instead of escaping a path — so the identical truncation happens there. File names come from torrents, so this is attacker-reachable. `{{.InfoHash}}` is wrapped too, even though a 40-hex infohash escapes to itself: the invariant lives in one expression in `engine` and nothing propagates it to the template, and an exemption keyed on a field name is a hole that gets copy-pasted by pattern.
- `TestURLAttributesUseURLPath` enforces that rule over the raw template source, and **`TestURLAttributeScanHasNoBlindSpot` enforces the enforcer** — a scanner test is green both when there are no violations and when it has stopped matching, so it counts matched attribute values against matched attribute names. Do not delete it as redundant; without it the gate can quietly stop being one. Consequence: URL attribute values must be double-quoted.
- **Formatting is a template func when it is generic, a view-model field when it encodes a decision.** `bytes`, `pct` and `urlpath` are funcs: they transform one value and mean the same thing everywhere. `fileView.Complete`, `torrentView.Idle`, `fsView.Modified` and `statsView.Uptime` are fields: each is a judgement (what counts as complete, as idle, how a duration reads) that belongs with the model, not repeated at each call site. Without the rule the next addition is a coin flip — it already produced `humanAgo` baked into one view model and called as a func from another.
- **A field's name must say what it holds.** `statsView.Uptime` was a `time.Time` of the process start, so `{{ago .Uptime}}` rendered "up 3 hours ago" and the tooltip printed a raw Go time with a monotonic-clock suffix. It is now `StartedAt` (the instant), `Started` (formatted) and `Uptime` (the duration).
- **`placeholder` is the empty and loading state; `fragment-message` is a message.** One wording site and one class instead of the five ad-hoc `<p>`s that were split across `.empty` and `.muted`. Their markup is identical today and they stay separate anyway: a message reports something that happened and could grow a dismiss control or `role="status"`, a placeholder stands in for content that has not arrived. Merging them because the HTML currently matches would make that divergence a two-site change.
- `fragment-message` is the shared plain-message fragment. Markup that a Go file would otherwise build as a string literal — "Torrent not found", render fallbacks — goes through it, so the class contract stays in the template set where the template tests can see it. One literal remains, in `writeMessage`, for the case where the template set itself failed to render.
- Arithmetic and comparisons happen in the view model, never the template. `fileView.Complete` and `.InProgress` exist because the file table used to test `eq .Percent 100.0` and `and (gt .Percent 0.0) (lt .Percent 100.0)` — float equality against a truncated percentage, so a file at 99.999% rendered as "100.00%" and had to *not* be ticked. `torrentView.Idle` replaced a second copy of the download rate that existed only so a template could compare it to `0.0`. `html/template` has none, and doing it inline invites `100*used/total`, whose divide-by-zero produces `+Inf` before the first disk sample lands.

Rendering and the SSE stream:

- **Every fragment must be wrapped in an element.** Verified in Chromium 150 with htmx 2.0.10 + idiomorph 0.7.4: a bare-text payload swapped with `hx-swap="morph:…"` lands as an *empty* target, with no error anywhere. `checkFragment` rejects it at render time.
- `TestFragmentsAreWrappedInElements` **enumerates `tmpl.Templates()`**, so a new `{{define}}` without a fixture fails the test rather than being skipped. It previously iterated a hand-written list that had drifted to 9 of 11 — `page` and `fsnode` were absent while this doc claimed every shipped template was covered. The enumeration skips the `*.html` entries `ParseFS` registers per file alongside each `{{define}}`; those are the whitespace between the defines and are never executed.
- **`renderer.execute` is the only place a template runs**, which is what makes `checkFragment` cover the pulled fragments and the page as well as the streamed regions.
- **All region names and fragment paths live in `regions.go`.** They are the wire protocol between the templates, the renderer and the browser; declaring them once is what makes a rename either complete or a compile error. `TestTemplateURLsAreDeclared` and `TestSSESwapNamesAreEmitted` check both directions (a template asking for an undeclared path, and a region emitted that nothing listens for); `server.TestKnownRoutesResolve` closes the loop by asserting each `KnownRoutes` entry reaches a handler.
- **`StaticAssets` is separate from `KnownRoutes` because it is asserted differently.** A `KnownRoutes` entry is a pattern and is checked by resolving it; the assets are real files and are checked by fetching them (`server.TestStaticAssetsAreServed`). Resolution would prove nothing here — the server mounts `GET /css/` and `GET /js/` as prefixes, so a renamed stylesheet resolves happily and 404s in the browser.
- The renderer keeps one map, not two. Framing is a pure function of `(event, body)` and the event is the key, so a separate body map held no information the framed one did not.
- Change detection compares rendered bytes, not source data — comparing data means maintaining an `Equal` per view model whose failure mode is a silently stale UI. Rendered bodies are retained (not just hashed) because a client connecting mid-tick must get every region's current body immediately.
- Regions are rendered **once per tick** and the same `[]byte` is fanned out; never render per client.
- **A tick is one broadcast and one frame, and a connect is one write.** The torrent list is a single region, rendered once per tick and byte-gated, so an unchanged list emits nothing. `TestOneBroadcastPerTick` pins it and also asserts the one frame carries every torrent, so "one broadcast" cannot be satisfied by sending less. This is what gives `subBuffer` a meaning: it measures ticks of lag, not changed rows.
- **Every SSE region name is a fixed literal and all of them exist from the first frame.** There are three: `torrent-list`, `stats`, `downloads-changed`. That is the contract keeping htmx's SSE extension out of this program's correctness argument.
- **Do not introduce a region name built at runtime.** What one costs, kept as a warning because the code that handled it is gone:
  - htmx's SSE extension unregisters a per-element listener *lazily, from inside the listener*. A name that stops being emitted leaks the listener and the detached DOM subtree it closes over, so a disappearing region needs one final empty event to let it collect itself.
  - An element cannot listen for a name before it exists, so a frame arriving in the same flush as the event that creates its element is silently discarded — observed in-browser: missed at 300 ms, delivered at 600 ms. That forces a "leave a tick" rule *and* a snapshot ordering where membership leads.
  - Tracking what the browsers have been told is cross-tick state, and it may only advance once they have actually been told — never from a `defer`, which also fires on the early return taken when a render fails.
  - Gating membership on the infohash *set* cannot reorder: the set does not change when a magnet's name arrives, so a row stayed in its "Fetching metadata…" position indefinitely. `TestSortOrderFollowsName` pins the fix.
- The trade taken: the whole list is re-sent whenever any torrent changes — ~2 KB per torrent framed, so ~42 KB/s with 20 active torrents against ~21 KB/s before. Idle costs nothing either way. If O(torrents)-per-tick ever bites, strip per-line indentation before framing (~20%, since `data: ` per line makes whitespace unusually expensive) or raise `pollInterval`; neither reintroduces tiers.
- The claim that small frames were cheaper on the wire was false and is worth not re-deriving: the stream is excluded from gzip outright, so there was never a persistent deflate window for them to fit inside.
- `snapshot` returns every region in one buffer for a connecting client. Order is irrelevant now that every name is fixed and its element is already in the page the browser holds.
- Rendering is serialized by `UI.mu`: the server's poll and stats loops both call in, and unsynchronized they can broadcast samples in the opposite order to the one they were taken in, leaving browsers on the older one.
- `hub.broadcast` never blocks. A subscriber whose buffer is full is disconnected rather than having the frame dropped, and the disconnect is self-correcting via EventSource's reconnect plus the snapshot replay. The torrent region is now a full snapshot rather than a delta, so a client that misses one also self-corrects on the next change — but `stats` and the downloads ping are still deltas, so the disconnect path stays.
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
