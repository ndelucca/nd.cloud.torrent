# static

## Purpose

Compiles the web UI's client-side assets into the binary and exposes them as an
`http.Handler`. The HTML itself is *not* here — it is rendered server-side from
`web/templates/`; this package ships only CSS, JavaScript and the favicon.

## Ownership

- `static.go` — package `ctstatic`: `//go:embed files/*`, and `FileSystemHandler()` which serves the `files/` subtree rooted at `/`
- `files/css/ct.css` — the whole stylesheet, hand-written, no framework
- `files/js/ct.js` — the client behaviour htmx and Alpine do not cover: idiomorph guards, tree collapse persistence, two-step delete, drag-and-drop upload, upload progress, the connection indicator, and the spacebar media toggle
- `files/js/vendor/` — pinned third-party bundles: htmx 2.0.10 (`htmx.min.js`), its SSE extension (`sse.js`, patched), idiomorph 0.7.4 (`idiomorph-ext.min.js`), Alpine 3.15.0 **CSP build** (`alpine.min.js`). Provenance and hashes: `static/VENDOR`
- `files/cloud-favicon.png`

## Local Contracts

- **`static/VENDOR` records what the four vendored bundles are**: package,
  version, source URL and sha384, each established by fetching the upstream
  artefact and comparing hashes rather than by reading a version string. Two of
  the four carry no version marker at all, so that is the only way to know. It
  lives outside `files/` because everything under there is embedded and served.
- **`sse.js` is patched.** It is `htmx-ext-sse@2.2.3` with one line changed:
  `api.swap` is called with `{ contextElement: elt }`, which upstream omits.
  htmx uses that to resolve extensions *for that element*, and the two SSE
  regions are `hx-swap="morph:innerHTML"`, so the morph extension has to be
  found for them. An upgrade that drops in the upstream file cleanly removes it,
  with no console error and no failing test. `static/VENDOR` has the detail.
- **Every vendored `<script>` carries `integrity=`**, and CI recomputes the
  hashes from the files, matching them against both the manifest and the page.
  A stale integrity value is invisible server-side — the browser refuses to run
  the script and the UI is simply dead — so it is a gate, not a convention.
  `ct.js` is ours and carries none.

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
  into it.** Two independent reasons: the value is *evaluated*, so a
  torrent-supplied file name there is a script-injection sink the
  `html/template` escaper cannot see; and Alpine leaves `_x_marker` set on an
  initialised element, so a changed `x-data` value is silently ignored forever.
  Pass data via `data-*` attributes and read it from `$el.dataset`.
- **Alpine is the CSP build (`@alpinejs/csp`), and that is load-bearing.** It
  parses attribute expressions into an AST and interprets them instead of
  compiling them with the AsyncFunction constructor, which is what lets the
  app's CSP drop `'unsafe-eval'`. Swapping back to the standard build breaks
  nothing at build time and every binding at runtime.
- **Its parser accepts expressions only.** Identifiers, literals, member and
  call expressions, binary and unary operators, conditionals, assignments,
  updates, array and object literals — all fine, and all fourteen of the
  existing bindings parse unchanged. Statements and sequences do not: `if (x) y()`
  and `a; b` are parse errors. That is the whole reason `torrentRow` exists,
  and a test scans the templates for both.
- Server-rendered `data-*` attributes, not DOM structure, carry per-node facts:
  `data-id` for the stored-state key, `data-top` for a tree node's default
  collapse state. Deriving depth by walking `parentElement` breaks on any markup
  change and throws on a null parent.
- `ct.js` installs the idiomorph guards and must therefore load *before* Alpine.
- **`ct.js` sets two htmx config flags, and the timing is load-bearing.**
  `allowEval = false` (nothing uses `hx-on::` or the `js:` prefixes, so htmx
  needs no eval) and `includeIndicatorStyles = false` (htmx injects a `<style>`
  for `.htmx-indicator` at boot, which `style-src 'self'` refuses; nothing here
  defines or uses that class). Both must be set before htmx boots — its boot is
  wrapped in a `DOMContentLoaded` deferral and every script here is `defer`, so
  document order gives `ct.js` its chance. Do not move it after Alpine.
- **No inline event handlers in any template.** `hx-on::` is compiled with
  `new Function` and a bare `onchange` is inline script; both would need
  `script-src 'unsafe-inline'`, which is the directive that actually stops an
  injected script from running. The two that existed — the omni form's reset and
  the file input's auto-submit — are delegated listeners in `ct.js`, scoped by
  element id like the others.
- An event dispatched by Alpine bubbles *up*, so an `hx-trigger` on a sibling
  never sees it — use `from:closest <ancestor>`.
- **The tree's collapse state lives in the DOM, and `localStorage` covers only a
  page reload.** `#downloads` is morphed and every `<li>` carries a stable
  server-rendered `id`, so idiomorph matches by id and Alpine's state survives a
  swap in place — including across an `EventSource` reconnect, which re-fires the
  `hx-trigger` and is therefore just another swap. A reload is the one case a
  morph cannot help with: the document is rebuilt and Alpine re-initialises from
  the server markup.
- **The stored value is the set of ids that *differ* from the server default**,
  under the single key `ct.tree.open`, and it is pruned against the rendered tree
  after each swap. One key per directory grows without bound — nothing removes a
  folder that no longer exists — and cannot express "top-level folder the user
  closed" without writing a value for every folder. The cost of pruning against
  the rendered tree: a folder hidden behind `files.Limit` loses its stored state.
  Storage failures are non-fatal by design (private mode, quota).
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
