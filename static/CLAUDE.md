# static

## Purpose

Compiles the web UI's client-side assets into the binary and exposes them as an
`http.Handler`. The HTML itself is *not* here — it is rendered server-side from
`web/templates/`; this package ships only CSS, JavaScript and the favicon.

## Ownership

- `static.go` — package `ctstatic`: `//go:embed files/*`, and `FileSystemHandler()` which serves the `files/` subtree rooted at `/`
- `files/css/ct.css` — the whole stylesheet, hand-written, no framework
- `files/js/ct.js` — the client behaviour htmx and Alpine do not cover: idiomorph guards, tree collapse persistence, two-step delete, drag-and-drop upload, upload progress, the connection indicator, and the spacebar media toggle
- `files/js/vendor/` — pinned third-party bundles: htmx 2.0.10 (`htmx.min.js`), its SSE extension (`sse.js`), idiomorph (`idiomorph-ext.min.js`), Alpine 3.15.0 (`alpine.min.js`)
- `files/cloud-favicon.png`

## Local Contracts

Embedding:

- Package name is `ctstatic`, not `static`; the server imports it as `github.com/ndelucca/nd.cloud.torrent/static`
- `files/` is the web root — `files/css/ct.css` is served at `/css/ct.css`
- Everything under `files/` ships inside the binary and is publicly served. Do not put documentation, notes, or secrets there — this doc lives at `static/CLAUDE.md` for exactly that reason.
- `go:embed` skips names beginning with `_` or `.`; never rely on such a name inside `files/`

Client behaviour:

- **Alpine state must live outside SSE swap targets, on an element with a stable
  server-rendered `id`.** Idiomorph preserves `_x_dataStack` when it matches a
  node by id, but it reverts *what Alpine wrote*, and Alpine never repairs it —
  its effects only re-run when the reactive data changes. Symptom: a collapsed
  panel popping open once per second.
- **The `beforeAttributeUpdated` guard derives its rule from the node's own
  bindings; do not go back to enumerating attributes.** It refuses `x-cloak`
  unconditionally, `style` on an `x-show` element, and any attribute the node
  binds with `:attr` or `x-bind:attr`. Enumeration cannot cover `:class` and
  `:aria-expanded` on elements that have no `x-show`.
- **The cost of the derived rule:** the server cannot vary the static class list
  of an element that also carries a `:class` binding. That is the correct
  semantics — such an element's classes belong to Alpine — but it is a real
  constraint on template edits.
- **`x-cloak` is the sharp edge.** Alpine strips it exactly once, at init. The
  server markup still carries it, so a morph re-adds it and
  `[x-cloak]{display:none!important}` hides the element with nothing left to
  remove it. Hence the guard's unconditional `x-cloak` case.
- **`data-preserve` opts a whole subtree out of morphing** (`beforeNodeMorphed`),
  and covers more than playing media: any panel whose content arrives via
  `hx-get` needs it, because the server markup there is only a placeholder and a
  morph reverts the fetched content back to it. The per-torrent file panel and
  the media preview are the two cases.
- **`x-data` must always be a constant literal — never interpolate server data
  into it.** Alpine leaves `_x_marker` set on an initialised element, so a changed
  `x-data` value is silently ignored forever. Pass data via `data-*` attributes
  and read it from `$el.dataset`.
- Server-rendered `data-*` attributes, not DOM structure, carry per-node facts:
  `data-id` for the localStorage key, `data-top` for a tree node's default
  collapse state. Deriving depth by walking `parentElement` breaks on any markup
  change and throws on a null parent.
- `ct.js` installs the idiomorph guards and must therefore load *before* Alpine.
- An event dispatched by Alpine bubbles *up*, so an `hx-trigger` on a sibling
  never sees it — use `from:closest <ancestor>`.
- The tree's collapse state lives in `localStorage` under `ct.tree.<id>`; storage
  failures are non-fatal by design (private mode, quota). It cannot live in the
  DOM: `#downloads` is `hx-swap="innerHTML"`, so the whole tree is replaced on
  every refetch. The per-torrent file panel needs no equivalent — its row is
  morphed and keeps its Alpine state across ticks.
- **The SSE stream stays open in a background tab, deliberately.** `EventSource`
  is not throttled when hidden, and without TLS this is HTTP/1.1 with ~6
  connections per origin, so many pinned tabs can starve it — accepted rather
  than fixed. Do not close it on `visibilitychange`: htmx owns the
  `EventSource`, so reopening one means driving unexported internals
  (`document.body["htmx-internal-data"].sseEventSource`), and the hide/show path
  cannot be covered by headless automation.

## Work Guidance

- Vanilla ES5-compatible JavaScript, no build step, no bundler, no transpiler — the shipped files are the source
- Update `files/js/vendor/` only by replacing whole pinned bundles
- Keep the tree small — every byte lands in the shipped binary for every platform
- Prefer CSS and Alpine over new JavaScript; `ct.js` should stay readable in one sitting

## Verification

- `go build ./...`
- Rebuild, load `http://localhost:3000`, and confirm the connection dot turns green
- Expand a torrent's Files panel and a download folder, wait a minute, and confirm both stay open
- Check the browser console for errors after touching any script

## Child DOX Index

No children. `files/` and everything under it is owned by this doc.
