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
cd web/dist
find . -type f \( -name '*.wasm' -o -name '*.js' -o -name '*.css' -o -name '*.html' -o -name '*.json' -o -name '*.svg' \) -exec gzip -9 -k {} +
find . -type f \( -name '*.wasm' -o -name '*.js' -o -name '*.css' -o -name '*.html' -o -name '*.json' -o -name '*.svg' \) -exec brotli -q 11 -k {} +
cd ../..
go build -o wasmukserve ./cmd/wasmukserve
./wasmukserve -listen 127.0.0.1:8181 -config config.json
```

`wasmukserve` serves the pre-compressed files, preferring brotli and falling
back to gzip, sets the cache headers below, and serves `config.json` from the
given path so it can be edited without rebuilding. `-dir` serves a directory
instead of the embedded files. Brotli is worth the minute it takes to
compress: 4.9 MB against 7.1 MB for the wasm binary, and that download is
most of a cold start.

Any other static file server works too: serve `web/dist/` as static files. The wasm mode is enabled
automatically when the files are served statically (the Go server injects a
`gomuks-frontend-etag` meta tag that turns it off).

Recommended headers:

* `index.html`, `config.json`, `wasmuks-media-sw.js`: `Cache-Control: no-cache`
* everything under `assets/`: `Cache-Control: public, max-age=31536000, immutable`
  (file names are content-hashed)
* enable brotli, or gzip if that is all the server has: the wasm binary is
  ~30 MB uncompressed, ~7 MB gzipped and ~4.9 MB with brotli, and it is
  fetched again after every deployment because its name is content-hashed
* do not add a `<link rel="preload">` or `rel="prefetch"` for the wasm binary.
  It is tempting, because the worker that fetches it starts late, but the
  frontend compiles it from the response stream: the hint downloads the file
  and the worker's own request then attaches to that response and fails with
  "WebAssembly compilation aborted". Measured in `startup.mjs`: two downloads
  on a fast connection, and on a throttled one the backend never starts at all

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
| `memory_limit_mb` | `512` | Soft limit for the Go heap in the worker. The garbage collector works harder near it. |
| `gc_ballast_mb` | `64` | Untouched allocation that keeps the GC's target up. The live heap is tiny between syncs, so without it every few MB of allocation triggered a full collection on the single thread. `0` disables it. |
| `log_level` | `debug` | Backend log level in the browser console (`trace`, `debug`, `info`, `warn`, `error`). `debug` logs every decrypted event; `info` keeps only the resource and timing lines below. |

The backend logs the effective values ("wasm configuration") and heap
statistics every 30 seconds in the browser console.

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
filter box: `/Memory stats|wasm compile|wasm configuration|Initial room list|Storage persistence/`.
Open DevTools before loading the page and look for:

| Line | Meaning |
|---|---|
| `wasm compile+instantiate: N ms` | Time V8 spent compiling the 30 MB module. Should be well under a second on a PC. |
| `wasm configuration` | Effective `config.json` values (connections, memory limit, timeline limit). |
| `Initial room list sent` | Time from Go start to the room list, number of rooms, and heap size right after the initial sync. |
| `Memory stats` | Go heap every 30 seconds (`heap_alloc_mb` is live data, `heap_sys_mb` is what wasm memory has grown to and never shrinks). |

For the browser's own view, use Chrome's Task Manager (Shift+Esc): the
renderer row for the tab includes V8's compiled code and the frontend. Note
it right after the initial sync and again after ten minutes of use: a number
that keeps climbing points to a leak, one that flattens is the platform cost.

## Troubleshooting

There used to be an `initial_timeline_limit` setting here. It is ignored now,
with a warning in the console if a config file still sets it: the filter it
fed is used for every sync rather than only the first, so a small window left
busy rooms with no message to sort or preview by, and any limited sync (which
happens whenever a suspended tab resumes) trimmed the stored timeline down to
it. The window is the same as the native build's.

**Picking up a new build.** A reload always fetches `index.html` fresh, so
Ctrl+R or restarting the app gets the newest build; unchanged files still
come from the browser cache, so it costs nothing when nothing changed.
Without a reload, a tab that has been in the background compares the served
`index.html` against the one it loaded with when it becomes visible again,
and reloads if they differ. If a deploy happens while the page is open and a
part of the app that loads on demand has gone missing, it reloads then too,
rather than failing with "error loading dynamically imported module".

Both checks work behind any static server, as long as `index.html` is served
without long-lived caching.

**An avatar stays as the generated letter, or an image stays broken, while
others load.** A failed download used to leave something permanent in the
media cache: first an error response, then, once the backoff was added, the
fallback letter avatar. Either way that one download could never succeed
again.

A third variant had the same look: a successful download never cleared the
error record, so the attempt count only grew, the backoff climbed towards its
week-long cap, and the fallback stayed cached long after the file had become
available. That is why one user could show as a letter in the timeline while
their real avatar loaded fine in the profile panel: those are two cache
entries, and only the small one had failed once.

All three are fixed, and the behaviour now matches the server build. A failure is
remembered by the backend with a backoff starting at a few seconds and
growing to a week, so a file the homeserver no longer has is not re-requested
on every render. While the backoff lasts, the letter avatar is served and
cached, but only until the next attempt is due, which is what the server
build achieves with a `Cache-Control` max-age. Anything left over from the
old behaviour is discarded, so this heals itself once the new build loads.

**A room sits in the wrong place in the room list on one device only.** The
room list is restored from an IndexedDB cache on every load, and the backend
then sends only rooms changed since the cached timestamp. An older build
could let a stale room snapshot into that cache, after which nothing refreshed
it. Reset the cache once in the browser console (F12), the room list is
rebuilt from the database:

```js
await client.store.deleteCache(); location.reload()
```

**A room shows no message preview in the room list, while the server build
shows one.** The same cache can store a room's preview event id without the
event it points to: the event is only written along if it happens to be
loaded in memory when the cache is flushed. Since a catchup sync only returns
rooms that changed, a quiet room stayed without a preview until someone said
something in it. The frontend now looks for those rooms once the first sync
is done and fetches the missing events from the backend, so the preview
appears on load. The `deleteCache` command above also clears it up.

## Media

Uploads take the same options as the server build, so the upload dialog's
controls do what they say: re-encoding to jpeg, png or gif, resizing by size
or percentage, the quality setting, sending as a plain file, and marking a
recording as a voice message. The image work is shared with the server build
(`gomuks.ReencodeImage`), since it is pure Go.

The waveform is computed the same way the server build's ffmpeg filter does,
deliberately including the part that looks wrong: each bucket is its peak
against full scale, and the whole thing is then scaled up so the loudest
bucket reaches the top. One loud transient, such as the click when a
recording stops, therefore sets the scale for everything else. Matching that
matters more than improving on it, so a voice message looks the same
whichever gomuks sent it.

The server build asks ffmpeg for the things it cannot read itself: how long an
audio or video file is, how big the picture is, a frame to use as a thumbnail,
and the waveform of a voice message. A browser already knows all of that, so
the wasm build reads it there instead (`web/src/api/wasm/probe.ts`): duration
and dimensions from a video element, a thumbnail by drawing a frame onto a
canvas, and the waveform from the decoded audio samples. The results travel
with the upload, and the backend attaches them exactly as the server build
does, including the blurhash on the thumbnail. If the browser cannot decode a
file, it is uploaded without the extra detail rather than failing.

Voice messages do come out as ogg/opus, the format the spec asks for, without
ffmpeg. Browsers record Opus in a WebM container, and the audio is already
what it needs to be, so `pkg/oggopus` moves the packets into an Ogg container
instead of converting them. No codec is involved and nothing is re-encoded.
It matters in practice: Element X will not play a WebM voice message.

The frontend asks for ogg/opus on every voice message exactly as it does for
the server build; only how the request is met differs. What still cannot work
is re-encoding video, or audio that was not recorded as Opus, since that
needs a codec: the upload dialog does not offer those targets in this build,
and anything that cannot be repackaged is sent as recorded. A WebAssembly build of ffmpeg would cover it, but it
is around 30 MB on its own, which is the same size as the whole client, so it
would only be worth loading at the moment someone asks for a conversion.
Uploads are also held in memory rather than streamed, so a very large file can
exhaust the worker.

When a link is pasted, the preview comes from the homeserver's `preview_url`
endpoint, the same as in the server build, and the image it points at has to
be re-uploaded because the media repository's copy is temporary. That upload
happened through `UploadMedia`, which starts by writing a temp file, so it
failed here with ENOSYS and the whole preview was dropped: the frontend logs
the error and sends the message without it, which made this look like a
missing feature rather than a broken one. The upload step is now replaceable
(`Gomuks.UploadMediaFunc`) and the wasm build does it in memory. Previews
without an image were never affected.

Media is served by a service worker (`web/public/wasmuks-media-sw.js`) out of
the Cache API, with the backend in the worker downloading on demand. The
server build serves the same URLs over HTTP, so the frontend does not know the
difference, with one thing that had to be reimplemented: byte ranges. The
server build gets them from Go's `http.ServeContent`; here the service worker
slices the cached body itself and answers 206. Without that, seeking in audio
and video does not work, and Safari refuses to play them at all.

## Building without Docker

The Docker build does all of this; doing it by hand needs one extra step the
server build doesn't, because the server generates the code block themes on
request and a static deployment has to ship them:

```sh
cd web
go run ../pkg/hicli/cmdspec/print src/api/types/stdcommands.json src/api/types/stdcommands.d.ts
mkdir -p public/_gomuks/codeblock && go run ../cmd/chromagen public/_gomuks/codeblock/
./build-wasm.sh
npm ci --include=dev && npm run build
```

Without that step code blocks in messages come out with no colours.

## Why startup costs what it does

Opening the page runs the whole backend from nothing: the SQLite module and
its storage layer load, the 30 MB Go program is fetched and compiled, schema
migrations run, and then the account and crypto store load before the first
sync. The file itself is cached by the browser, since its name contains a
build hash and it is served as immutable.

`pkg/sqlite-wasm-js/bench/startup.mjs` measures this in a cold browser and
prints the phases. At 10 Mbit with a 60 ms round trip, which is roughly a
phone on mobile data, a first load with gzip looks like this:

| | |
|---|---|
| frontend loaded, worker started, pickle key read | 1.5 s |
| Go module downloaded, compiled and instantiated | 6.9 s |
| database opened, migrations, crypto store | 7.5 s |

So the download is about 85% of it, and it is the bandwidth that decides,
not the order things happen in:

* **Compress it properly.** Brotli takes the download from 7.1 MB to 4.9 MB,
  and the same measurement from 7.5 s to 6.1 s. That saving repeats on every
  deployment, because the file name is content-hashed and every device fetches
  the whole thing again.
* **Starting it earlier doesn't help.** The download begins only after SQLite
  and the OPFS pool are set up, because the worker awaits those first, which
  looks like an obvious thing to fix. Running it alongside them instead
  measured 7.52 s against 7.66 s, inside the spread of the runs, so it was
  reverted rather than carried as a difference from upstream. A cold start at
  a given bandwidth is close to total bytes divided by bandwidth: fetching the
  module earlier only makes it share the connection with the rest of the
  frontend, and it finishes at the same time.
* **Don't try to start it from the page.** A `<link rel="preload">` looks like
  the obvious next step and breaks the load; see the note under recommended
  headers above, and the comment on `loadIndex` in `cmd/wasmukserve/main.go`.

Brotli costs nothing on a fast connection: unthrottled, the same measurement
is around 900 ms whether the file is served raw, gzipped or brotli'd, so the
decode doesn't show up. Note that browsers only offer `br` on a secure
origin, so a deployment reached over plain HTTP (other than localhost) will
be served the gzip sidecar instead.

Linking the wasm with `-s -w` is not worth it. It saves 0.6 MB uncompressed
but only 120 KB after brotli, and what it removes is the wasm name section,
which is what the browser labels wasm frames with: without it the devtools
profiler and any JavaScript-side stack trace show `wasm-function[1234]`
instead of a function name. Go's own panic tracebacks are unaffected either
way, since those come from the pclntab, which stays. Go's wasm output has no
DWARF to begin with, so `-w` does nothing at all.

What is left is the file itself. Making it smaller by dropping features is
the only remaining lever, and it is a poor one: the largest single thing in
there is chroma's syntax highlighting, and removing it saves 3.4 MB
uncompressed but only 0.5 MB after brotli, because it is mostly XML. The
same goes for the rest: 16.4 MiB of the binary is code, spread thin over
13000 functions, with the Go runtime the largest single entry at 811 KiB.

Whether the *compiled* code is reused is up to the engine, and the only way
to get it is the implicit cache. Chrome writes compiled WebAssembly to disk
and reuses it on later loads, but only for modules instantiated from a
stream, only above 128 KB, and keyed by the resource URL. The frontend takes
that path when the response says `application/wasm`, which `wasmukserve`
sends and `cmd/wasmukserve/main_test.go` checks, because serving it as a
generic binary would silently cost every visitor a full recompile. Every
deployment changes the URL, so the first load after one always recompiles.

Storing a compiled module in IndexedDB is not an option. It was an
experimental Firefox feature, removed in Firefox 63, never shipped in Chrome
or Safari, and the WebAssembly specification decided against it in favour of
the implicit caches above. Safari has no documented compiled-code cache, so
it most likely recompiles on every cold start; the only lever there is a
smaller module.

## Later

Not done, kept as options:

* **Push notifications** via a stateless Matrix push gateway in
  `wasmukserve`: the frontend would register a pusher with
  `format: event_id_only` and the Web Push subscription in the pusher data, so
  the server forwards "new message in room X" without ever holding keys or
  content.
* **Smaller binary.** The only lever on startup time in a browser without a
  compiled-code cache, which is to say Safari. Measured by building
  `cmd/wasmuks` with the syntax highlighter stubbed out, against the same
  commit unchanged:

  | | uncompressed | gzipped |
  |---|---|---|
  | as it ships | 30.4 MB | 7.0 MB |
  | without syntax highlighting | 25.6 MB | 6.0 MB |
  | saving | 4.8 MB, 16 % | 1.0 MB, 15 % |

  Most of that is data rather than code: chroma embeds 353 language
  definitions, about 2.5 MB of XML, and they all come along because the lexer
  is looked up by name at runtime. Highlighting runs when incoming HTML is
  sanitized, so dropping it means code blocks in messages you read lose their
  colours unless the frontend takes over.

  Markdown is not separable the same way. goldmark reaches the build through
  mautrix's `format` package, which hicli uses for rendering, parsing and
  HTML-to-markdown conversion, so removing it means replacing that package
  rather than stubbing a call. On its own it is 1.2 MB uncompressed, which is
  the ceiling on what that would save.

  `wasm-opt -Oz` from Binaryen is the other half, and it has now been
  measured. It rewrites the finished module rather than the Go code: dead
  code removed, instructions simplified, duplicates merged.

  | | uncompressed | gzipped |
  |---|---|---|
  | as it ships | 30.4 MB | 7.0 MB |
  | after `wasm-opt -Oz` | 27.3 MB | 6.8 MB |
  | saving | 3.1 MB, 10 % | 0.2 MB, 3 % |

  The optimised binary passes the startup smoke test, so it works. Compile
  time should fall roughly with the uncompressed size, which is the number
  that matters for a browser with no code cache; the download barely changes,
  because gzip had already found most of what wasm-opt removes.

  It is not enabled, for two reasons. It takes over six minutes on one core
  for a module this size, which belongs in CI rather than in a local build.
  And Go's wasm output is not a target Binaryen promises to support, so a
  passing smoke test is reassurance rather than proof. Both savings combined,
  dropping the highlighter and running the optimiser, would be about a quarter
  of the module.

  Reproduce with:

  ```sh
  npm install binaryen
  ./node_modules/.bin/wasm-opt -Oz --enable-bulk-memory --enable-sign-ext \
      --enable-nontrapping-float-to-int --enable-reference-types \
      web/src/api/wasm/_gomuks.wasm -o optimised.wasm
  ```
* **Native gomuks per user** as a compose file, for people who use the
  server machine itself as a client.
* **Upstream**: the wasm build stores a failed media download in the Cache API
  as a 500 and the service worker serves any cached response, so one bad fetch
  breaks that image permanently. The backend it wraps already handles this
  properly: `database.MediaError` keeps an attempt count and retries with
  exponential backoff, but the wasm media path never consulted it, so the
  Cache API layer made the failure permanent instead. Fixed here on both
  sides.
* **Upstream**: `pkg/sqlite-wasm-js/stmt.go` at the version this fork started
  from returns `LastInsertId` and `RowsAffected` the wrong way round for
  prepared statements, so an UPDATE reports the last inserted rowid as its
  affected row count. Fixed here already, worth reporting.
* **Upstream**: the websocket path has the same initial-sync/live-event
  ordering race that wasmuks now guards against (`sendInitialData` runs
  alongside the event writer); and a failed IndexedDB cache flush leaves
  `flushing` set, so no later flush runs until reload.

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
