# sqlite-wasm-js benchmark

Microbenchmark for the `sqlite-wasm-js` driver, run in headless Chromium against
the real `@sqlite.org/sqlite-wasm` build with the OPFS SAHPool VFS. It uses a
table shaped like gomuks' `event` table (22 columns, ~1 KB rows) and measures
three operations: bulk insert inside one transaction, a timeline-style
`SELECT ... LIMIT n`, and 500 point lookups by unique key.

Variants, in the order they build on each other:

* `upstream` – the driver as it stood in gomuks v26.09, copied verbatim into
  `upstream/`: one crossing into JavaScript per value, no statement cache,
  normal locking and a rollback journal.
* `upstream+reuse` – the same driver with one prepared statement reused for
  every row. It has no statement cache, so this is the closest thing to giving
  it one, and it separates the cache's share of the improvement from the
  bridge's.
* `upstream+exclusive+persist` – the same driver with `PRAGMA
  locking_mode=EXCLUSIVE` and `journal_mode=PERSIST`.
* `current` – this driver (batched bridge, statement cache) with the old
  pragmas pinned, so the bridge is the only difference from `upstream`.
* `reuse` – same, with one prepared statement reused for every row.
* `current+exclusive+persist` – this driver with its defaults. What gomuks
  ships.

Two comparisons fall out of that. `upstream` against `current` isolates the
bridge; either driver against its own `+exclusive+persist` isolates the
locking mode. Every variant also runs against an in-memory database, which is
the floor once storage is out of the picture.

```sh
cd pkg/sqlite-wasm-js/bench
npm install
npm run build
node run.js 2000                                  # one run, all variants
node run.js 10000 "upstream,current"              # a subset
```

Chromium comes from Playwright (`npx playwright install chromium`); set
`CHROMIUM_PATH` to use another Chromium binary instead.

## What the numbers say

Run it yourself rather than trusting a table: the absolute numbers depend
entirely on the machine, and on a Raspberry Pi a single crossing into
JavaScript costs three to four times what it does on a laptop. The shape
holds across the machines it has been run on:

* The **batched bridge** is what makes bulk work faster. Inserting and reading
  many rows both improve substantially, because the old driver paid a crossing
  per value and a row here has 22 columns.
* **EXCLUSIVE locking** is what makes many small reads fast, by a large factor.
  Under normal locking every read transaction takes and releases a file lock
  and cannot trust its cached pages between statements, and on the SAH pool
  each of those is a synchronous file operation. Holding the lock for the
  session brings point lookups down to in-memory speed.
* The **statement cache** is worth checking on the machine you care about.
  `reuse` against `current` shows what the caller would gain by holding
  prepared statements itself once the driver already caches them, which is
  nothing; `upstream+reuse` against `upstream` shows what the cache is worth
  to a driver that has none.

The remaining insert cost on storage is the actual file writes, around
60 MB/s in this environment.

## Repeating it

One run on a machine that is doing other things is not worth much, so
`runs.js` repeats the whole benchmark in a fresh browser process and prints
the spread. Everything is cold in each run: new browser, new database files.

```sh
node runs.js 10                                       # 10 runs, 2000 rows, all variants
node runs.js 20 2000 "upstream,current+exclusive+persist"   # a subset
```

Each run warms up every variant on a small table first and throws those
numbers away. Without that the variant that happens to run first carries the
warm-up cost in every repetition, so taking the minimum does not remove it,
and it looked like a real difference of about a third on the read.

It prints min, median, mean, max and max/min per variant and operation, and
writes every raw result to `results-<timestamp>.json`. Read the spread before
believing a difference: anything inside it is noise. Ten runs is usually
enough to separate the effects that matter from the ones that don't.

Chromium comes from Playwright by default; set `CHROMIUM_PATH` to use another
binary. If Playwright has no build for the architecture, which is the case on
a Raspberry Pi, the system browser (`/usr/bin/chromium` and friends) is used
automatically. Building `bench.wasm` needs the same Go version as the project.

## Measuring the device you actually use

The benchmark measures the browser doing the database work, so the machine
that counts is the one running the browser, not the one serving the files. In
a wasmuks deployment the server only hands out static files; the database work
happens on the laptop or phone. To measure one of those, build here once and
serve the directory to the network:

```sh
BENCH_HOST=0.0.0.0 node serve.js
```

Then open `http://<that machine>:29399/?n=2000` on the device. The page runs
the same benchmark and prints the same JSON.

## Startup smoke test

`smoke.mjs` serves a built `web/dist` in headless Chromium and checks that the
wasm backend initializes, the login screen renders, `config.json` defaults are
applied, and a second tab is refused by the Web Lock:

```sh
cd web && ./build-wasm.sh && npm run build && cd ../pkg/sqlite-wasm-js/bench
CHROMIUM_PATH=/path/to/chrome node smoke.mjs
```
