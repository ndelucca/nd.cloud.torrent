# web

## Purpose

Renders the user interface and pushes it to browsers: the templates, the view
models, the SSE hub, and every handler that produces HTML.

## Ownership

- `ui.go` — `UI`, `Deps`, `New`, `Watchers`, `Close`
- `render.go` — `parseTemplates`, the template funcs, `urlPath`, the formatters (`humanBytes`, `humanAgo`, `humanSince`, `percentOf`), and `renderer`: `execute`, `store`, `snapshot`, change detection and SSE framing
- `regions.go` — the SSE region names, `KnownRoutes` (the fragment paths the templates ask for) and `StaticAssets` (the embedded files `page.html` loads)
- `events.go` — `hub` (fan-out with backpressure, `close`) and `ServeEvents`, the `/events` endpoint
- `stats.go` — `StatsData`, `statsView` (which embeds `sysstat.Stats`), `RenderStats`
- `torrents.go` — `torrentView`, `fileView`, `RenderTorrents`
- `downloads.go` — `downloadsView`, `fsView`, `shapeSignature`/`contentSignature`, `RenderDownloads`
- `fragments.go` — `ServePage`, `ServeDownloads`, `ServeTorrentFiles`, `WriteAPIResult`
- `templates/` — thirteen `{{define}}`s: `page`; `stats`; `torrent-list`/`torrent-row`/`torrent-files`; `downloads`/`fsnode`; `omni`/`config`/`api-ok`/`api-error`/`fragment-message`/`placeholder`

## Local Contracts

Boundary:

- **`web` must never import `server`.** Everything this package needs from the
  outside arrives through the closures in `Deps`, over value types
  (`engine.Torrent`, `engine.Config`, `files.Node`) — which is what lets a test
  construct a `UI` from three literals instead of a torrent client, a config
  file and two bound ports.
- `WriteAPIResult` is the whole of the API layer's dependency on the template
  set. It takes the **message**, not an error: what a failure says is decided by
  `server.classify`. This package does not see errors at all.
- `StatsData` carries `sysstat.Stats` through untouched. A field-by-field copy
  costs a dozen assignments kept in lockstep with a struct elsewhere, failing
  silently as a stat that renders zero.
- `templates/` lives here because `//go:embed` only reaches inside its own
  package directory. Moving the HTML means moving the embed with it.

Templates:

- Templates are addressed **only** by their `{{define}}` name. `template.ParseFS`
  names by base filename, so two files named `row.html` in different directories
  silently collide, and requesting a name no file provides yields an empty
  template that renders nothing without erroring.
- `parseTemplates` returns its error rather than using `template.Must`: the
  recursive `fsnode` template can fail the contextual autoescaper with
  `ErrOutputContext` at *parse* time, and a package-level `Must` would turn a
  template edit into a startup panic. `web.New` propagates it.
- **Any path rendered into a URL attribute must go through the `urlpath`
  template func — every URL attribute, with no exceptions for the ones
  `html/template` recognises.** An htmx attribute like `hx-delete` is plain text
  to the escaper, so a file named `a#b.mkv` yields a request for `/download/a`.
  `href` and `src` are not the safe case they look like: they get the URL
  *normalizer*, which passes `#`, `?` and `&` through unescaped **by design**
  (`urlProcessor` in `html/template/url.go`), so the identical truncation
  happens there. File names come from torrents, so this is attacker-reachable.
  `{{.InfoHash}}` is wrapped too: an exemption keyed on a field name is a hole
  that gets copy-pasted by pattern.
  A test enforces this over the raw template source, and a second test enforces
  the enforcer — a scanner is green both when there are no violations and when it
  has stopped matching, so it counts matched attribute values against matched
  attribute names. Consequence: URL attribute values must be double-quoted.
- **Arithmetic and comparisons happen in the Go view model, never in a
  template.** `html/template` has no arithmetic, and doing it inline invites
  `100*used/total`, whose divide-by-zero produces `+Inf` before the first disk
  sample lands (`percentOf` guards it). Float comparison is the other trap:
  `fileView.Complete`/`.InProgress` exist so nothing tests `eq .Percent 100.0`
  against a percentage that renders truncated.
- **Formatting is a template func when it is generic, a view-model field when it
  encodes a decision.** `bytes`, `pct`, `round` and `urlpath` are funcs:
  one value in, same meaning everywhere. `fileView.Complete`, `torrentView.Idle`,
  `fsView.Modified` (which calls `humanAgo` from Go) and `statsView.Uptime` are
  fields: each is a judgement (what
  counts as complete, as idle, how a duration reads) belonging with the model
  rather than repeated at each call site. A field's name must say what it holds —
  `statsView` splits the process start into `StartedAt` (instant), `Started`
  (formatted) and `Uptime` (duration) rather than one field doing all three.
- **`placeholder` is the empty and loading state; `fragment-message` is a
  message.** Their markup is identical today and they stay separate anyway: a
  message reports something that happened and could grow a dismiss control or
  `role="status"`, a placeholder stands in for content that has not arrived.
  Markup a Go file would otherwise build as a string literal goes through
  `fragment-message`, so the class contract stays where the template tests can
  see it. One literal remains, in `writeMessage`, for when the template set
  itself failed to render.

Rendering and the SSE stream:

- **Every fragment must be wrapped in an element.** A bare-text payload swapped
  with `hx-swap="morph:…"` lands as an *empty* target, with no error anywhere
  (Chromium 150, htmx 2.0.10). `checkFragment` rejects it at render time, and a
  test enumerates `tmpl.Templates()` so a new `{{define}}` without a fixture
  fails rather than being skipped.
- **`renderer.execute` is the only place a template runs**, which is what makes
  `checkFragment` cover the pulled fragments and the page as well as the
  streamed regions.
- **All region names and fragment paths live in `regions.go`.** They are the wire
  protocol between the templates, the renderer and the browser; declaring them
  once is what makes a rename either complete or a compile error. Tests check
  both directions — a template asking for an undeclared path, and a region
  emitted that nothing listens for — and `server` asserts each `KnownRoutes`
  entry reaches a handler.
- **`StaticAssets` is separate from `KnownRoutes` because it is asserted
  differently:** a `KnownRoutes` entry is a pattern, checked by resolving it;
  the assets are real files, checked by fetching them. Resolving would prove
  nothing — the server mounts `GET /css/` and `GET /js/` as prefixes, so a
  renamed stylesheet resolves happily and 404s in the browser.
- Change detection compares rendered bytes, not source data — comparing data
  means maintaining an `Equal` per view model whose failure mode is a silently
  stale UI. Rendered bodies are retained (not just hashed) because a client
  connecting mid-tick must get every region's current body immediately.
- Regions are rendered **once per tick** and the same `[]byte` is fanned out;
  never render per client. A tick is one broadcast and one frame, a connect is
  one write. That is what gives `subBuffer` a meaning: it measures ticks of lag,
  not changed rows.
- **Every SSE region name is a fixed literal and all of them exist from the first
  frame.** There are three: `torrent-list`, `stats`, `downloads-changed`. That is
  the contract keeping htmx's SSE extension out of this program's correctness
  argument. **Do not introduce a region name built at runtime** — an element
  cannot listen for a name before it exists, and the extension unregisters
  per-element listeners lazily, from inside the listener.
- The trade taken: the whole list is re-sent whenever any torrent changes, ~2 KB
  per torrent framed; idle costs nothing, because the region is byte-gated. If
  O(torrents)-per-tick ever bites, strip per-line indentation before framing
  (~20%, since `data: ` per line makes whitespace unusually expensive) or raise
  `pollInterval`; neither reintroduces dynamic region names.
- Rendering is serialized by `UI.mu`: the server's poll and stats loops both call
  in, and unsynchronized they can broadcast samples in the opposite order to the
  one they were taken in, leaving browsers on the older one.
- `hub.broadcast` never blocks. A subscriber whose buffer is full is disconnected
  rather than having the frame dropped, which is self-correcting via
  EventSource's reconnect plus the snapshot replay. `stats` and the downloads
  ping are deltas, so that path stays needed even though `torrent-list` is a full
  snapshot.
- `hub.close` reuses that mechanism for shutdown but means something different
  and must stay distinct. It also **evicts** (the reader may already be gone, so
  nobody will call `unsubscribe`) and it **latches**, so a request arriving after
  it subscribes to an already-released subscriber. Nothing self-corrects — the
  server is going away — and there is deliberately no farewell event: htmx owns
  the `EventSource`, so telling the page to stop retrying means driving
  unexported internals, whose failure mode is a permanently dead UI.
- The SSE stream sets a per-write deadline through an `http.ResponseController`,
  so **every middleware wrapping the `ResponseWriter` must implement `Unwrap`**,
  and any a streaming path passes through must implement `Flush`. `ServeEvents`
  type-asserts `http.Flusher` and 500s if it fails, while a missing `Unwrap`
  fails silently: `SetWriteDeadline` returns `ErrNotSupported` and the stream
  runs with no timeout.
- The stream must be excluded from gzip. That exception lives in the server's
  middleware chain, but the reason is here: `gzhttp` buffers until 1 KiB before
  deciding whether to compress, and an SSE frame is usually smaller, so the first
  event would never arrive.
- **The fragment handlers do no method checking and no path parsing.** Both
  belong to the server's route table; done here they produce a hand-rolled
  prefix-and-suffix match that reads `torrent/a/b/files` as the infohash `a/b`.
  `ServeTorrentFiles` takes its hash from `r.PathValue`, so it is one path
  segment by construction, and it fetches through `Deps.TorrentFiles` — a keyed
  lookup for the one row being expanded, not a scan of a full snapshot.
  `engine.Torrent` deliberately has no `Files` field, so the streamed row cannot
  pay for a file table by accident.

## Work Guidance

- A new region is a `{{define}}`, a view model, and a `Render*` method that takes
  what it needs as an argument. Do not give `UI` a field to hold state between
  ticks unless it is covered by `UI.mu`. There is exactly one such field today —
  the download tree's last admitted content signature — and it earns its place:
  "still changing" versus "settled" is a property of the tree over time, so no
  pure function of the tree in hand can decide it.
- **The download tree is fingerprinted in two halves.** `shapeSignature` (names,
  `IsDir`, `Truncated`) fires the ping immediately; `contentSignature` (sizes,
  mtimes) is admitted at most once per `downloadsSettle`. Hashing both together
  changes on every tick of every download, so the browser re-fetches the whole
  tree once a second. The rate limit is not a throttle: once writing stops the
  stored signature still differs, so the last write is always published within
  one window rather than dropped.
- Anything expensive or bulky whose visibility the server cannot know belongs
  behind an `hx-get` fragment, not in the stream — per-torrent file tables and
  the download tree are the two existing cases.
- Keep the client-side rules in `static/CLAUDE.md` in mind when changing a
  template: Alpine state must live outside swap targets, and server data must
  never be interpolated into an `x-data` expression.

## Verification

- `go test -race ./web/`
- `grep -rn "html/template" ../server/*.go` must return nothing.
- Manual: `curl -sN localhost:3000/events` must show named events arriving and
  then **fall silent** on an idle server. Continuous output means change
  detection is broken.
- Manual, in a browser: expand a torrent's Files panel and a download folder,
  wait a minute, and confirm both stay open.

## Child DOX Index

No children. `templates/` is owned by this doc.
