Tulir's sqlite-wasm-js driver as it stood in gomuks v26.09, copied verbatim
from upstream commit 6a38b6a, with one change: it registers itself as
`sqlite-wasm-js-upstream` so the benchmark can open connections through both
drivers in the same page.

It exists so the "before" column of the benchmark is measured rather than
remembered. Do not fix anything here; it is a reference point.

It has one bug worth knowing about, found by the benchmark's update phase:
`stmt.go` fills in `LastInsertId` and `RowsAffected` the wrong way round for
statements that go through Prepare, so an UPDATE reports the last inserted
rowid as the number of rows it changed. The current driver gets this right.
Left as it is here on purpose.

The one thing it needs that the current bridge no longer provides,
`meow.read_int64_column`, is added back by worker.js before the benchmark
starts, so the production bridge stays as it is.
