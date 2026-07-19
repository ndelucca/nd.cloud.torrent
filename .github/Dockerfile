# build stage
# The minor is fixed because an older toolchain cannot satisfy the `go` directive
# in go.mod. The tag still floats within 1.25.x, which is the intent: patch
# releases of the toolchain should be picked up.
FROM golang:1.25-alpine AS build
# Version is passed in by CI (github.ref_name); the default marks a local build.
ARG VERSION=0.0.0-src
WORKDIR /src
# Copy manifests first so the module download layer caches independently of source edits.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build \
  -trimpath \
  -ldflags "-s -w -X main.version=${VERSION}" \
  -o /out/app/bin

# Stage the final /app tree here, with its ownership, because the run stage is
# `scratch`: there is no shell, so no RUN can mkdir or chown after the fact.
#
# Both paths have to exist and be owned by the runtime UID before it ever runs:
#   /app/downloads  the default download directory (./downloads under WORKDIR)
#   /app            configfile.Save writes a temp file *beside* the config and
#                   renames it, so the directory itself must be writable, not
#                   just the file
# Without this the image runs as 65534 against a root-owned 0755 /app and cannot
# write either one.
RUN mkdir -p /out/app/downloads && chown -R 65534:65534 /out/app

# run stage
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# Copied from /out so that /app is itself a copied entry and takes the --chown.
# Copying /out/app to /app instead would create /app as an implicit destination
# directory, which does not reliably inherit it.
COPY --from=build --chown=65534:65534 /out/ /
WORKDIR /app
# Declared so the paths are discoverable from the image itself rather than only
# from the README. An anonymous volume is initialised from the image content at
# this path, ownership included, which is the other reason the directory has to
# exist in the image rather than being created at run time.
VOLUME ["/app/downloads"]
USER 65534:65534
EXPOSE 3000
ENTRYPOINT ["/app/bin"]
