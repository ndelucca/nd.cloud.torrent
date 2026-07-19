# sysstat

## Purpose

Samples host resource usage: CPU, memory, disk, and this process's own heap and
goroutine count.

## Ownership

- `sysstat.go` — `Stats`, `Sample`

## Local Contracts

- **One type, shared by everyone who touches a sample.** `Stats` is both the
  `/api/state` wire shape (its `json` tags are that contract) and what the stats
  region renders. Add a field here rather than a parallel wire or view shape — two
  shapes drift silently into a stat that renders zero.
- `Sample` is a pure function of the host: no state, no pushing, no opinion about
  *when* to sample. The caller owns the cadence.
- `Set` reports whether **every** source succeeded, and consumers check it before
  showing CPU, memory or disk, so a partial sample is never shown as current.
- **`cpu.Percent(0, false)` reports usage since the previous call in this process,
  so the caller's interval is the measurement window.** That is why
  `server.statsInterval` is fixed rather than adaptive, and why the server samples
  every tick even with nobody watching: a skipped sample widens the window, and the
  next reading averages over the idle spell while reporting `Set` true.
- This is the only package that imports `gopsutil`, and it must stay that way.

## Work Guidance

- A new metric is a field with a `json` tag plus a read in `Sample`; adding it to
  the template is a separate, optional step.
- Failures are logged and leave the field zero rather than returning an error. A
  stats sample is not worth failing a request over — `Set` is how a caller knows
  not to trust it.

## Verification

- `go test -race ./sysstat/`
- `curl -s localhost:3000/api/state | jq .Stats.System` must show every field with
  its lower-camel name; those names are the wire contract.
