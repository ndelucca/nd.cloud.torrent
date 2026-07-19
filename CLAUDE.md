# Cloud Torrent

## Working here

- `CLAUDE.md` files are the contract for their subtree. Read this one plus every `CLAUDE.md` on
  the path to the files you are changing; the closest doc wins on local detail.
- Update the owning doc when a change moves a contract, a boundary, an input or an output.
  Delete text that stops being true rather than annotating it.
- Document the invariant that holds now. What the code used to do, which bug was hit and which
  test pins it belong in `git log` and in the test name — both recoverable, neither able to go
  stale in the source.
- Address the user as Naza. Do not sign commits with agent attribution (no `Co-Authored-By`, no
  "Generated with Claude Code").

## Project

Cloud Torrent: self-hosted remote torrent client in Go. A single binary embeds a server-rendered
web UI (`html/template` + htmx + Alpine), a torrent engine backed by `anacrolix/torrent`, and an
HTTP server that streams downloaded files and pushes live HTML fragments to browsers over
Server-Sent Events.

- Module path: `github.com/ndelucca/nd.cloud.torrent` (Go 1.25.4)
- Entry point: `main.go` — builds `server.Server` defaults, registers the CLI flags via
  `internal/cli`, calls `Run(version)`
- `version` is injected at build time with `-ldflags "-X main.version=..."`; the `0.0.0-src`
  default means an unreleased local build
- Runtime config persists to `cloud-torrent.json` (path overridable with `--config-path`)

Request flow: `main` → `server.New` → `Server.Run` → handler chain (`reqlog` → security headers →
`auth` → gzip → `Server.routes`) → `/events` SSE / `/` page / `/fragments/*` (all three in `web`) /
`/api/*` / `/download/` (in `files`) / static assets.

## Repo-Wide Contracts

- The server owns the HTTP surface and process lifecycle; the engine owns torrent state. Below the
  server sit five leaf packages with one job each: `web` renders, `files` walks and serves the
  download directory, `fetch` pulls a remote `.torrent`, `sysstat` reads the host, `configfile`
  loads and atomically persists the engine config. None of them imports `server` or each other,
  except `web` → {`files`, `sysstat`} for the types it renders.
- Frontend assets and HTML templates are compiled into the binary via `go:embed`; any change to
  either requires a rebuild to take effect.
- Dependency direction is one-way and enforced by the compiler:

  ```
  main   → { server, internal/cli }
  server → { engine, web, files, fetch, sysstat, static, configfile, internal/auth, internal/reqlog }
  configfile → engine
  web    → { engine, files, sysstat }
  files  → stdlib only
  fetch  → stdlib only
  sysstat→ gopsutil
  engine → anacrolix/torrent
  ```

  Do not introduce back-edges. `web` importing `server` is the specific one to watch for: the
  temptation is shared state, and it undoes the split.

## Work Guidance

- Keep the binary dependency-free at runtime: `CGO_ENABLED=0` builds must keep working across
  linux/darwin/windows. OpenBSD is deliberately not built — `anacrolix/torrent`'s storage package
  does not compile there (`undefined: unix.SEEK_DATA`); see `.github/goreleaser.yml`.
- Error strings are ordinary lowercase Go, in every package. `server.classify` owns what the user
  is shown: it capitalises the detail of errors the caller caused and substitutes a fixed message
  for everything else, logging the chain. Error strings are not UI copy — treating them as such
  welds the producing package to an HTML caller and puts raw syscall text in front of users.
- Prefer the standard library. Three direct dependencies remain, each encapsulating platform or
  protocol detail: `anacrolix/torrent` (the engine), `klauspost/compress` (gzip middleware) and
  `gopsutil/v4` (cross-platform system stats). Authentication, CLI parsing and request logging live
  in `internal/` — weigh that precedent before adding a dependency for something small.
- Run `go mod tidy` after touching `go.mod`.
- **Never hand-edit the `// indirect` block.** It is derived output: it records the modules needed
  to resolve versions across *every* valid configuration — any `GOOS`, either `CGO_ENABLED`, and
  the tests of our dependencies — not the modules linked into our binary. Of the 90 indirect
  entries, 89 are reachable from `anacrolix/torrent`, and 16 are never linked into any shipped
  build (the sqlite storage backends and `go-libutp`, which are cgo-only; `plan9stats`/`perfstat`,
  for platforms we do not publish; and test-only deps of deps). Deleting one gets it restored by
  the next `go mod tidy` and breaks `go test ./...` for anyone building with cgo. The count is a
  property of the engine, not something to optimise.
- `pion/webrtc` and its 15 companion modules are unconditional in `anacrolix/torrent` —
  `client.go`, `torrent.go` and `config.go` import it with no build tag. There is no way to drop
  WebTorrent support short of forking the engine.

## Verification

- `go build -v -o /dev/null .`
- `go vet ./...` and `gofmt -l .` (both must be clean; CI enforces them)
- `go test -race ./...` — the race detector is not optional here: the bugs this codebase actually
  shipped were unsynchronized map access
- CI triggers on every pull request, and on push for `master`, `refactor/**` and `v*` tags.
  - linux/macOS/windows: `go vet`, the build, and `go test -race`. **macOS and Windows run
    `-short`**, which skips the two wall-clock tests; only linux runs the full suite.
  - linux only: `gofmt`, coverage, the `go mod tidy` gate, the boundary checks, `staticcheck` and
    `govulncheck`.
  - The release *build* (`goreleaser build --snapshot`, `docker build`) runs on every PR, so a
    broken packaging config is not discovered on a tag that is already public. The release
    *publish* jobs are gated on a `v*` tag and on `test`, `analyze` and `boundaries` passing.
- **Some architectural boundaries are CI gates.** The `boundaries` job currently enforces three:
  `web` must not import `server` (via `go list -deps`, the real build graph), `server` must not
  import `html/template`, and no `jpillora` module may return to the graph. Other boundaries stated
  in the child docs — `internal/`, `files` and `fetch` staying stdlib-only, `sysstat` owning the
  `gopsutil` import, `configfile` importing only `engine` — are conventions, not gates.

## Child DOX Index

- `configfile/CLAUDE.md` — loading and atomically persisting the engine config as JSON
- `engine/CLAUDE.md` — torrent engine: client lifecycle, torrent/file state, start/stop/delete semantics
- `internal/CLAUDE.md` — stdlib-only replacements for third-party helpers: `auth` (session cookies), `cli` (flags), `reqlog` (request logging), plus `testutil` (shared test fixtures)
- `server/CLAUDE.md` — process lifecycle, middleware chain, routing, `/api/*`, `/api/state`, system stats
- `web/CLAUDE.md` — templates, view models, the SSE hub, and every handler that produces HTML (owns `web/templates/`)
- `files/CLAUDE.md` — the download tree walk, path containment, file and zip serving
- `fetch/CLAUDE.md` — the SSRF-guarded remote `.torrent` download
- `sysstat/CLAUDE.md` — host resource sampling; owns the `Stats` type shared by `/api/state` and the stats region
- `static/CLAUDE.md` — embedded CSS/JS assets (covers `static/files/` too; no doc may live under it, it would be embedded and served). The HTML lives in `web/templates/`.
- `.github/CLAUDE.md` — CI, release, and Docker packaging

Owned by this doc: `main.go`, `go.mod`/`go.sum`, `README.md`, `CONTRIBUTING.md`, `LICENSE`, `.gitignore`.
