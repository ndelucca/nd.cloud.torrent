# files

## Purpose

Walks the download directory and serves what is in it. Takes a root and answers
questions about the tree below it; knows nothing about torrents, rendering or
authorization.

## Ownership

- `files.go` — `Node`, `Limit`, `ErrOutsideRoot`, `List`, `ResolveWithin`,
  `isWithin`, `visible`, and the bounded `list` walk
- `serve.go` — `Handler` (GET/HEAD over `/download/`), `Remove`, `sandbox`,
  `serveZip`

## Local Contracts

- **`Handler` is read-only, and deliberately.** Deleting lives in `Remove`, a
  plain function, so that mounting the handler cannot expose a destructive
  operation by accident. `Handler` answers 405 to anything that is not GET or
  HEAD. Do not add a mutating method to it.
- **Nothing here performs authorization.** `Remove` deletes for anyone who calls
  it; the gate is `server.requireSameOrigin`, middleware wrapping the whole mux
  by method. The server routes `DELETE /download/{path...}` through `apiRoute`
  so the reply is an `api-ok`/`api-error` fragment like every other mutation —
  answering it from `Handler` meant a 200 with an empty body on success and a
  500 on failure, neither of which htmx reports.
- **Every user-supplied path goes through `ResolveWithin`** — `Handler` and
  `Remove` alike. A
  `strings.HasPrefix` check is not enough — it has no separator boundary, so
  `<root>-backup/secret` passes it. `ResolveWithin` uses `filepath.Rel` *and*
  resolves symlinks and re-checks, because a link inside the download directory
  can otherwise point anywhere on disk.
- Rejections return `ErrOutsideRoot` and callers must answer with a generic 404.
  Echoing the resolved path back turns every rejected probe into a
  filesystem-layout oracle.
- `Handler.Root` is a `func() string`: `/api/configure` can move the download
  directory at any time and a captured copy would serve from the old one. The
  request path is read as relative to that root, so mount behind
  `http.StripPrefix`.
- The walk is bounded by `Limit`; hitting it sets `Truncated` rather than failing.
  `List` returns an empty `Node` for a missing or unreadable root — a download
  directory that does not exist yet is a normal state.
- **`visible` is applied to a directory's *entries*, never to the walk root — in
  `List` and `serveZip` alike.** Dotfiles and non-regular entries are skipped so
  the tree and its zip agree on what exists, while testing the root would make a
  directory the operator deliberately named `.torrents` walk or zip up empty.
- `serveZip` checks the request context on every entry, so a client that navigates
  away does not leave the walk and its file reads running with nowhere to write
  them. There is deliberately no entry or byte cap: archiving a large directory is
  the feature, and a cap could only produce a silently truncated archive.
- **Everything served over GET/HEAD carries `Content-Security-Policy: sandbox`.**
  Content-Type comes from the file extension, so a torrent containing an
  `index.html` is served as `text/html` from the app's own origin — and `nosniff`
  does not help when the declared type *is* `text/html`. That script would run
  same-origin and could drive every `/api/*` mutation and `DELETE /download/*`.
  The header lives here rather than in the server's middleware so it travels with
  the bytes and cannot be lost by a future mount; it is ignored for non-document
  responses, so image, audio and video previews are unaffected. It does not cover
  the app's own pages, which have their own policy — `server.appCSP`, whose
  directives and reasoning belong to `server/CLAUDE.md` and are deliberately not
  repeated here. What matters at this boundary is only the ordering: the
  middleware sets `appCSP` before this handler runs, so the `Set` here wins and
  downloaded content ends up strictly more constrained than the app.

## Work Guidance

- Keep this package free of `engine`, `server` and rendering concerns — a `Node`
  is a filesystem fact.
- Path-containment and sandbox-header changes are security-relevant: add the case
  to the tests and watch it fail before fixing it.

## Verification

- `go test -race ./files/`
- Manual, against a running server: a nested file downloads, a directory returns a
  zip, `Range` answers 206, `../../etc/passwd` answers 404, and a cross-origin
  `DELETE` answers 403 while a same-origin one succeeds.
- Manual: drop an `index.html` containing a `<script>` into the download
  directory, open it under `/download/`, and confirm the browser console reports
  the script blocked by CSP.
