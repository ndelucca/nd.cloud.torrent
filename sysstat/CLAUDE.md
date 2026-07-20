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
- **A download directory that does not exist yet is not a failed sample.** The
  torrent client creates it lazily on the first write, so on a fresh install
  `disk.Usage` reports ENOENT — and one `Set` for the whole sample meant that hid
  CPU and memory too, neither of which had failed. `diskTarget` substitutes the
  parent, which is not a way of tolerating the miss but of answering the question:
  the directory will be created there, and a directory lands on the filesystem its
  parent is on, so the parent's free space is exactly what "room for downloads"
  means.
- **`diskTarget` climbs exactly one level, and only for a missing leaf.** Walking
  further would answer confidently about an ancestor with nothing to do with the
  target — a download directory under an unmounted `/mnt/bigdisk` would report the
  root filesystem as though it were the download disk. A path missing more than
  its last component is a broken configuration and is left to fail, which is what
  keeps "one bad source clears `Set`" a real contract rather than an unreachable
  one.
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
