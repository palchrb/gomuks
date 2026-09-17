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

Chromium comes from Playwright (`npx playwright install chromium`); set
`CHROMIUM_PATH` to use another Chromium binary instead.

## Reference numbers

Headless Chromium 130-era build, OPFS SAHPool, n = 2000 rows, 500 point
lookups, minimum of 3 repetitions, milliseconds. "Before" is the driver as of
gomuks v26.09 (per-value bridge calls, WAL silently falling back to a rollback
journal with normal locking); "after" is the batched bridge with statement
cache and EXCLUSIVE locking + PERSIST journal.

| Operation | before | after |
|---|---|---|
| insert 2000 rows in one transaction | 600 | 293 |
| timeline select, 2000 rows | 155 | 45 |
| 500 point lookups by unique key | 627 | 20 |

In `memory` mode (no I/O, pure bridge cost) the same operations went from
600 / 205 / 245 ms to 93 / 60 / 31 ms. The remaining OPFS insert cost is the
actual file writes (~60 MB/s).

## Startup smoke test

`smoke.mjs` serves a built `web/dist` in headless Chromium and checks that the
wasm backend initializes, the login screen renders, `config.json` defaults are
applied, and a second tab is refused by the Web Lock:

```sh
cd web && ./build-wasm.sh && npm run build && cd ../pkg/sqlite-wasm-js/bench
CHROMIUM_PATH=/path/to/chrome node smoke.mjs
```
