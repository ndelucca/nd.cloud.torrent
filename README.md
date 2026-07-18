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
docker run -d -p 3000:3000 -v /path/to/downloads:/downloads nd-cloud-torrent
```

The image runs as a non-root user; make sure the mounted directory is writable
by it.

## Usage

```
$ nd-cloud-torrent --help

  Usage: nd-cloud-torrent [options]

  Options:
  --title, -t        Title of this instance (default Cloud Torrent, env TITLE)
  --port, -p         Listening port (default 3000, env PORT)
  --host, -h         Listening interface (default all)
  --auth, -a         Optional basic auth in form 'user:password' (env AUTH)
  --config-path, -c  Configuration file path (default cloud-torrent.json)
  --key-path, -k     TLS Key file path
  --cert-path, -r    TLS Certicate file path
  --log, -l          Enable request logging
  --open, -o         Open now with your default browser
  --help
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

## HTTP endpoints

| Path | Purpose |
|---|---|
| `/` | the web UI |
| `/events` | Server-Sent Events: named events carrying HTML fragments |
| `/api/state` | the full server state as JSON — for scripts and monitoring |
| `/api/*` | commands: `add`, `magnet`, `url`, `torrentfile`, `configure`, `torrent`, `file` |
| `/download/<path>` | file download (range requests supported); a directory streams as a zip |

`/api/*` requires `POST` and rejects cross-origin requests.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Credits

Original project by [Jaime Pillora](https://github.com/jpillora). Torrent engine
by [@anacrolix](https://github.com/anacrolix/torrent).

## License

AGPL-3.0. See [LICENSE](LICENSE).
