# .github

## Purpose

Build, test, release, and container packaging. The root doc states which checks
run where; this doc owns the details behind them.

## Ownership

- `workflows/ci.yml` — `test`, `boundaries`, `analyze`, `release_build`
  (packaging dry run, on everything that is *not* a tag), `release_binaries` and
  `release_docker` (gated on `refs/tags/v*`)
- `goreleaser.yml` — cross-platform binary matrix, deb/rpm/apk packages, changelog
  filters
- `Dockerfile` — two-stage build producing a `scratch` image with only the binary
  and CA certificates

## Local Contracts

- **`goreleaser.yml` and the `Dockerfile` have no consumer except the release
  jobs**, which is why `release_build` exercises both on every non-tag run. It is
  amd64-only and never pushes; the multi-arch matrix stays with the tagged job.
- Version is stamped through `-ldflags "-X main.version=..."`. goreleaser uses
  `{{.Version}}`; the Dockerfile takes `ARG VERSION` (default `0.0.0-src`) and CI
  supplies `github.ref_name` on a tag, `0.0.0-ci` in the dry run. The Dockerfile
  does not and cannot call `git` — `.dockerignore` excludes `.git`.
- Builds are `CGO_ENABLED=0` everywhere, which is what allows the `scratch` image
  and the cross-compilation matrix.
- The Dockerfile copies CA certificates deliberately: fetching a remote `.torrent`
  over HTTPS fails without them on `scratch`. It runs as `65534:65534`.
- `WORKDIR` is `/app` and the default download directory is the relative
  `./downloads`, so the mount target is `/app/downloads`. `VOLUME` declares it so
  the path is discoverable from the image, not only from the docs.
- Docker images publish to `ghcr.io/<repo>` with semver tags; goreleaser publishes
  the GitHub release artifacts.

## Work Guidance

- The CI Go version (`1.25`) is pinned separately from `go.mod` (`1.25.4`) — keep
  them in step when bumping either.
- Coverage uses **`-coverpkg=./...`** and is not optional: without it a function is
  credited only to its own package's tests, so `web`'s fragment handlers read 0.0%
  while `server` tests drive them through the real mux. There is deliberately no
  threshold — a gate mostly rewards tests written to move the number.
- `staticcheck` and `govulncheck` are **pinned**, not `latest`. This is the
  security job: a tool that moves under us can break the build or quietly stop
  checking.

## Verification

- Push a branch and confirm the `Build & Test` job passes
- `goreleaser build --snapshot --config .github/goreleaser.yml` for packaging
  changes
