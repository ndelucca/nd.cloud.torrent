# .github

## Purpose

Build, test, release, and container packaging. The root doc states which checks
run where; this doc owns the details behind them.

## Ownership

- `workflows/ci.yml` — `test`, `boundaries`, `analyze`, `release_build`
  (packaging dry run, on everything that is *not* a tag), `release_binaries` and
  `release_docker` (gated on `refs/tags/v*`)
- The `boundaries` job is the import-graph gate; the root doc lists what it
  enforces. Two rules for adding a check to it: derive the answer from
  `go list`/`go mod`, never from a text search over sources — a grep matches
  comments and `_test.go` files, and misses transitive edges — and run the `go`
  command on its own line so `set -e` catches a tooling failure. A check written
  as `n=$(go mod graph | grep -c X || true)` reports zero and passes green when
  `go mod graph` itself fails.
- `goreleaser.yml` — cross-platform binary matrix, deb/rpm/apk packages, changelog
  filters
- `Dockerfile` — two-stage build producing a `scratch` image with only the binary
  and CA certificates

## Local Contracts

- **`goreleaser.yml` and the `Dockerfile` have no consumer except the release
  jobs**, which is why `release_build` exercises both on every non-tag run. It is
  amd64-only and never pushes; the multi-arch matrix stays with the tagged job.
- **`release_build` also *runs* the image, with no mounts.** Building only proves
  it compiles. The run stage is `scratch` under `USER 65534`, so `/app` and
  `/app/downloads` must be created and chowned in the build stage — and a
  container with no mounts is exactly the case that was broken. The step asserts
  the declared user, the ownership of both paths, that `POST /api/configure`
  answers 200, and that the config really landed on disk. That last one matters:
  a 200 with no file is what a silent write failure looks like.
- Ownership is read with `docker cp … | tar --numeric-owner -tvf -`, because
  `scratch` has no shell to `exec` into. `--numeric-owner` is not optional —
  without it GNU tar prints the local name for 65534 (`nobody`) and the check
  passes vacuously.
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
- **`/app` and `/app/downloads` are staged and chowned to `65534` in the build
  stage, and copied as one tree.** The run stage is `scratch`, so there is no
  shell and no `RUN` that could create or chown them afterwards. Both are
  needed, not just the download directory: `configfile.Save` writes a temp file
  beside the config and renames it, so `/app` itself must be writable by the
  runtime UID. The copy source is `/out/` rather than `/out/app` so that `/app`
  is a copied entry that takes `--chown`, instead of a destination directory
  Docker creates implicitly. An anonymous volume is initialised from the image
  content at the mount path, ownership included, which is the other reason the
  directory has to exist in the image.
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
- **Third-party actions in `release_binaries` and `release_docker` are pinned to
  commit SHAs**, with the version in a trailing comment. Those two jobs hold
  `contents: write` and `packages: write` and a registry token, so a mutable
  `@v4` there is an action that can publish under this repo's name and change
  underneath it. The line is drawn deliberately: the read-only `test`,
  `boundaries` and `analyze` jobs stay on major tags, where the same drift costs
  a broken build rather than a compromised release. Bump a SHA and its comment
  together — the comment is the only thing that makes the pin readable.
- Known deprecation, not yet acted on: `archives[].format` in `goreleaser.yml`
  was superseded by `formats` (a list) in GoReleaser v2. It is a warning today
  and will become an error on some future minor, and the action resolves
  `~> v2`. `release_build` runs on every PR, so the break will surface there
  rather than on a published tag — fix it when a v2 with `formats` support is
  confirmed for the resolved version.

## Verification

- Push a branch and confirm the `Build & Test` job passes
- `goreleaser build --snapshot --config .github/goreleaser.yml` for packaging
  changes
