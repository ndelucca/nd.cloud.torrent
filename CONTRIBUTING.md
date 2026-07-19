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

Some behaviour only fails in a browser, so it is worth 30 seconds after any UI
change:

```sh
go run . --port 3000
```

- The connection dot turns green.
- Add a magnet; progress advances **without reloading the page**.
- Expand a torrent's Files panel and a download folder. Wait a minute. Both must
  still be open — if either snaps shut, an Alpine-owned element has ended up
  inside a swap target.
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
