# sqlite-wasm-js benchmark

Microbenchmark for the `sqlite-wasm-js` driver, run in headless Chromium against
the real `@sqlite.org/sqlite-wasm` build with the OPFS SAHPool VFS. It uses a
table shaped like gomuks' `event` table (22 columns, ~1 KB rows) and measures
three operations: bulk insert inside one transaction, a timeline-style
`SELECT ... LIMIT n`, and 500 point lookups by unique key.

Variants:

* `current` – the driver through `database/sql`, one prepare per call (what hicli does).
* `reuse` – same, with a prepared statement reused for all rows.
* `current+exclusive+persist` – the driver with `PRAGMA locking_mode=EXCLUSIVE; journal_mode=PERSIST`.
* `batched*` – a JS-side prototype where the whole parameter set / result set
  crosses the Go↔JS boundary as one byte buffer (reference ceiling for the
  driver's batched mode; see `bridge.js`).

Each variant runs in `memory` (pure bridge cost) and `opfs-sahpool`. Results
are the minimum of 3 repetitions, in milliseconds.

```sh
cd pkg/sqlite-wasm-js/bench
npm install
npm run build
node run.js 2000                                  # all variants, n=2000
node run.js 10000 "current,current+exclusive+persist"   # subset
```

`run.js` looks for Playwright's Chromium via `PLAYWRIGHT_BROWSERS_PATH` or the
`CHROMIUM_PATH` environment variable.
