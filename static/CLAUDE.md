# static

## Purpose

Compiles the web UI's client-side assets into the binary and exposes them as an
`http.Handler`. The HTML itself is *not* here — it is rendered server-side from
`web/templates/`; this package ships only CSS, JavaScript and the favicon.

## Ownership

- `static.go` — package `ctstatic`: `//go:embed files/*`, and `FileSystemHandler()` which serves the `files/` subtree rooted at `/`
- `files/css/ct.css` — the whole stylesheet, hand-written, no framework
- `files/js/ct.js` — the client behaviour htmx and Alpine do not cover: idiomorph guards, drag-and-drop upload, upload progress, the connection indicator, tree collapse persistence, and the spacebar media toggle
- `files/js/vendor/` — pinned third-party bundles: htmx 2.0.10, its SSE extension, idiomorph, Alpine 3.x

## Local Contracts

Embedding:

- Package name is `ctstatic`, not `static`; the server imports it as `github.com/ndelucca/nd.cloud.torrent/static`
- `files/` is the web root — `files/css/ct.css` is served at `/css/ct.css`
- Everything under `files/` ships inside the binary and is publicly served. Do not put documentation, notes, or secrets there — this doc lives at `static/CLAUDE.md` for exactly that reason.
- `go:embed` skips names beginning with `_` or `.`; never rely on such a name inside `files/`
- Assets are baked in at compile time: editing anything under `files/` requires a rebuild before it is visible

Client behaviour:

- **Alpine state must live outside SSE swap targets, on an element with a stable server-rendered `id`.** Verified in Chromium 150: idiomorph preserves `_x_dataStack` when it matches a node by id, but it reverts *what Alpine wrote* — `x-show`'s inline style is stripped and Alpine never repairs it, because its effects only re-run when the reactive data changes. The visible symptom is a collapsed panel popping open once per second.
- **Never interpolate server data into an `x-data` expression.** Alpine leaves `_x_marker` set on an initialised element, so a changed `x-data` value is silently ignored forever. Pass data via `data-*` attributes (the tree uses `data-id`) and read it from `$el.dataset`.
- `ct.js` installs the idiomorph guards and must therefore load *before* Alpine. `data-preserve` on an element opts its whole subtree out of morphing — use it for playing media.
- Fragments arriving over SSE must be wrapped in an element; a bare-text payload swaps as empty. The server enforces this (`checkFragment`), but the same rule applies to anything written here.
- An event dispatched by Alpine bubbles *up*, so an `hx-trigger` on a sibling never sees it — use `from:closest <ancestor>`.
- The tree's collapse state lives in `localStorage` under `ct.tree.<id>`; storage failures are non-fatal by design (private mode, quota).

## Work Guidance

- Vanilla ES5-compatible JavaScript, no build step, no bundler, no transpiler — the shipped files are the source
- Update `files/js/vendor/` only by replacing whole pinned bundles
- Keep the tree small — every byte lands in the shipped binary for every platform
- Prefer CSS and Alpine over new JavaScript; `ct.js` should stay readable in one sitting

## Verification

- `go build ./...`
- Rebuild, load `http://localhost:3000`, and confirm the connection dot turns green
- Expand a torrent's Files panel and a download folder, wait a minute, and confirm both stay open — that is the golden rule holding
- Check the browser console for errors after touching any script

## Child DOX Index

No children. `files/` and everything under it is owned by this doc.
