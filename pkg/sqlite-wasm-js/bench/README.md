# sqlite-wasm-js benchmark

Microbenchmark for the `sqlite-wasm-js` driver, run in headless Chromium against
the real `@sqlite.org/sqlite-wasm` build with the OPFS SAHPool VFS. It uses a
table shaped like gomuks' `event` table (22 columns, ~1 KB rows) and measures
four operations: bulk insert inside one transaction, a timeline-style
`SELECT ... LIMIT n`, 500 point lookups by unique key, and 500 single-row
updates each in its own transaction, which is what marking things read and
bumping rooms looks like.

Variants, in the order they build on each other:

* `upstream` – the driver as it stood in gomuks v26.09, copied verbatim into
  `upstream/`: one crossing into JavaScript per value, no statement cache,
  normal locking and a rollback journal.
* `nosync`, `journalmem` – the shipping configuration without the flush at the
  end of each write, and with the rollback journal held in memory. Both trade
  durability for speed and exist to show where the time in a small write goes,
  not as recommendations.
* `page4k` … `page64k` – the shipping configuration with a different SQLite
  page size. The sqlite-wasm build already defaults to 8 KiB rather than
  SQLite's usual 4 KiB, and the `page4k` variant shows why. Not part of the
  default set; ask for them by name.
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

Measured on a Raspberry Pi, median of ten runs, 2000 rows, milliseconds:

| | insert 2000 rows | read 2000 rows | 500 single-row lookups |
|---|---|---|---|
| upstream wasmuks | 1496 | 802 | 1234 |
| this build | 545 | 89 | 70 |
| floor: no storage, database in memory | 298 | 88 | 69 |
| improvement vs upstream wasmuks | 2.7x | 9.0x | 17.6x |

Reads and lookups have reached the floor, so nothing is left to win there.
Only inserts still pay for the disk, and that difference is the file writes.
A faster machine shows the same shape with smaller absolute numbers.

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

### Page size

Larger pages cut the insert time substantially, roughly a sixth at 16 KiB and
a third at 32 KiB, with reads unchanged. The worry was that a rollback journal
copies whole pages whatever the size of the change, so gomuks' constant small
updates would pay for the larger page. They don't: the update phase measures
the same at 8, 16 and 32 KiB. So the case for a larger default is open, though
it only applies to databases created afterwards unless something runs VACUUM.

### The expensive thing is small writes

A single-row update in its own transaction costs a few milliseconds, against
well under a tenth of that for a point lookup. Five hundred of them outweigh
inserting two thousand rows and reading them back, several times over.

The `nosync` and `journalmem` variants say where that time goes. Skipping the
flush at the end of each write saves little; holding the rollback journal in
memory instead of a file cuts the same work by about six times. So it is the
journal file, not the flush.

That is not a licence to move the journal into memory. It is the file that
lets SQLite undo a half-finished write, and a browser tab can be killed at any
moment. The safe version of the same saving is to write less often: the
benchmark does the same five hundred updates a second time inside one
transaction, and that is about seven times faster with no change in
durability. If small writes ever show up in a profile, that is the direction.

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

At the end it prints a three-line summary: the upstream driver, this build,
and the same code with storage taken out, which is the floor. Everything
above that is the detail behind those three lines.

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

## Cold-start timing

`startup.mjs` serves a built `web/dist` and times a cold load: a fresh
browser each run, so neither the HTTP cache nor Chromium's compiled-code
cache carries anything over. It prints the phases on the way to a started
backend and counts how many times the wasm binary is fetched.

```sh
cd web && npm run build && cd dist \
	&& find . -type f \( -name '*.wasm' -o -name '*.js' -o -name '*.css' -o -name '*.html' \) -exec gzip -9 -k {} +
cd ../../pkg/sqlite-wasm-js/bench
node startup.mjs 3                    # as fast as the local disk
THROTTLE_MBPS=10 node startup.mjs 3   # roughly a phone on mobile data
```

Throttle it. Unthrottled, the 30 MB download is nearly free and the
measurement says nothing about the load a phone sees; at 10 Mbit the download
is around 85% of the time to a started backend. Compress the dist first if
the download is part of what you're measuring, because the server here serves
the sidecars when they exist, like `wasmukserve` does.

The useful thing this measures is bytes. At a given bandwidth the cold start
is close to total bytes divided by bandwidth, so compressing better moves it
and reordering the startup does not. `--hint preload` and `--hint prefetch`
add a `<link>` for the wasm binary to `index.html`, which looks like it
should help and instead fetches the file twice, or, throttled, leaves the
worker's streaming compile attached to an aborted response so the backend
never starts at all.

## Crash test

`crash/run.mjs` kills a worker in the middle of SQLite write transactions on
the OPFS SAH pool, over and over, and runs `integrity_check` after each kill.
That is what a page reload, or iOS killing a backgrounded app, does to the
wasm build.

```
node crash/run.mjs 100            # the VFS as shipped: corrupt within a few kills
node crash/run.mjs 100 --patch    # with the reserved-lock fix: survives
node crash/run.mjs 100 --upstream-pragmas   # upstream gomuks' settings
```

The cause is in the VFS: `xCheckReservedLock` always reports a write lock,
so SQLite never rolls back a hot journal. The fix is in
`web/src/api/wasm/sqlite_bridge.ts` (`patchReservedLock`), with a copy in
`crash/reservedlock.js` for the test.
