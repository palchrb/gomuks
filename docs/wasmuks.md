# Hosting gomuks web with the embedded backend (wasmuks)

The wasm build runs the whole gomuks backend inside the browser: sync,
encryption and the SQLite database all live in a Web Worker, with the database
stored in the browser's origin private file system (OPFS). The server only
serves static files and never sees access tokens, encryption keys or message
content, so one static site can serve any number of people, each logging in
to their own Matrix account.

## Building

```sh
cd web
./build-wasm.sh      # builds src/api/wasm/_gomuks.wasm (needs Go)
npm ci
npm run build        # writes web/dist/
```

## Running with Docker

The easiest way to host it is the prebuilt image: a single static Go binary
(`cmd/wasmukserve`) with the frontend embedded, the same way the native gomuks
server embeds its frontend.

```sh
cp config.example.json config.json           # optional, see below; do it before the first "up"
docker compose -f docker-compose.wasmuks.yml pull
docker compose -f docker-compose.wasmuks.yml up -d
# http://127.0.0.1:8181
```

If `config.json` doesn't exist when the container first starts, Docker creates
an empty *directory* with that name and the config is ignored (the server
logs a warning). Remove the directory, create the file, and restart.

The image is built by the "Docker (wasmuks)" GitHub Actions workflow for amd64
and arm64 on every push and published to `ghcr.io/<owner>/gomuks-web`, tagged
with the branch name (`latest` for `main`; slashes become dashes). Put
`GOMUKS_WEB_TAG=<tag>` in a `.env` file next to the compose file to run a
branch build. It can also be built
locally with `docker build -f Dockerfile.wasmuks .`, but that needs a few GB of
RAM: **don't build on a Raspberry Pi**, pull the image instead.

## Serving without Docker

Build the frontend as above, optionally pre-compress it, and build the server
binary, which embeds `web/dist/`:

```sh
cd web && find dist -type f \( -name '*.wasm' -o -name '*.js' -o -name '*.css' -o -name '*.html' -o -name '*.json' -o -name '*.svg' \) -exec gzip -9 -k {} + && cd ..
go build -o wasmukserve ./cmd/wasmukserve
./wasmukserve -listen 127.0.0.1:8181 -config config.json
```

`wasmukserve` serves the pre-compressed files to clients that accept gzip,
sets the cache headers below, and serves `config.json` from the given path
so it can be edited without rebuilding. `-dir` serves a directory instead of
the embedded files.

Any other static file server works too: serve `web/dist/` as static files. The wasm mode is enabled
automatically when the files are served statically (the Go server injects a
`gomuks-frontend-etag` meta tag that turns it off).

Recommended headers:

* `index.html`, `config.json`, `wasmuks-media-sw.js`: `Cache-Control: no-cache`
* everything under `assets/`: `Cache-Control: public, max-age=31536000, immutable`
  (file names are content-hashed)
* enable brotli or gzip; the wasm binary is ~30 MB uncompressed, ~7 MB gzipped,
  and browsers cache the compiled module after the first load

Cross-origin isolation headers (COOP/COEP) are not required: the OPFS
SAH pool VFS doesn't use SharedArrayBuffer, and `Cross-Origin-Embedder-Policy:
require-corp` would block images from homeservers that don't send CORP headers.

The homeserver must allow cross-origin requests from the browser (the Matrix
spec requires this and all common homeservers do it by default).

## Default preferences

Put a `config.json` next to `index.html` to set defaults for everyone using the
site. Keys are the preference names from the settings screen; users can still
override each one per account, device or room.

```json
{
	"preferences": {
		"show_membership_events": false,
		"show_hidden_events": false
	}
}
```

Unknown keys and values of the wrong type are ignored with a warning in the
browser console.

The same file can tune the wasm backend in a `wasm` section (see
`config.example.json`):

| Key | Default | Meaning |
|---|---|---|
| `single_connection` | `true` | One SQLite connection with EXCLUSIVE locking (fastest per query, but reads wait for running write transactions) or five connections with normal locking. Compare on your own account. |
| `memory_limit_mb` | `512` | Soft limit for the Go heap in the worker. The garbage collector works harder near it. |
| `initial_timeline_limit` | `20` | Events per room requested in the initial sync. Lower is less memory and a faster first sync; older history is loaded when scrolling. |
| `log_level` | `debug` | Backend log level in the browser console (`trace`, `debug`, `info`, `warn`, `error`). `debug` logs every decrypted event; `info` keeps only the resource and timing lines below. |

The backend logs the effective values ("wasm configuration") and heap
statistics every 30 seconds in the browser console, and commands that take
longer than 200 ms ("Slow command"), so slowness can be measured rather than
guessed.

## Client requirements

Everything runs in the browser, so the *client* machine does the work:
V8 compiles a 30 MB wasm module and the Go heap holds the sync state. Plan
for roughly 1-2 GB of free RAM in the browser for accounts with many rooms,
and a 64-bit browser. A Raspberry Pi is a fine place to *serve* the files
from, but a poor place to *use* them: for someone who uses the Pi itself as a
client, run the native `gomuks` server on the Pi for that account instead
(about 50 MB of RAM) and use the wasm build from phones and laptops.

## Persistent storage

Browsers may evict a site's storage under pressure or after inactivity unless
it's "persistent". gomuks asks for that on start and shows a banner if it was
refused. How to get it depends on the browser:

* **Firefox** shows a permission prompt (the banner's button triggers it).
* **Chrome** never prompts. It grants persistence silently to sites that are
  installed as an app, bookmarked, or allowed to send notifications. gomuks
  asks again after the notification permission is granted.
* **Safari** grants it to web apps added to the home screen.

If the data is evicted anyway, the next start shows the login screen with an
explanation; the room list cache is discarded and the session must be
verified again like any new device.

## Encryption keys for old messages

A new session can't read old encrypted history until it has the room keys.
Verifying the session with a recovery key restores them from the server-side
key backup: in the wasm build this runs as an RPC command (the native server
uses an HTTP endpoint) and shows progress in Settings › Key export/import ›
Restore from backup. Without a full restore, keys are fetched from the backup
one session at a time as messages fail to decrypt, which is slow for a large
history.

## Measuring resource use

Everything is logged to the browser console. The default level is `debug`,
which is very chatty (every decrypted event); set `"log_level": "info"` in
`config.json` to keep only the lines below, or type this in the console's
filter box: `/Memory stats|wasm compile|wasm configuration|Initial room list|Slow command|Slow sync|Key backup restore|Storage persistence/`.
Open DevTools before loading the page and look for:

| Line | Meaning |
|---|---|
| `wasm compile+instantiate: N ms` | Time V8 spent compiling the 30 MB module. Should be well under a second on a PC. |
| `wasm configuration` | Effective `config.json` values (connections, memory limit, timeline limit). |
| `Initial room list sent` | Time from Go start to the room list, number of rooms, and heap size right after the initial sync. |
| `Memory stats` | Go heap every 30 seconds (`heap_alloc_mb` is live data, `heap_sys_mb` is what wasm memory has grown to and never shrinks). |
| `Slow command` / `Slow sync processing` | Any frontend request over 200 ms or sync batch over 500 ms, with its duration. |
| `Key backup restore finished` | Sessions restored and how long saving took. |

For the browser's own view, use Chrome's Task Manager (Shift+Esc): the
renderer row for the tab includes V8's compiled code and the frontend. Note
it right after the initial sync and again after ten minutes of use: a number
that keeps climbing points to a leak, one that flattens is the platform cost.

To compare database modes on your own account, set
`"wasm": {"single_connection": false}` in `config.json`, reload, and compare
`Slow command` lines and how quickly rooms open.

## Later

Not done, kept as options:

* **Push notifications** via a stateless Matrix push gateway in
  `wasmukserve`: the frontend would register a pusher with
  `format: event_id_only` and the Web Push subscription in the pusher data, so
  the server forwards "new message in room X" without ever holding keys or
  content.
* **Smaller binary**: `wasm-opt -Oz` and leaving syntax highlighting and
  markdown rendering out of the wasm build (roughly 20-25 % smaller, which
  also shrinks V8's compiled code proportionally).
* **Native gomuks per user** as a compose file, for people who use the
  server machine itself as a client.

## Limitations compared to the server build

* **No private browsing in Firefox.** Firefox private windows don't provide
  the origin private file system the database lives in; Chrome's incognito
  does, but wipes it when the window closes.
* **One tab at a time.** The database can only be opened by one worker; a second
  tab shows a message asking to close the first one.
* **No push notifications and no sync while the tab is closed.** Use a native
  Matrix client on the phone for push; Matrix accounts can have many devices.
* **Storage can be evicted.** If the browser refuses persistent storage (shown
  as a banner), it may delete the local database after a period of inactivity.
  Safari does this after seven days unless the site is added to the home screen.
* **No HEIC thumbnails.** HEIC decoding needs cgo and isn't available in wasm.
* Performance depends on the browser's OPFS implementation. The driver is
  benchmarked in `pkg/sqlite-wasm-js/bench`.

## Data on shared devices

Logging out wipes the OPFS database, the decrypted media cache and the
IndexedDB room list cache from the browser.
