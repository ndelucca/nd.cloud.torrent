# sysstat

## Purpose

Samples host resource usage: CPU, memory, disk, and this process's own heap and
goroutine count.

## Ownership

- `sysstat.go` — `Stats`, `Sample`

## Local Contracts

- **One type, shared by everyone who touches a sample.** `Stats` is both the
  `/api/state` wire shape (its `json` tags are that contract) and what the stats
  region renders. The server used to keep its own tagged copy and the renderer a
  view copy, with a dozen field assignments between them — updated in lockstep
  by hand, and failing silently as a stat that renders zero. Do not reintroduce
  a parallel shape; add the field here.
- `Sample` is a pure function of the host. It holds no state, decides nothing
  about *when* to sample, and never pushes — the caller owns the cadence, which
  is what lets the server gate sampling on whether anyone is watching.
- `Set` reports whether **every** source succeeded. A partial sample must not be
  displayed as though it were current, so consumers check it before showing CPU,
  memory or disk.
- `cpu.Percent(0, false)` reports usage since the previous call *in this
  process*, so the caller's interval defines the measurement window. That is why
  `server.statsInterval` must stay fixed rather than adaptive, and why the
  server samples on every tick instead of only when a browser is connected:
  skipping samples silently widens the window, so the first reading after an
  idle spell was an average over however long nobody was watching — reported
  with `Set` true.
- This is the only package that imports `gopsutil`, and it must stay that way:
  it is the one dependency here that earns its place purely by encapsulating
  per-platform detail.

## Work Guidance

- A new metric is a field with a `json` tag plus a read in `Sample`, and it
  becomes visible to both `/api/state` and the stats template at once. Adding it
  to the template is a separate, optional step.
- Failures are logged and leave the field zero rather than returning an error.
  A stats sample is not worth failing a request over; `Set` is how a caller
  knows not to trust it.

## Verification

- `go build ./...`
- `go test -race ./sysstat/` — `TestSampleReportsPartialFailure` is the one that
  matters: it pins that a failed source clears `Set` while the Go-runtime fields
  are still filled, so `Set` rather than a zero value is the signal.
- `curl -s localhost:3000/api/state | jq .Stats.System` must show every field
  with its lower-camel name — those names are the wire contract.

## Child DOX Index

No children.
