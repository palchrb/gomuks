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

## Serving

Serve `web/dist/` as static files from any web server. The wasm mode is enabled
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

## Limitations compared to the server build

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
