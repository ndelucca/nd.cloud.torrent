# configfile

## Purpose

Loads and atomically persists the engine configuration. No state, no HTTP.

## Ownership

- `configfile.go` — `Defaults`, `Load`, `Save`, `writeAtomic`, `defaultIncomingPort`

## Local Contracts

- **Named for the file, not the type.** `engine.Config` lives in the engine and
  nowhere else; a package called `config` reads as though it owns the type, which
  is the invitation to grow a second copy. Free functions over a path for the same
  reason — a `Store` type would invite caching the config here.
- **`Save` writes a sibling temp file and renames.** An interrupted write-in-place
  leaves a truncated file that `Load` rejects as malformed, and the server then
  refuses to start until someone deletes it by hand. The temp file goes in the
  *target's* directory because rename is only atomic within a filesystem, and is
  `Sync`ed before the rename so the metadata cannot land ahead of the bytes.
- `writeAtomic` returns raw errors and `Save` wraps them once. Keep the wrap text:
  `server` uses `"failed to save configuration: …"` as its example of an
  operational error whose detail must not reach the user.
- **`Load` does not clamp.** It unmarshals over `Defaults()`, so an absent field
  keeps its default; a clamp could only fire on a value someone explicitly wrote,
  and silently rewriting that is worse than reporting it. Port validity is
  `engine.Config.Validate`'s call and nowhere else — two policies for one rule end
  up disagreeing.
- A missing or empty file yields the defaults; malformed JSON is an error. A first
  run is normal, a corrupt file is not.
- May import `engine` and the stdlib, nothing else. Never `server`, and `engine`
  must never import it.

## Work Guidance

- A new field is added to `engine.Config` and, if it needs a non-zero default, to
  `Defaults`. Nothing else here changes — this package marshals whatever the
  struct holds, and unmarshalling over the defaults already covers fields that did
  not exist when the file was written.

## Verification

- `go test -race ./configfile/ ./server/`
