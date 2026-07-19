# engine

## Purpose

Wraps `anacrolix/torrent` in a small, server-friendly facade: one `*torrent.Client`, a mutable
`Config`, and a cache of `*Torrent` view models keyed by hex infohash. It knows nothing about HTTP.

## Ownership

- `engine.go` — the `Engine` type: client lifecycle (`Configure`, `needsRestart`, `clientConfig`),
  adding torrents (`NewMagnet`,
  `NewTorrentFile`, `addSpec`), the metadata watchers (`watchInfoLocked`, `infoArrived`), the `ts`
  cache and its sampler (`GetTorrents`, `refresh`, `sampleLoop`), and start/stop/delete
- `torrent.go` — `Torrent`/`TorrentWithFiles`/`File` (the exported views), `torrentState` (the
  internal record), `view`, `viewWithFiles`, `update`, `sample`, `percent`
- `config.go` — the `Config` struct the server persists as JSON, plus `Validate`

## Local Contracts

Boundaries:

- Bottom of the dependency chain: must not import `server` or `static`, and `anacrolix/torrent`
  types must not appear in exported signatures. Torrent parsing lives here for that reason.
- `Torrent`, `TorrentWithFiles`, `File` and `Config` exported field names are marshalled straight to
  the browser and round-trip through the settings form — they are the UI and wire contract.
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
- **`Configure` runs entirely under `mu`**, so it is atomic and needs no second lock. Keep it that
  way: the moment work moves outside the lock there is a window in which the engine has no client,
  and a concurrent caller entering it steals the port.
- **`Close` is a one-way door.** `Configure` after `Close` returns `ErrClosed`, checked under `mu`
  after `cancel`, so a `Configure` racing shutdown either installed its client before `Close` took
  the lock — and `Close` releases it — or finds the context already cancelled. `Close` also clears
  `ts`, so a closed engine reports no torrents.
- **`Close` must release `mu` before `wg.Wait()`**: the sampler takes `mu` every tick, so waiting
  under it deadlocks.
- `wg.Add` is safe against that `Wait` because watchers register under `mu` after an `e.ctx.Err()`
  check and `Close` cancels before taking `mu`, and because the sampler's `Add` is in `New`, before
  the pointer is reachable. Unsynchronized `Add` is the documented "Add called concurrently with
  Wait" misuse and panics at a zero counter.
- **There is exactly one path that builds a client**, and it runs once: the first `Configure`, when
  `client == nil`. Nothing rebuilds a live client.
- **Everything the client bakes in at construction is fixed for the process lifetime**, and
  `Configure` reports `ErrRestartRequired` rather than pretending otherwise. Verified against
  `anacrolix/torrent` v1.59.1: `DataDir` is read exactly once, in `Client.init`, to build the default
  storage; `NoUpload`, `Seed` and `HeaderObfuscationPolicy` are read live but from a `*ClientConfig`
  the client *aliases* and its own goroutines touch with no lock we hold, so writing them is a data
  race and only partly effective anyway (`HeaderObfuscationPolicy` is snapshotted per dial); and
  `IncomingPort` is the listening socket. There are no `Client` setters for any of them.
- **`AutoStart` is the sole exception and applies live**, because the client never sees it —
  `infoArrived` is its only reader.
- **`needsRestart` is written as "copy across what applies live, then compare"**, not as a list of
  fields, so a field added to `Config` requires a restart by default. That is the safe direction and
  nobody has to remember it.
- `ErrRestartRequired` is not a failed request: the server persists the config anyway and reports a
  restart, because refusing to save would leave no way to change the listen port at all.

Torrents:

- **Every torrent without metadata has exactly one watcher, registered under `mu` by
  `watchInfoLocked`, and `infoArrived` is the only place the post-metadata decision is made.**
  Registration is unconditional, not gated on `AutoStart`: the watcher is also what calls
  `DownloadAll` for a torrent started before its metadata landed, which `startLocked` cannot do.
  Re-added magnets need a fresh watcher — the original is parked on the previous handle.
- Stopping is destructive: `StopTorrent` drops the underlying torrent and clears `t.t` rather than
  pausing. `StartTorrent` re-adds from the retained `spec`, so start-after-stop works.
- Start/stop is per torrent, never per file: `anacrolix/torrent` has no per-file pause that composes
  with `DownloadAll` and the engine tracks no per-file priorities, so a per-file API could only be a
  lie. `File` is a read-only progress view.

Sampling:

- **One torrent, three types, and the split is the point.** `torrentState` is the internal record
  and never leaves the package — it holds the live handle, the spec and `updatedAt`, so the internal
  state is *unrepresentable* outside rather than nil'd out by hand. `Torrent` is progress only, with
  **no file list**. `TorrentWithFiles` adds one.
- **`GetTorrents` is the hot path and carries no files.** It runs once per sample for every connected
  browser and the streamed row discards file tables, so copying every file into every row once a
  second was pure waste. `GetTorrentsWithFiles` is for `/api/state`, one document per request;
  `TorrentWithFiles(hash)` is a keyed lookup for the on-demand file fragment, so expanding one row
  costs one torrent's files rather than a copy of the whole engine.
- **`GetTorrents` is a pure read**: a copy of the last sample, touching neither the client nor any
  torrent's reading. The `ts` map and its values never escape.
- **The engine owns its cadence.** `refresh` is the only thing that samples and `sampleLoop` (started
  in `New`, ticking at `SampleInterval`) the only thing that calls it, so the interval between
  readings *is* the window `DownloadRate` is measured over. The server's render loop follows this
  clock rather than running one of its own — two independent 1 Hz timers drift, and a render between
  samples has nothing new to draw.
- **There is deliberately no exported way to force a refresh.** If readers could sample, every extra
  reader (`/api/state`, opening a Files panel) would insert a reading just after the loop's, consume
  the interval the next real sample needed, and drive displayed rates toward zero.
- **Sampling is not gated on whether anyone is watching**, and must not be: the first reading after
  an idle spell would be an average over however long nobody was connected.
- `Downloaded`, `updatedAt` and `DownloadRate` are one sample — `Torrent.sample` moves all three or
  none. Its `dt <= 0` guard is arithmetic safety, not debouncing: one `refresh` pass shares a
  timestamp, and a zero interval yields `+Inf`, which fails `json.Marshal` and freezes the UI.
- Before the first `Configure` the sampler ticks with `client == nil` and `refresh` returns at once.
  Rates come from real timestamps rather than the assumed cadence, so do not "optimise" `sample` into
  `db / SampleInterval`.
- **`Sampled` is a hint, not a queue.** One buffered slot, non-blocking send, never closed. The
  non-blocking send is load-bearing in two ways: a reader that is slow or absent must not stall
  sampling, and a blocking send parks `sampleLoop` so `Close`'s `wg.Wait()` deadlocks. The signal is
  emitted *outside* `refresh` so it also fires on ticks with no client — the render loop draws the
  download tree and host stats too, and those must keep moving while the engine is unconfigured.
- `Torrent.Files` is rebuilt from the live torrent every pass. Patching in place assumes index *i* is
  the same file across ticks, which stops being true once a torrent is re-added. `File` is a value,
  so a nil entry is unrepresentable.
- `percent` truncates rather than rounds, and that is load-bearing: `web.newFileView` tests
  `Percent >= 100` for completeness, so rounding would mark a file done at 99.999%.

## Work Guidance

- Return `error` for invalid input rather than panicking — the server surfaces these as HTTP 4xx.
- `infoArrived`'s `DownloadAll` call ships verified by inspection, not by test: anacrolix exposes no
  observable effect of it, so every available assertion passes with the branch removed.

## Verification

- `go build ./...`
- `go test -race ./...`

## Child DOX Index

No children.
