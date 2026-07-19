# .github

## Purpose

Build, test, release, and container packaging.

## Ownership

- `workflows/ci.yml` — `test` (gofmt/vet/build/test/coverage/tidy), `boundaries` (the three architectural gates) and `analyze` (staticcheck/govulncheck) on every PR and on push to `master` and `refactor/**`; `release_build` (packaging dry run) on everything that is *not* a tag; `release_binaries` and `release_docker` gated on `refs/tags/v*`
- `goreleaser.yml` — cross-platform binary matrix, deb/rpm/apk packages, changelog filters
- `Dockerfile` — two-stage build producing a `scratch` image with only the binary and CA certificates

## Local Contracts

- Releases trigger on `v*` tags only; both release jobs depend on `test`, `analyze` and `boundaries` passing
- **`release_build` builds the release artifacts on every non-tag run.** `goreleaser.yml` and the `Dockerfile` have no other consumer, so without it a broken packaging config surfaced during a release, on a tag that was already public — the one irreversible operation in the pipeline was the least tested. The dry run is amd64-only and never pushes; the multi-arch matrix stays with the tagged job.
- Version is stamped through `-ldflags "-X main.version=..."` in all three files — goreleaser uses `{{.Version}}`, the Dockerfile uses `git describe --abbrev=0 --tags`. Keep them consistent with `main.version`.
- Builds are `CGO_ENABLED=0` everywhere, which is what allows the `scratch` image and the cross-compilation matrix
- Docker images publish to `ghcr.io/<repo>` with semver tags; goreleaser publishes GitHub release artifacts
- The Dockerfile copies CA certificates deliberately: `/api/*` fetches remote `.torrent` files over HTTPS and would fail without them on a `scratch` image

## Work Guidance

- The CI Go version (`1.25`) is pinned separately from `go.mod` (`1.25.4`) — keep them in step when bumping either
- Test goreleaser changes locally with `goreleaser build --snapshot --config .github/goreleaser.yml`

- CI measures coverage on linux with **`-coverpkg=./...`** and prints the total. There is no threshold: the number is there to be looked at, and a gate would mostly reward tests written to move it. The flag is not optional — without it a function is only credited to its own package's tests, so `web`'s fragment handlers read 0.0% while `server` tests drive them through the real mux. That understated the total by ~6 points and hid real gaps behind fake ones.
- The two wall-clock tests are guarded by `testing.Short()` and CI passes `-short` on macOS and Windows. Those runners are the slowest and most contended, so a timing failure there is far more likely to be the runner than the code; linux runs the full set.
- CI fails if `go mod tidy` would change `go.mod`/`go.sum`. With ~90 indirect entries, drift is otherwise invisible until someone else's build breaks.
- `staticcheck` and `govulncheck` are **pinned**, not `latest`. This is the security job: a tool that moves under us can break the build or, worse, quietly stop checking.
- The Dockerfile's `WORKDIR` is `/app` and the default download directory is the relative `./downloads`, so the mount target is `/app/downloads`. The README documented `/downloads`, which meant downloads landed inside the container and vanished on restart. `VOLUME` declares it so the path is discoverable from the image rather than only from the docs.

## Verification

- Push a branch and confirm the `Build & Test` job passes
- `goreleaser build --snapshot --config .github/goreleaser.yml` for packaging changes

## Child DOX Index

No children.
