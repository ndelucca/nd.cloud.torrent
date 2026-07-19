# engine

## Purpose

Wraps `anacrolix/torrent` in a small, server-friendly facade: one `*torrent.Client`, a mutable `Config`, and a cache of `*Torrent` view models keyed by hex infohash.

## Ownership

- `engine.go` — `Engine` type: client lifecycle (`Configure` and its steps `clientConfig`/`evictForRebind`/`buildClient`/`installClient`/`closeClient`), adding torrents (`NewMagnet`, `NewTorrentFile`), the `ts` cache (`GetTorrents`, `upsertLocked`), and start/stop/delete for torrents
- `torrent.go` — `Torrent` and `File` view models, `update`/`updateLoaded` (copy live state out of the underlying torrent), `sample` (the atomic progress reading) and `clone` (deep copy for callers)
- `config.go` — the `Config` struct persisted by the server as JSON, plus `Validate`

## Local Contracts

- `Engine` must not import `server` or `static`; it is the bottom of the dependency chain
- Public methods take a hex infohash string; `str2ih` checks the length *before* decoding — `hex.Decode` is bounded by `len(src)`, not `len(dst)`, so an over-long input writes past the array and panics
- `Configure` is serialized end to end by `configureMu`. `mu` alone is not enough: the same-port path releases `mu` between dropping the old client and installing the replacement, and a second caller in that window saw `client == nil`, took the non-retrying branch and stole the port. `TestConcurrentConfigure` pins it, and fails five times out of five when `configureMu` is removed — burning the full `rebindTimeout` each time, which is exactly the failure this describes.
- `Close` clears `ts`. Without it, `GetTorrents` on a closed engine kept returning torrents that no longer existed anywhere.
- **`Close` is a one-way door and takes `configureMu`.** Without that lock it observed `client == nil` mid-rebind — the same-port path having already evicted it — reported a clean shutdown, and the `Configure` still in flight then installed a live client, with its listening socket, goroutines and DHT, into an engine everyone believed was down. `Configure` after `Close` returns `ErrClosed` rather than building a client nothing would release. `TestConfigureAfterCloseDoesNotLeak` and `TestCloseDuringConfigure` pin both halves and fail without the fix.
- **A rebind must never be cancellable by the request that triggered it.** `Configure` takes no context and must not grow one: aborting between evicting the old client and installing the replacement leaves the engine with none, so a user navigating away mid-save would take their own torrent client down. The only cancellation wired in is `e.ctx`, which only `Close` cancels, so shutdown does not have to wait out a retrying rebind.
- `Configure` is a sequence of four named steps — `clientConfig`, `evictForRebind`, `buildClient`, `installClient` — and the order between them is the contract, not an implementation detail. Keep them separate: the ordering is the part that was historically wrong, and it is only reviewable when each step is small enough to read on its own.
- `Configure` picks its teardown order by whether the listen port changes. Different port: build the replacement first, so a failure leaves the running client untouched. Same port: the old client holds the port, so it must be closed and waited on first — building first fails with "address already in use" every time, which is the common case since any other settings change keeps the port. Waiting on `Closed()` is necessary but not sufficient (the kernel releases the socket slightly later), hence the bounded rebind retry.
- **Every torrent without metadata has exactly one watcher, registered under `mu` by `watchInfoLocked`, and `infoArrived` is the only place the post-metadata decision is made.** Registration is unconditional, not gated on `AutoStart`: the watcher is also what calls `DownloadAll` for a torrent the user started before its metadata landed — `startLocked` cannot, and the old `AutoStart`-only registration plus a watcher body that bailed out whenever `Started` was set left that torrent flagged as running while downloading nothing, permanently. Re-added magnets need a fresh watcher for the same reason: the original is parked on the old handle and correctly declines to act.
- Registering under `mu` is also what makes `wg.Add` safe against `Close`'s `wg.Wait`. `Close` cancels, then takes `mu` before waiting, so a watcher is either already counted or never registered; `addSpec` previously called `wg.Add` under no lock `Close` held, which is the documented "Add called concurrently with Wait" misuse and panics when the counter is at zero.
- If `installClient` fails to re-add a torrent it clears `Started`. Leaving it set showed the torrent as running against a client that never accepted it, with `t.t` nil, so nothing would ever move it again.
- `Torrent`/`File` fields are read by the server and marshalled straight to the browser; treat exported field names as part of the UI contract
- Stopping is destructive: `StopTorrent` drops the underlying torrent rather than pausing it, and clears `t.t`. `StartTorrent` re-adds from the retained `spec`, so start-after-stop works.
- **`GetTorrents` refreshes the cache and then returns a deep copy — it is not a pure read**, and callers must not assume otherwise. The internal `ts` map and its `*Torrent` values never escape the engine.
- **`Downloaded`, `updatedAt` and `DownloadRate` are one sample: `Torrent.sample` moves all three or none.** Because reads refresh, every caller produces a reading — the poll loop, `GET /api/state`, and opening a torrent's Files panel. The old code advanced `Downloaded`/`updatedAt` on all of them while recomputing the rate only when the interval was positive, so an extra reader consumed the interval the next real sample needed and drove the displayed rate toward zero; two clients polling at 1 Hz roughly halved every rate on the page. A reading closer than `minRateInterval` is now dropped whole. `TestExtraReadsDoNotDisturbTheRate` pins it.
- `Torrent.Files` is rebuilt from the live torrent on every pass, not patched in place. Patching assumed index *i* is the same file across ticks, which is untrue once a torrent is re-added and was written down nowhere. `File` is a value, so a nil entry — and the nil check every consumer carried — is unrepresentable.
- `percent` truncates rather than rounds, and that is load-bearing: `torrents.html` tests `eq .Percent 100.0` to decide whether a file is complete, so rounding would mark a file done at 99.999%.
- Errors are sentinels (`ErrMissingTorrent`, `ErrAlreadyStarted`, …) wrapped with `%w`; the server maps them to HTTP status codes
- Start/stop is per torrent, never per file. `anacrolix/torrent` has no per-file pause that composes with `DownloadAll`, and the engine tracks no per-file priorities, so a per-file API could only ever be a lie. `File` is a read-only progress view.
- Torrent parsing lives here, not in `server`: `anacrolix/torrent` types must not appear in exported signatures

## Work Guidance

- Every exported method takes `e.mu`; helpers suffixed `Locked` assume the caller already holds it
- `Close` cancels the metadata watchers, clears the cache and releases the client; the server calls it on shutdown
- `infoArrived`'s `DownloadAll` call has no test: anacrolix exposes no effect of it (piece priorities read back unchanged), so every available assertion passes with the branch removed. It ships verified by inspection, and `TestStartedWithoutMetadataDownloadsOnArrival` says so rather than implying coverage it does not have.
- Return `error` for invalid input rather than panicking — the server surfaces these directly as HTTP 400 text

## Verification

- `go build ./...`
- `go test -race ./...`

## Child DOX Index

No children.
