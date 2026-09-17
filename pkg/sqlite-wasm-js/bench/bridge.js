// Port of web/src/api/wasm/sqlite_bridge.ts (the `meow` helpers the real driver
// needs) plus prototype helpers for a *batched* bridge, where a whole parameter
// set / result set crosses the Go<->JS boundary as one byte buffer.
import sqlite3InitModule from "./node_modules/@sqlite.org/sqlite-wasm/dist/index.mjs"

function safeFormatBigint(value) {
	if (typeof value === "bigint" && (value > Number.MAX_SAFE_INTEGER || value < Number.MIN_SAFE_INTEGER)) {
		return value.toString()
	}
	return Number(value)
}

class Writer {
	constructor() {
		this.buf = new Uint8Array(64 * 1024)
		this.dv = new DataView(this.buf.buffer)
		this.off = 0
	}
	ensure(n) {
		if (this.off + n <= this.buf.length) return
		let size = this.buf.length * 2
		while (size < this.off + n) size *= 2
		const nb = new Uint8Array(size)
		nb.set(this.buf.subarray(0, this.off))
		this.buf = nb
		this.dv = new DataView(nb.buffer)
	}
	u8(v) { this.ensure(1); this.buf[this.off++] = v }
	u32(v) { this.ensure(4); this.dv.setUint32(this.off, v, true); this.off += 4 }
	i64(v) { this.ensure(8); this.dv.setBigInt64(this.off, BigInt(v), true); this.off += 8 }
	f64(v) { this.ensure(8); this.dv.setFloat64(this.off, v, true); this.off += 8 }
	bytes(b) { this.ensure(b.length); this.buf.set(b, this.off); this.off += b.length }
	result() { return this.buf.subarray(0, this.off) }
}

export default async function init() {
	const sqlite3 = await sqlite3InitModule()
	const capi = sqlite3.capi
	const wasm = sqlite3.wasm
	const openDBs = new Map()

	function prepare(connPtr, sql) {
		const stack = wasm.pstack.pointer
		try {
			const ppStmt = wasm.pstack.allocPtr()
			const pzTail = wasm.pstack.allocPtr()
			const rc = capi.sqlite3_prepare_v2(connPtr, sql, -1, ppStmt, pzTail)
			if (rc !== capi.SQLITE_OK) {
				return { rc }
			}
			if (wasm.peekPtr(pzTail) !== 0) {
				throw new Error("sqlite3_prepare_v2 returned a non-zero tail pointer, which is unsupported")
			}
			return { ptr: wasm.peekPtr(ppStmt) }
		} finally {
			wasm.pstack.restore(stack)
		}
	}

	function exec(connPtr, sql) {
		const rc = capi.sqlite3_exec(connPtr, sql, 0, 0, 0)
		if (rc !== capi.SQLITE_OK) {
			throw new Error(`sqlite3_exec failed: rc=${rc} ${capi.sqlite3_errmsg(connPtr)} for ${sql.slice(0, 60)}`)
		}
	}

	sqlite3.meow = {
		// --- helpers used by the real driver (identical to sqlite_bridge.ts) ---
		prepare,
		last_insert_rowid: (connPtr) => safeFormatBigint(capi.sqlite3_last_insert_rowid(connPtr)),
		read_int64_column: (rowPtr, columnIndex) => safeFormatBigint(capi.sqlite3_column_int64(rowPtr, columnIndex)),

		// --- prototype batched bridge ---
		openDB(path, mode, extraPragmas) {
			let db
			if (mode === "memory") {
				db = new sqlite3.oo1.DB(":memory:", "c")
			} else {
				db = new sqlite3.PoolUtil.OpfsSAHPoolDb(path)
			}
			openDBs.set(db.pointer, { db, path, mode })
			exec(db.pointer, "PRAGMA foreign_keys = ON")
			if (mode !== "memory") {
				exec(db.pointer, "PRAGMA journal_mode = WAL")
				exec(db.pointer, "PRAGMA synchronous = NORMAL")
			}
			exec(db.pointer, "PRAGMA busy_timeout = 10000")
			for (const p of (extraPragmas ?? "").split(";").filter(Boolean)) {
				exec(db.pointer, p)
			}
			self.lastDBInfo = JSON.stringify({
				journal_mode: db.selectValue("PRAGMA journal_mode"),
				synchronous: db.selectValue("PRAGMA synchronous"),
				cache_size: db.selectValue("PRAGMA cache_size"),
				page_size: db.selectValue("PRAGMA page_size"),
				locking_mode: db.selectValue("PRAGMA locking_mode"),
			})
			return db.pointer
		},
		closeDB(ptr) {
			const entry = openDBs.get(ptr)
			entry?.db.close()
			openDBs.delete(ptr)
			if (entry && entry.mode !== "memory") {
				for (const f of sqlite3.PoolUtil.getFileNames()) {
					if (f.startsWith(entry.path)) sqlite3.PoolUtil.unlink(f)
				}
			}
		},
		exec,
		finalize(stmt) {
			capi.sqlite3_finalize(stmt)
		},
		// params: Uint8Array [u8 count][tagged values]; returns Uint8Array [u32 ncol][rows...]
		batchedQuery(stmt, params) {
			capi.sqlite3_reset(stmt)
			capi.sqlite3_clear_bindings(stmt)
			const dv = new DataView(params.buffer, params.byteOffset, params.byteLength)
			let off = 0
			const count = dv.getUint8(off++)
			for (let i = 1; i <= count; i++) {
				const tag = dv.getUint8(off++)
				let rc
				switch (tag) {
				case 0:
					rc = capi.sqlite3_bind_null(stmt, i)
					break
				case 1:
					rc = capi.sqlite3_bind_int64(stmt, i, dv.getBigInt64(off, true))
					off += 8
					break
				case 2:
					rc = capi.sqlite3_bind_double(stmt, i, dv.getFloat64(off, true))
					off += 8
					break
				case 3:
				case 4: {
					const len = dv.getUint32(off, true)
					off += 4
					const ptr = wasm.alloc(Math.max(len, 1))
					if (len > 0) {
						wasm.heap8u().set(params.subarray(off, off + len), ptr)
					}
					rc = (tag === 3 ? capi.sqlite3_bind_text : capi.sqlite3_bind_blob)(stmt, i, ptr, len, capi.SQLITE_WASM_DEALLOC)
					off += len
					break
				}
				default:
					throw new Error(`bad param tag ${tag}`)
				}
				// sqlite3_bind_null is declared void in sqlite-wasm's capi table and returns undefined
				if (rc !== undefined && rc !== capi.SQLITE_OK) {
					throw new Error(`bind ${i} failed rc=${rc}`)
				}
			}
			const w = new Writer()
			const ncol = capi.sqlite3_column_count(stmt)
			w.u32(ncol)
			for (;;) {
				const rc = capi.sqlite3_step(stmt)
				if (rc === capi.SQLITE_DONE) break
				if (rc !== capi.SQLITE_ROW) throw new Error(`step failed rc=${rc}`)
				for (let c = 0; c < ncol; c++) {
					const t = capi.sqlite3_column_type(stmt, c)
					switch (t) {
					case capi.SQLITE_NULL:
						w.u8(0)
						break
					case capi.SQLITE_INTEGER:
						w.u8(1)
						w.i64(capi.sqlite3_column_int64(stmt, c))
						break
					case capi.SQLITE_FLOAT:
						w.u8(2)
						w.f64(capi.sqlite3_column_double(stmt, c))
						break
					case capi.SQLITE_TEXT:
					case capi.SQLITE_BLOB: {
						// column_blob gives the raw UTF-8 bytes for TEXT too: no TextDecoder/TextEncoder round trip.
						const ptr = capi.sqlite3_column_blob(stmt, c)
						const len = capi.sqlite3_column_bytes(stmt, c)
						w.u8(t === capi.SQLITE_TEXT ? 3 : 4)
						w.u32(len)
						if (len > 0) w.bytes(wasm.heap8u().subarray(ptr, ptr + len))
						break
					}
					}
				}
				w.u32(0xFFFFFFFF)
			}
			capi.sqlite3_reset(stmt)
			return w.result()
		},
	}

	sqlite3.PoolUtil = await sqlite3.installOpfsSAHPoolVfs({ initialCapacity: 32 })
	self.sqlite3 = sqlite3
	return sqlite3
}
