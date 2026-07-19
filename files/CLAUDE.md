# files

## Purpose

Walks the download directory and serves what is in it. Takes a root and answers
questions about the tree below it; knows nothing about torrents, rendering or
authorization.

## Ownership

- `files.go` — `Node`, `Limit`, `List`, `ResolveWithin`, `isWithin`, and the bounded `list` walk
- `serve.go` — `Handler` (GET/HEAD/DELETE over `/download/`), `sandbox` and `serveZip`

## Local Contracts

- **`Handler` performs no authorization and must never be mounted on a mutating route without a caller-side gate.** DELETE here removes files from disk for anyone who reaches it. `server.serveDownload` is that gate and checks same-origin before delegating; the split moved the check there deliberately, so CSRF policy lives in one package. Any new mutating method added here inherits the assumption.
- **Every user-supplied path goes through `ResolveWithin`.** A `strings.HasPrefix` check is not enough — it has no separator boundary, so `<root>-backup/secret` passes it. `ResolveWithin` uses `filepath.Rel` *and* resolves symlinks and re-checks, because a link inside the download directory can otherwise point anywhere on disk.
- Rejections return `ErrOutsideRoot` and callers must answer with a generic 404. Echoing the resolved path back turns every rejected probe into a filesystem-layout oracle.
- `Handler.Root` is a `func() string`, not a string: `/api/configure` can move the download directory at any time and a captured copy would serve from the old one.
- `Handler` reads the request path as relative to the root, so it is mounted behind `http.StripPrefix`.
- The walk is bounded by `Limit`. Hitting it sets `Truncated` rather than failing, so the UI can say the listing is partial instead of presenting it as complete.
- `List` returns an empty `Node` for a missing or unreadable root. A download directory that does not exist yet is a normal state, not an error.
- **`visible` is applied to a directory's *entries*, never to the walk root.** Dotfiles and non-regular entries are skipped. Testing the root itself made a download directory named `.torrents` abort the entire walk — logging a failure once per poll tick and rendering an empty tree, for a directory the operator deliberately chose.
- **Everything served over GET/HEAD carries `Content-Security-Policy: sandbox`.** Content-Type is derived from the file extension, so a torrent containing an `index.html` is served as `text/html` from the app's own origin, and `nosniff` does not help when the declared type *is* `text/html` — that script would run same-origin and could drive every `/api/*` mutation and `DELETE /download/*`. The header lives here rather than in the server's middleware so it travels with the bytes and cannot be lost by a future mount; it is ignored for non-document responses, so image, audio and video previews are unaffected. This does not cover the app's own pages, which is a separate problem (Alpine needs `unsafe-eval`).

## Work Guidance

- Keep this package free of `engine`, `server` and rendering concerns. Presentation over the tree (`fsView`, the change-detection signature) belongs to the rendering layer, not here — a `Node` is a filesystem fact.
- Path-containment changes are security-relevant: add the case to `TestResolveWithin` and watch it fail before fixing it. The same applies to the sandbox header — `TestServedContentIsSandboxed` was verified to fail without it.

## Verification

- `go test -race ./files/`
- Manual, against a running server: a nested file downloads, a directory returns a zip, `Range` requests answer 206, `../../etc/passwd` answers 404, and a cross-origin `DELETE` answers 403 while a same-origin one succeeds.
- Manual: drop an `index.html` containing a `<script>` into the download directory, open it under `/download/`, and confirm the browser console reports the script blocked by CSP.

## Child DOX Index

No children.
