# configfile

## Purpose

Loads and atomically persists the engine configuration. Three functions over a
path; no state, no HTTP.

## Ownership

- `configfile.go` — `Defaults`, `Load`, `Save`, the unexported `writeAtomic`, and `defaultIncomingPort`

## Local Contracts

- **Named for the file, not the type.** The config itself lives in the engine and
  nowhere else. A package called `config` reads as though it owns
  `engine.Config`, which is the invitation to grow a second copy of it —
  precisely what `server/CLAUDE.md` warns against.
- **Free functions taking a path, not a `Store` type.** There are three call
  sites. A type would invite caching the config here, which is that second copy
  again.
- **`Save` writes a sibling temp file and renames.** An interrupted
  write-in-place — a crash, a full disk, a container stop — leaves a truncated
  file that `Load` rejects as malformed, and the server then refuses to start
  until someone deletes it by hand. The temp file is created in the *target's*
  directory because rename is only atomic within a filesystem, and it is
  `Sync`ed before the rename so the metadata cannot land ahead of the bytes.
- `writeAtomic` returns raw errors and `Save` wraps them once. Wrapping at each
  step was six identical `fmt.Errorf` calls saying the same thing. Keep the
  wrap text as it is: `server/errors_test.go` uses `"failed to save
  configuration: …"` as its example of an operational error whose detail must
  not reach the user.
- **`Load` does not clamp.** It unmarshals over `Defaults()`, so an absent field
  already keeps its default — a clamp could only ever fire on a value someone
  explicitly wrote, and silently rewriting that is worse than reporting it.
  Port validity is `engine.Config.Validate`'s call and nowhere else; two
  policies for one rule end up disagreeing.
- A missing or empty file yields the defaults; malformed JSON is an error. A
  first run is a normal state, a corrupt file is not.
- This package may import `engine` (for the type) and nothing else beyond the
  stdlib. It must never import `server`, and `engine` must never import it.

## Work Guidance

- A new configuration field is added to `engine.Config` and, if it needs a
  non-zero default, to `Defaults` here. Nothing else in this package changes —
  it marshals whatever the struct holds.
- Do not add a migration or versioning scheme without a reason to: unmarshalling
  over the defaults already handles fields that did not exist when the file was
  written.

## Verification

- `go test -race ./configfile/`
- `go test -race ./server/` — `TestNewDoesNotWriteConfig`,
  `TestNewWithNoConfigCreatesNone` and `TestBadPortInConfigIsReported` cover how
  the server uses this package.

## Child DOX Index

No children.
