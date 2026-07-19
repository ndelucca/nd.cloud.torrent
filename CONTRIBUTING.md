# Contributing

## Layout

```
main.go              flag parsing and process lifecycle
engine/              torrent engine: wraps anacrolix/torrent
server/              process shell, middleware, routing, /api/*
web/                 rendering, view models, the SSE stream
web/templates/       html/template fragments — the UI's HTML
configfile/          loading and atomically persisting the engine config
files/               download tree, path containment, file serving
fetch/               SSRF-guarded remote .torrent download
sysstat/             host CPU/memory/disk sampling
static/files/        embedded CSS and JavaScript
internal/auth/       basic-auth login plus a signed session cookie
internal/cli/        flag registration, env fallbacks, the help screen
internal/reqlog/     one log line per request
internal/testutil/   fixtures shared by more than one package's tests
```

Dependencies flow one way: `main` → {`server`, `internal/cli`}, `server` →
{`engine`, `web`, `files`, `fetch`, `sysstat`, `static`, `configfile`,
`internal/auth`, `internal/reqlog`}, `configfile` → `engine`, and `web` →
{`engine`, `files`, `sysstat`}. Nothing below the server imports it, and the
`boundaries` job in CI enforces that rather than trusting it.

## Building

```sh
go build -o nd-cloud-torrent .
./nd-cloud-torrent --port 3000
```

**Upgrading a vendored JS bundle** means updating `static/VENDOR` and the
matching `integrity=` in `web/templates/page.html` together; CI fails if they
drift. Read `static/VENDOR` first — `sse.js` carries a local patch that an
upstream drop-in would silently remove.

**Editing anything under `static/files/` or `web/templates/` requires a
rebuild before it is visible.** Both are compiled into the binary with
`go:embed`; there is no file-watching dev server and no build step. This is the
single most common thing to trip over.

## Verifying

The fast checks, in order of how quickly they fail:

```sh
gofmt -l .          # must print nothing
go vet ./...
go build ./...
go test -race ./... # the race detector is not optional here
```

The race detector matters specifically: the bugs this codebase actually shipped
were unsynchronised map access, twice.

CI runs more than the above, so these passing is necessary and not sufficient:

- linux, macOS and Windows: `go vet`, the build, and `go test -race`. macOS and
  Windows run it with `-short`, which skips the two wall-clock tests, so only
  linux runs the full suite.
- linux only: `gofmt`, coverage, a `go mod tidy` gate, the `boundaries` job
  (import-graph checks), `staticcheck` and `govulncheck`.

To run the linux-only analysis locally:

```sh
staticcheck ./...
govulncheck ./...
go mod tidy && git diff --exit-code go.mod go.sum
```

### Manual checks

**A large part of this UI cannot fail in CI.** A CSP violation, a lost morph, a
bad SRI hash — none of them break the build or a test. They break the page, in
silence. So the browser pass is not optional after a UI change, and the console
is part of it.

```sh
go build -o nd-cloud-torrent . && ./nd-cloud-torrent --port 3000
```

Remember the assets are `go:embed`ed: without the rebuild you are testing the
last binary, not your change.

**With the console open, and zero errors in it:**

- The connection dot turns green.
- **No CSP violation reports.** The app serves `script-src 'self'` with no
  `unsafe-inline` and no `unsafe-eval`; anything that needs either shows up here
  and nowhere else. A blocked script usually reads as "the page loaded but
  nothing works".
- **No SRI failures.** A stale `integrity=` makes the browser refuse the script
  outright — the symptom is identical to the above, so read the message.

**Then, with a real download running** (the state-preservation checks are
meaningless on an idle server, because nothing is re-rendering):

- Add a magnet; progress advances **without reloading the page**.
- Expand a torrent's Files panel and a download folder. Wait a minute. Both must
  still be open — if either snaps shut, an Alpine-owned element has ended up
  inside a swap target.
- Open a video preview and press play. Wait a minute. **It must still be
  playing.** The downloads tree is morphed rather than replaced specifically so
  that this survives; if the video restarts or disappears, the morph is not
  holding and the reduced `localStorage` in `ct.js` rests on a false premise.
- Reload the page. Folders you opened stay open, folders you closed stay closed.
  That is the one case a morph cannot cover and the only reason the stored state
  exists.

**And the things with no automated cover at all:**

- Delete a file from the tree: the panel must **not** go blank, and the outcome
  is reported in the status region above. Delete something that no longer exists
  (delete it twice) — the failure must be reported, not silent.
- Tab to the `×` on a tree row and press Enter. Focus should land somewhere
  usable, not on `<body>` — the button hides itself via `x-show`, so a keyboard
  user can be dumped back to the top of the document. **This is a known open
  defect**; confirm whether it still reproduces before fixing it.
- With more than `files.Limit` (1000) entries in the download directory, a
  folder below the cut loses its remembered open/closed state on each swap. That
  is the accepted cost of pruning stored state against the rendered tree, but it
  has never been confirmed in a browser.
- `curl -sN localhost:3000/events` shows named events, and **falls silent** on an
  idle server. Continuous output means change detection is broken.

## The UI

The UI is server-rendered HTML pushed over SSE, with htmx swapping fragments and
Alpine holding the little client state that remains. Two rules carry most of the
weight, both verified in a real browser rather than assumed:

1. **Alpine state lives outside swap targets**, on an element with a stable
   server-rendered `id`. A morph reverts what Alpine wrote to the DOM (`x-show`'s
   inline style) and Alpine does not repair it.
2. **Every SSE fragment must be wrapped in an element.** A bare-text payload
   swaps as empty, with no error anywhere.

`web/CLAUDE.md` owns the fragment-wrapping rule and `static/CLAUDE.md` owns the
Alpine/idiomorph rules; `server/CLAUDE.md` owns the middleware chain and routing.
Read the relevant one before changing that layer.

## Commits

Keep unrelated changes in separate commits. Explain *why* in the body — the code
already shows what changed.
