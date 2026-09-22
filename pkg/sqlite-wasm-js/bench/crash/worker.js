import sqlite3InitModule from "/sqlite-wasm/index.mjs"
import { patchReservedLock } from "./reservedlock.js"

self.addEventListener("error", e => self.postMessage({ integrity: `worker error: ${e.message}` }))
self.addEventListener("unhandledrejection", e => self.postMessage({ integrity: `worker error: ${e.reason?.stack ?? e.reason}` }))

const params = new URL(self.location.href).searchParams
const sqlite3 = await sqlite3InitModule()
// A terminated worker's access handles are released asynchronously, and a
// failed install keeps the handles it did get, so the harness retries with a
// fresh worker rather than this one retrying.
let pool
try {
	pool = await sqlite3.installOpfsSAHPoolVfs({ initialCapacity: 12 })
} catch (err) {
	self.postMessage({ busy: `${err}` })
	await new Promise(() => {})
}
if (params.get("patch") === "1") {
	patchReservedLock(sqlite3, pool)
}
const open = () => {
	const db = new pool.OpfsSAHPoolDb("/crash.db")
	// "upstream" is what upstream gomuks sets: WAL, which this VFS can't do
	// without EXCLUSIVE locking, so SQLite silently stays in DELETE mode.
	db.exec(params.get("pragmas") === "upstream"
		? "PRAGMA journal_mode = WAL; PRAGMA synchronous = NORMAL"
		: "PRAGMA locking_mode = EXCLUSIVE; PRAGMA journal_mode = PERSIST; PRAGMA synchronous = NORMAL")
	return db
}
if (params.get("mode") === "check") {
	let result
	try {
		const db = open()
		result = {
			integrity: db.selectValue("PRAGMA integrity_check"),
			rows: db.selectValue("SELECT count(*) FROM t"),
			sum: db.selectValue("SELECT sum(n) FROM t"),
		}
		db.close()
	} catch (err) {
		result = { integrity: `open failed: ${err}` }
	}
	self.postMessage(result)
} else {
	const db = open()
	db.exec("CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY, v BLOB, n INTEGER NOT NULL)")
	self.postMessage({ started: true })
	// Every transaction keeps sum(n) = 0 mod 1000 and rewrites many pages,
	// so a half-applied one shows up either in integrity_check or the sum.
	try {
		for (let i = 0; ; i++) {
			db.exec("BEGIN")
			db.exec("WITH RECURSIVE s(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM s WHERE x < 200) INSERT INTO t (v, n) SELECT randomblob(3000), 0 FROM s")
			db.exec("UPDATE t SET n = n + 1, v = randomblob(3000) WHERE id % 7 = " + (i % 7))
			db.exec("UPDATE t SET n = n - 1 WHERE id % 7 = " + (i % 7))
			db.exec("DELETE FROM t WHERE id IN (SELECT id FROM t ORDER BY random() LIMIT 150)")
			db.exec("COMMIT")
			self.postMessage({ commits: i + 1 })
		}
	} catch (err) {
		self.postMessage({ writeError: `${err}` })
	}
}
