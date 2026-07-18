# build stage
# Pinned: an older toolchain cannot satisfy the `go` directive in go.mod.
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
  -o /tmp/bin

# run stage
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
WORKDIR /app
COPY --from=build /tmp/bin /app/bin
# Run unprivileged: the process only needs to read/write the mounted download dir.
USER 65534:65534
EXPOSE 3000
ENTRYPOINT ["/app/bin"]
