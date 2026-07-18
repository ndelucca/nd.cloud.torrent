# static

## Purpose

Compiles the web UI into the binary and exposes it as an `http.Handler`. Also owns the UI itself: an AngularJS 1.x single-page app that renders server state pushed over velox and drives the backend through `/api/*`.

## Ownership

- `static.go` — package `ctstatic`: `//go:embed files/*`, and `FileSystemHandler()` which serves the `files/` subtree rooted at `/`
- `files/index.html` — the only page; declares `ng-app="app"`, defines the layout, loads every script in a fixed order
- `files/js/run.js` — root controller: opens the velox connection to `/sync`, mirrors it into `$rootScope.state`, exposes shared helpers
- `files/js/utils.js` — the `api` factory (POSTs to `api/<action>`), the `search` service, `storage`, filters, error handling
- `files/js/config-controller.js` — settings panel; submits `state.Config` as JSON to `api.configure`
- `files/js/omni-controller.js` — the magnet/URL/search omnibox
- `files/js/torrents-controller.js` — torrent and per-file actions
- `files/js/downloads-controller.js` — the downloaded-files tree
- `files/js/semantic-checkbox.js` — Semantic UI checkbox directive
- `files/js/vendor/` — pinned third-party bundles (angular, moment, query-string)
- `files/template/` — partial views: `config`, `omni`, `torrents`, `downloads`, `download-tree`
- `files/css/` — `app.css`, per-section styles, Semantic UI bundle, Lato webfont, theme assets

## Local Contracts

Embedding:

- Package name is `ctstatic`, not `static`; the server imports it as `github.com/jpillora/cloud-torrent/static`
- `files/` is the web root — `files/index.html` is served at `/`, `files/js/run.js` at `/js/run.js`
- Everything under `files/` ships inside the binary and is publicly served. Do not put documentation, notes, or secrets there — this doc lives at `static/CLAUDE.md` for exactly that reason.
- `go:embed` skips names beginning with `_` or `.`; never rely on such a name inside `files/`
- Assets are baked in at compile time: editing anything under `files/` requires a rebuild before it is visible

Web app:

- State is read-only in the browser: the server pushes `state` over velox and the UI never mutates it directly — every change goes through an `api` call
- `state.*` field names mirror exported Go fields in `server.Server.state`. Renaming on either side breaks the UI silently.
- API actions are enumerated in the `actions` array in `files/js/utils.js` and must match the switch in `server/server_api.go`: `configure`, `magnet`, `url`, `torrent`, `file`, `torrentfile`
- Composite action arguments are colon-joined and order-sensitive — `torrent` takes `<action>:<infohash>`, `file` takes `<action>:<infohash>:<path>`
- API responses are plain text: `OK` on success, the raw error message on `400`
- `js/velox.js` is not a file on disk — the server serves the velox client library at that path
- Scripts are plain globals loaded in dependency order by `index.html`; `window.app` is created inline before the controllers. New scripts must be added to that list.
- No build step, no bundler, no transpiler — the shipped files are the source

## Work Guidance

- Stay on AngularJS 1.x idioms (`app.controller`, `app.factory`, `$scope`) and ES5 syntax to match the existing code
- Update `files/js/vendor/` only by replacing whole pinned bundles; do not edit `files/css/semantic.min.css`
- Keep the tree small — every byte lands in the shipped binary

## Verification

- `go build ./...`
- Rebuild, load `http://localhost:3000`, and confirm the connection indicator is green
- Check the browser console for errors after touching any script

## Child DOX Index

No children. `files/` and everything under it is owned by this doc.
