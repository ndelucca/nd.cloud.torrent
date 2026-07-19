# nd.cloud.torrent

A self-hosted remote torrent client in Go. Start torrents on a machine you
control, download them to its disk, then browse, stream or fetch the files over
HTTP.

A fork of [jpillora/cloud-torrent](https://github.com/jpillora/cloud-torrent),
rewritten around a server-rendered UI.

## Features

- **Single binary.** No runtime dependencies, no build step, no `node_modules`.
  The UI, its assets and the torrent engine are all embedded.
- **Server-rendered UI.** `html/template` + [htmx](https://htmx.org) +
  [Alpine](https://alpinejs.dev), pushed over Server-Sent Events. ~150 KB of
  assets, of which most is the four pinned vendor bundles.
- **Live updates** without polling the whole state: only the regions that
  actually changed are re-sent.
- **Streamable downloads** through Go's `ServeContent`, so seeking works in the
  browser's media player.
- **Cross platform.** Linux, macOS and Windows, `CGO_ENABLED=0`.

## Install

### From source

Go 1.25 or newer:

```sh
go install github.com/ndelucca/nd.cloud.torrent@latest
```

Or build a checkout:

```sh
git clone https://github.com/ndelucca/nd.cloud.torrent
cd nd.cloud.torrent
go build -o nd-cloud-torrent .
```

### Docker

```sh
docker build -f .github/Dockerfile -t nd-cloud-torrent .
docker run -d -p 3000:3000 \
  -v /path/to/downloads:/app/downloads \
  -v /path/to/config.json:/app/cloud-torrent.json \
  nd-cloud-torrent
```

The paths matter. The image's working directory is `/app` and the default
download directory is the relative `./downloads`, so downloads land in
`/app/downloads` — mounting `/downloads` instead leaves them inside the
container, where they vanish on restart. The config mount is optional but
without it the settings you save do not survive one either.

The image runs as a non-root user (65534); make sure the mounted directory is
writable by it.

## Usage

```
$ nd-cloud-torrent --help

  Usage: nd-cloud-torrent [options]

  Options:
  --title, -t        Title of this instance (default Cloud Torrent, env TITLE)
  --port, -p         Listening port (default 3000, env PORT)
  --host             Listening interface (default all)
  --auth, -a         Optional basic auth in form 'user:password' (env AUTH)
  --config-path, -c  Configuration file path (default cloud-torrent.json)
  --key-path, -k     TLS Key file path
  --cert-path        TLS Certificate file path
  --log, -l          Enable request logging
  --open, -o         Open now with your default browser
  --help, -h
  --version, -v
```

Runtime settings — download directory, incoming port, upload and seeding — live
in `cloud-torrent.json` next to the binary and are editable from the Settings
panel in the UI.

### Exposing it

**Authentication is off by default.** With `--auth` unset, anyone who can reach
the port can add torrents, browse the download directory and delete files. Do
not put an unauthenticated instance on a public address.

For anything reachable from the internet, use `--auth` together with
`--cert-path`/`--key-path`, or put it behind a reverse proxy that terminates TLS
and handles authentication.

`--auth` uses HTTP basic auth to log in, then issues a session cookie holding a
random token — nothing derived from the password. The cookie is `HttpOnly` and
`SameSite=Lax`, and is marked `Secure` when the server is serving TLS. Sessions
last a fortnight, are checked against the clock on every request, and are held
in memory, so restarting the server ends them.

## HTTP endpoints

| Path | Purpose |
|---|---|
| `/` | the web UI |
| `/events` | Server-Sent Events: named events carrying HTML fragments |
| `/api/state` | torrents, the download tree, viewer count and host stats as JSON — for scripts and monitoring. Read live, so it is correct with no browser open. The engine configuration is not included; it is not the server's to publish. |
| `/api/*` | commands: `add`, `torrentfile`, `configure`, `torrent` |
| `/download/<path>` | file download (range requests supported); a directory streams as a zip |

`/api/*` requires `POST` and rejects cross-origin requests.

| Action | Body |
|---|---|
| `add` | a magnet URI or an `http(s)` URL to a `.torrent` — either as the raw body, or as a `uri` form field. The server dispatches on the scheme. |
| `torrentfile` | raw `.torrent` bytes, or a multipart upload under `torrent` |
| `configure` | form-encoded; omitted fields keep their current value |
| `torrent` | form-encoded `action` (`start`, `stop`, `delete`) and `infohash` |

Note that `curl -d` defaults to `Content-Type: application/x-www-form-urlencoded`,
so `add` will look for a `uri` field; use `--data-urlencode "uri=…"`, or send the
bare URI with `-H 'Content-Type: text/plain'`.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Credits

Original project by [Jaime Pillora](https://github.com/jpillora). Torrent engine
by [@anacrolix](https://github.com/anacrolix/torrent).

## License

AGPL-3.0. See [LICENSE](LICENSE).
