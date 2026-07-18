# DOX framework

- DOX is highly performant CLAUDE.md hierarchy installed here
- Agent must follow DOX instructions across any edits

## Core Contract

- CLAUDE.md files are binding work contracts for their subtrees
- Work products, source materials, instructions, records, assets, and durable docs must stay understandable from the nearest applicable CLAUDE.md plus every parent CLAUDE.md above it

## Read Before Editing

1. Read the root CLAUDE.md
2. Identify every file or folder you expect to touch
3. Walk from the repository root to each target path
4. Read every CLAUDE.md found along each route
5. If a parent CLAUDE.md lists a child CLAUDE.md whose scope contains the path, read that child and continue from there
6. Use the nearest CLAUDE.md as the local contract and parent docs for repo-wide rules
7. If docs conflict, the closer doc controls local work details, but no child doc may weaken DOX

Do not rely on memory. Re-read the applicable DOX chain in the current session before editing.

## Update After Editing

Every meaningful change requires a DOX pass before the task is done.

Update the closest owning CLAUDE.md when a change affects:

- purpose, scope, ownership, or responsibilities
- durable structure, contracts, workflows, or operating rules
- required inputs, outputs, permissions, constraints, side effects, or artifacts
- user preferences about behavior, communication, process, organization, or quality
- CLAUDE.md creation, deletion, move, rename, or index contents

Update parent docs when parent-level structure, ownership, workflow, or child index changes. Update child docs when parent changes alter local rules. Remove stale or contradictory text immediately. Small edits that do not change behavior or contracts may leave docs unchanged, but the DOX pass still must happen.

## Hierarchy

- Root CLAUDE.md is the DOX rail: project-wide instructions, global preferences, durable workflow rules, and the top-level Child DOX Index
- Child CLAUDE.md files own domain-specific instructions and their own Child DOX Index
- Each parent explains what its direct children cover and what stays owned by the parent
- The closer a doc is to the work, the more specific and practical it must be

## Child Doc Shape

- Create a child CLAUDE.md when a folder becomes a durable boundary with its own purpose, rules, responsibilities, workflow, materials, or quality standards
- Work Guidance must reflect the current standards of the project or user instructions; if there are no specific standards or instructions yet, leave it empty
- Verification must reflect an existing check; if no verification framework exists yet, leave it empty and update it when one exists

Default section order:

- Purpose
- Ownership
- Local Contracts
- Work Guidance
- Verification
- Child DOX Index

## Style

- Keep docs concise, current, and operational
- Document stable contracts, not diary entries
- Put broad rules in parent docs and concrete details in child docs
- Prefer direct bullets with explicit names
- Do not duplicate rules across many files unless each scope needs a local version
- Delete stale notes instead of explaining history
- Trim obvious statements, repeated rules, misplaced detail, and warnings for risks that no longer exist

## Closeout

1. Re-check changed paths against the DOX chain
2. Update nearest owning docs and any affected parents or children
3. Refresh every affected Child DOX Index
4. Remove stale or contradictory text
5. Run existing verification when relevant
6. Report any docs intentionally left unchanged and why

## User Preferences

When the user requests a durable behavior change, record it here or in the relevant child CLAUDE.md

- Address the user as Naza
- Do not sign commits with agent attribution (no `Co-Authored-By: Claude`, no "Generated with Claude Code")

## Project

Cloud Torrent: self-hosted remote torrent client in Go. A single binary embeds a server-rendered web UI (`html/template` + htmx + Alpine), a torrent engine backed by `anacrolix/torrent`, and an HTTP server that streams downloaded files and pushes live HTML fragments to browsers over Server-Sent Events.

- Module path: `github.com/ndelucca/nd.cloud.torrent` (Go 1.25.4)
- Entry point: `main.go` — builds `server.Server` defaults, registers the CLI flags via `internal/cli`, calls `Run(version)`
- `version` is injected at build time with `-ldflags "-X main.version=..."`; the `0.0.0-src` default means an unreleased local build
- Runtime config persists to `cloud-torrent.json` (path overridable with `--config-path`)

Request flow: `main` → `server.New` → `Server.Run` → handler chain (requestlog → security headers → cookieauth → gzip → `Server.route`) → `/events` SSE / `/` page / `/fragments/*` / `/api/*` / `/download/` / static assets.

## Repo-Wide Contracts

- The server owns all HTTP surface and process lifecycle; the engine owns torrent state and never imports `server`
- Frontend assets and HTML templates are compiled into the binary via `go:embed`; any change to either requires a rebuild to take effect
- Dependency direction is one-way: `main` → {`server`, `internal/cli`} and `server` → {`engine`, `static`, `internal/auth`, `internal/reqlog`}. Nothing under `internal/` imports `server` or `engine`. Do not introduce back-edges.

## Work Guidance

- Keep the binary dependency-free at runtime: `CGO_ENABLED=0` builds must keep working across linux/darwin/windows/openbsd
- Prefer the standard library. Three direct dependencies remain, each earning its place by encapsulating platform or protocol detail: `anacrolix/torrent` (the engine), `klauspost/compress` (gzip middleware) and `gopsutil/v4` (cross-platform system stats). Authentication, CLI parsing and request logging now live in `internal/` — weigh that precedent before adding a dependency for something small.
- Run `go mod tidy` after touching `go.mod`

## Verification

- `go build -v -o /dev/null .`
- `go vet ./...` and `gofmt -l .` (both must be clean; CI enforces them)
- `go test -race ./...` — the race detector is not optional here: the bugs this codebase actually shipped were unsynchronized map access
- CI runs these plus `staticcheck` and `govulncheck` on every push and pull request, across linux/macos/windows

## Child DOX Index

- `engine/CLAUDE.md` — torrent engine: client lifecycle, torrent/file state, start/stop/delete semantics
- `internal/CLAUDE.md` — stdlib-only replacements for third-party helpers: `auth` (session cookies), `cli` (flags), `reqlog` (request logging)
- `server/CLAUDE.md` — HTTP server, server-side rendering, the SSE stream, `/api/*`, file serving, system stats
- `static/CLAUDE.md` — embedded CSS/JS assets (covers `static/files/` too; no doc may live under `files/`, it would be embedded and served). The HTML lives in `server/templates/`.
- `.github/CLAUDE.md` — CI, release, and Docker packaging

Owned by this doc: `main.go`, `go.mod`/`go.sum`, `README.md`, `CONTRIBUTING.md`, `LICENSE`, `.gitignore`.
