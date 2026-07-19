# engine

## Purpose

Wraps `anacrolix/torrent` in a small, server-friendly facade: one `*torrent.Client`, a mutable
`Config`, and a cache of `*Torrent` view models keyed by hex infohash. It knows nothing about HTTP.

## Ownership

- `engine.go` — the `Engine` type: client lifecycle (`Configure` and its steps `clientConfig`,
  `evictForRebind`, `buildClient`, `installClient`, `closeClient`), adding torrents (`NewMagnet`,
  `NewTorrentFile`, `addSpec`), the metadata watchers (`watchInfoLocked`, `infoArrived`), the `ts`
  cache and its sampler (`GetTorrents`, `refresh`, `sampleLoop`), and start/stop/delete
- `torrent.go` — the `Torrent` and `File` view models, `update`, `sample`, `clone`, `percent`
- `config.go` — the `Config` struct the server persists as JSON, plus `Validate`

## Local Contracts

Boundaries:

- Bottom of the dependency chain: must not import `server` or `static`, and `anacrolix/torrent`
  types must not appear in exported signatures. Torrent parsing lives here for that reason.
- `Torrent`, `File` and `Config` exported field names are marshalled straight to the browser and
  round-trip through the settings form — they are the UI and wire contract.
- Errors are sentinels (`ErrMissingTorrent`, `ErrAlreadyStarted`, …) wrapped with `%w`, in ordinary
  lowercase Go; `server.classify` decides status and presentation.
- **Anything the caller caused wraps `ErrInvalidInput`** — bad magnet URI, unparseable `.torrent`
  bytes, bad infohash, invalid config value. It is what lets the server show the wrapped detail while
  everything else gets a fixed message. Forgetting it turns a caller mistake into a 500.
- `str2ih` checks the infohash length *before* decoding: `hex.Decode` is bounded by `len(src)`, not
  `len(dst)`, so an over-long input writes past the array and panics.
- `addSpec` rejects a spec with no infohash. `AddTorrentSpec` *panics* on one, and the magnet parser
  produces one from input like `magnet:?nonsense`, so anyone reaching `/api/add` could otherwise
  take down a request handler.

Locking and lifecycle:

- Every exported method takes `e.mu`; helpers suffixed `Locked` assume the caller holds it.
- **`Configure` is serialized end to end by `configureMu`.** `mu` alone is not enough: the same-port
  path releases `mu` between dropping the old client and installing the replacement, and a second
  caller in that window sees `client == nil`, takes the non-retrying branch and steals the port.
- **`Close` is a one-way door and takes `configureMu`.** Without that lock it observes
  `client == nil` mid-rebind, reports a clean shutdown, and the in-flight `Configure` then installs a
  live client — socket, goroutines, DHT — into an engine everyone believes is down. `Configure` after
  `Close` returns `ErrClosed`. `Close` also clears `ts`, so a closed engine reports no torrents.
- **`Close` must release `mu` before `wg.Wait()`**: the sampler takes `mu` every tick, so waiting
  under it deadlocks.
- `wg.Add` is safe against that `Wait` because watchers register under `mu` after an `e.ctx.Err()`
  check and `Close` cancels before taking `mu`, and because the sampler's `Add` is in `New`, before
  the pointer is reachable. Unsynchronized `Add` is the documented "Add called concurrently with
  Wait" misuse and panics at a zero counter.
- **A rebind must never be cancellable by the request that triggered it.** `Configure` takes no
  context and must not grow one: aborting between evicting the old client and installing the
  replacement leaves the engine with none, so a user navigating away mid-save takes their own client
  down. The only cancellation is `e.ctx`, cancelled only by `Close`, so shutdown does not wait out a
  retrying rebind.
- The order of `Configure`'s four steps is the contract, not an implementation detail. Keep them
  separate — the ordering is only reviewable when each step reads on its own.
- Teardown order depends on whether the listen port changes. Different port: build the replacement
  first, so a failure leaves the running client untouched. Same port (the common case, since any
  other settings change keeps the port): close and wait on the old client first, or the bind fails
  with "address already in use". `Closed()` is necessary but not sufficient — the kernel releases the
  socket slightly later — hence the bounded `rebindTimeout` retry.

Torrents:

- **Every torrent without metadata has exactly one watcher, registered under `mu` by
  `watchInfoLocked`, and `infoArrived` is the only place the post-metadata decision is made.**
  Registration is unconditional, not gated on `AutoStart`: the watcher is also what calls
  `DownloadAll` for a torrent started before its metadata landed, which `startLocked` cannot do.
  Re-added magnets need a fresh watcher — the original is parked on the previous handle.
- If `installClient` fails to re-add a torrent it clears `Started`. Left set, the torrent shows as
  running against a client that never accepted it, with `t.t` nil, so nothing would ever move it.
- Stopping is destructive: `StopTorrent` drops the underlying torrent and clears `t.t` rather than
  pausing. `StartTorrent` re-adds from the retained `spec`, so start-after-stop works.
- Start/stop is per torrent, never per file: `anacrolix/torrent` has no per-file pause that composes
  with `DownloadAll` and the engine tracks no per-file priorities, so a per-file API could only be a
  lie. `File` is a read-only progress view.

Sampling:

- **`GetTorrents` is a pure read**: a deep copy of the last sample, touching neither the client nor
  any torrent's reading. The `ts` map and its `*Torrent` values never escape the engine.
- **The engine owns its cadence.** `refresh` is the only thing that samples and `sampleLoop` (started
  in `New`, ticking at `sampleInterval`) the only thing that calls it, so the interval between
  readings *is* the window `DownloadRate` is measured over.
- **There is deliberately no exported way to force a refresh.** If readers could sample, every extra
  reader (`/api/state`, opening a Files panel) would insert a reading just after the loop's, consume
  the interval the next real sample needed, and drive displayed rates toward zero.
- **Sampling is not gated on whether anyone is watching**, and must not be: the first reading after
  an idle spell would be an average over however long nobody was connected.
- `Downloaded`, `updatedAt` and `DownloadRate` are one sample — `Torrent.sample` moves all three or
  none. Its `dt <= 0` guard is arithmetic safety, not debouncing: one `refresh` pass shares a
  timestamp, and a zero interval yields `+Inf`, which fails `json.Marshal` and freezes the UI.
- Across a rebind the sampler skips ticks where `client == nil`, so the next tick spans ~2s. That
  stays correct because the rate comes from real timestamps; do not "optimise" `sample` into
  `db / sampleInterval`.
- `Torrent.Files` is rebuilt from the live torrent every pass. Patching in place assumes index *i* is
  the same file across ticks, which stops being true once a torrent is re-added. `File` is a value,
  so a nil entry is unrepresentable.
- `percent` truncates rather than rounds, and that is load-bearing: `torrents.html` tests
  `eq .Percent 100.0` for completeness, so rounding would mark a file done at 99.999%.

## Work Guidance

- Return `error` for invalid input rather than panicking — the server surfaces these as HTTP 4xx.
- `infoArrived`'s `DownloadAll` call ships verified by inspection, not by test: anacrolix exposes no
  observable effect of it, so every available assertion passes with the branch removed.

## Verification

- `go build ./...`
- `go test -race ./...`

## Child DOX Index

No children.
