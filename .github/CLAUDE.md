# .github

## Purpose

Build, test, release, and container packaging.

## Ownership

- `workflows/ci.yml` — `test` job (gofmt/vet/build/test) and `analyze` job (staticcheck/govulncheck) on every PR, and on push to `master` and `refactor/**`; `release_binaries` and `release_docker` jobs gated on `refs/tags/v*`
- `goreleaser.yml` — cross-platform binary matrix, deb/rpm/apk packages, changelog filters
- `Dockerfile` — two-stage build producing a `scratch` image with only the binary and CA certificates

## Local Contracts

- Releases trigger on `v*` tags only; both release jobs depend on `test` and `analyze` passing
- Version is stamped through `-ldflags "-X main.version=..."` in all three files — goreleaser uses `{{.Version}}`, the Dockerfile uses `git describe --abbrev=0 --tags`. Keep them consistent with `main.version`.
- Builds are `CGO_ENABLED=0` everywhere, which is what allows the `scratch` image and the cross-compilation matrix
- Docker images publish to `ghcr.io/<repo>` with semver tags; goreleaser publishes GitHub release artifacts
- The Dockerfile copies CA certificates deliberately: `/api/*` fetches remote `.torrent` files over HTTPS and would fail without them on a `scratch` image

## Work Guidance

- The CI Go version (`1.25`) is pinned separately from `go.mod` (`1.25.4`) — keep them in step when bumping either
- Test goreleaser changes locally with `goreleaser build --snapshot --config .github/goreleaser.yml`

## Verification

- Push a branch and confirm the `Build & Test` job passes
- `goreleaser build --snapshot --config .github/goreleaser.yml` for packaging changes

## Child DOX Index

No children.
