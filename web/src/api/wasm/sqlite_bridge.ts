// Copyright (c) 2025 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

import type { Database, SAHPoolUtil, Sqlite3Static, WasmPointer } from "@sqlite.org/sqlite-wasm"
import sqlite3InitModule from "@sqlite.org/sqlite-wasm"

interface Meowlite extends Sqlite3Static {
	PoolUtil?: SAHPoolUtil
	meow?: {
		prepare: (connPtr: Database | WasmPointer, sql: string | WasmPointer) => {
			rc?: number,
			ptr?: WasmPointer,
		}
		last_insert_rowid: (connPtr: Database | WasmPointer) => number | string
		batchedQuery: (
			stmt: WasmPointer, params: Uint8Array, paramsLen: number,
			wantColumns: boolean, keepRows: boolean, limitBytes: number,
		) => Uint8Array
		batchedStep: (stmt: WasmPointer, keepRows: boolean, limitBytes: number) => Uint8Array
	}
}

// Wire format for batchedQuery/batchedStep. The authoritative description
// (and the Go decoder) is in pkg/sqlite-wasm-js/codec.go; keep the two in sync.
const enum Tag {
	Null = 0,
	Int = 1,
	Float = 2,
	Text = 3,
	Blob = 4,
}

const enum Phase {
	Bind = 1,
	Step = 2,
}

const textEncoder = new TextEncoder()

class ResultWriter {
	buf = new Uint8Array(64 * 1024)
	dv = new DataView(this.buf.buffer)
	off = 0

	reset() {
		this.off = 0
	}

	ensure(n: number) {
		if (this.off + n <= this.buf.length) {
			return
		}
		let size = this.buf.length * 2
		while (size < this.off + n) {
			size *= 2
		}
		const next = new Uint8Array(size)
		next.set(this.buf.subarray(0, this.off))
		this.buf = next
		this.dv = new DataView(next.buffer)
	}

	u8(v: number) {
		this.ensure(1)
		this.buf[this.off++] = v
	}

	u32(v: number) {
		this.ensure(4)
		this.dv.setUint32(this.off, v, true)
		this.off += 4
	}

	u32At(off: number, v: number) {
		this.dv.setUint32(off, v, true)
	}

	i64(v: bigint | number) {
		this.ensure(8)
		this.dv.setBigInt64(this.off, BigInt(v), true)
		this.off += 8
	}

	f64(v: number) {
		this.ensure(8)
		this.dv.setFloat64(this.off, v, true)
		this.off += 8
	}

	bytes(b: Uint8Array) {
		this.ensure(b.length)
		this.buf.set(b, this.off)
		this.off += b.length
	}

	str(s: string) {
		const b = textEncoder.encode(s)
		this.u32(b.length)
		this.bytes(b)
	}

	result(): Uint8Array {
		return this.buf.subarray(0, this.off)
	}
}

// One writer for the whole worker: the Go side copies the returned view
// synchronously before it can make another call.
const writer = new ResultWriter()

const HEADER_RC_OFFSET = 0
const HEADER_PHASE_OFFSET = 4
const HEADER_DONE_OFFSET = 5

declare global {
	interface Window {
		sqlite3: Meowlite
	}
}

function safeFormatBigint(value: bigint): string | number {
	if (value > Number.MAX_SAFE_INTEGER || value < Number.MIN_SAFE_INTEGER) {
		return value.toString()
	}
	return Number(value)
}

async function init() {
	const sqlite3: Meowlite = await sqlite3InitModule()

	const capi = sqlite3.capi
	const wasm = sqlite3.wasm

	function bindParams(stmt: WasmPointer, params: Uint8Array): number {
		const dv = new DataView(params.buffer, params.byteOffset, params.byteLength)
		let off = 0
		const count = dv.getUint32(off, true)
		off += 4
		for (let i = 0; i < count; i++) {
			const index = dv.getUint16(off, true)
			off += 2
			const tag = params[off++]
			let rc: number | undefined
			switch (tag) {
			case Tag.Null:
				rc = capi.sqlite3_bind_null(stmt, index)
				break
			case Tag.Int:
				rc = capi.sqlite3_bind_int64(stmt, index, dv.getBigInt64(off, true))
				off += 8
				break
			case Tag.Float:
				rc = capi.sqlite3_bind_double(stmt, index, dv.getFloat64(off, true))
				off += 8
				break
			case Tag.Text:
			case Tag.Blob: {
				const len = dv.getUint32(off, true)
				off += 4
				const ptr = wasm.alloc(Math.max(len, 1))
				if (len > 0) {
					// heap8u() must be re-fetched after every alloc, growth invalidates old views
					wasm.heap8u().set(params.subarray(off, off + len), ptr)
				}
				rc = tag === Tag.Text
					? capi.sqlite3_bind_text(stmt, index, ptr, len, capi.SQLITE_WASM_DEALLOC)
					: capi.sqlite3_bind_blob(stmt, index, ptr, len, capi.SQLITE_WASM_DEALLOC)
				off += len
				break
			}
			default:
				throw new Error(`unknown parameter tag ${tag}`)
			}
			// sqlite3_bind_null is declared without a return value in sqlite-wasm's capi table
			if (rc !== undefined && rc !== capi.SQLITE_OK) {
				return rc
			}
		}
		return capi.SQLITE_OK
	}

	function writeRow(stmt: WasmPointer, ncol: number) {
		for (let c = 0; c < ncol; c++) {
			const type = capi.sqlite3_column_type(stmt, c)
			switch (type) {
			case capi.SQLITE_NULL:
				writer.u8(Tag.Null)
				break
			case capi.SQLITE_INTEGER:
				writer.u8(Tag.Int)
				writer.i64(capi.sqlite3_column_int64(stmt, c))
				break
			case capi.SQLITE_FLOAT:
				writer.u8(Tag.Float)
				writer.f64(capi.sqlite3_column_double(stmt, c))
				break
			case capi.SQLITE_TEXT:
			case capi.SQLITE_BLOB: {
				// column_blob gives the raw UTF-8 bytes for TEXT too, which avoids a
				// TextDecoder/TextEncoder round trip. column_bytes must be called after it.
				const ptr = capi.sqlite3_column_blob(stmt, c)
				const len = capi.sqlite3_column_bytes(stmt, c)
				writer.u8(type === capi.SQLITE_TEXT ? Tag.Text : Tag.Blob)
				writer.u32(len)
				if (len > 0) {
					writer.bytes(wasm.heap8u().subarray(ptr, ptr + len))
				}
				break
			}
			default:
				throw new Error(`unknown column type ${type}`)
			}
		}
	}

	// Steps the statement until it's done or the chunk limit is reached, and
	// finishes the result buffer. The header (rc, phase, done, ncol, columns,
	// nrows placeholder) must already be written.
	function stepInto(stmt: WasmPointer, ncol: number, nrowsOff: number, keepRows: boolean, limitBytes: number) {
		let nrows = 0
		let done = false
		for (;;) {
			const rc = capi.sqlite3_step(stmt)
			if (rc === capi.SQLITE_DONE) {
				done = true
				break
			} else if (rc !== capi.SQLITE_ROW) {
				writer.u32At(HEADER_RC_OFFSET, rc)
				writer.buf[HEADER_PHASE_OFFSET] = Phase.Step
				return writer.result()
			}
			if (keepRows) {
				writeRow(stmt, ncol)
				nrows++
				if (writer.off >= limitBytes) {
					break
				}
			}
		}
		writer.u32At(nrowsOff, nrows)
		if (done) {
			writer.buf[HEADER_DONE_OFFSET] = 1
			const db = capi.sqlite3_db_handle(stmt)
			writer.i64(capi.sqlite3_changes64(db))
			writer.i64(capi.sqlite3_last_insert_rowid(db))
		}
		return writer.result()
	}

	function writeHeader(stmt: WasmPointer, wantColumns: boolean): { ncol: number, nrowsOff: number } {
		writer.reset()
		writer.u32(0) // rc
		writer.u8(0) // phase
		writer.u8(0) // done
		const ncol = capi.sqlite3_column_count(stmt)
		writer.u32(ncol)
		if (wantColumns) {
			writer.u8(1)
			for (let c = 0; c < ncol; c++) {
				writer.str(capi.sqlite3_column_name(stmt, c))
				writer.str(capi.sqlite3_column_decltype(stmt, c) ?? "")
			}
		} else {
			writer.u8(0)
		}
		const nrowsOff = writer.off
		writer.u32(0)
		return { ncol, nrowsOff }
	}

	sqlite3.meow = {
		batchedQuery: (stmt, params, paramsLen, wantColumns, keepRows, limitBytes) => {
			const { ncol, nrowsOff } = writeHeader(stmt, wantColumns)
			capi.sqlite3_reset(stmt)
			capi.sqlite3_clear_bindings(stmt)
			const rc = bindParams(stmt, params.subarray(0, paramsLen))
			if (rc !== capi.SQLITE_OK) {
				writer.u32At(HEADER_RC_OFFSET, rc)
				writer.buf[HEADER_PHASE_OFFSET] = Phase.Bind
				return writer.result()
			}
			return stepInto(stmt, ncol, nrowsOff, keepRows, limitBytes)
		},
		batchedStep: (stmt, keepRows, limitBytes) => {
			const { ncol, nrowsOff } = writeHeader(stmt, false)
			return stepInto(stmt, ncol, nrowsOff, keepRows, limitBytes)
		},
		prepare: (connPtr, sql) => {
			const stack = sqlite3.wasm.pstack.pointer
			try {
				const ppStmt = sqlite3.wasm.pstack.allocPtr()
				const pzTail = sqlite3.wasm.pstack.allocPtr()
				const rc = sqlite3.capi.sqlite3_prepare_v2(connPtr, sql, -1, ppStmt, pzTail)
				if (rc !== sqlite3.capi.SQLITE_OK) {
					return { rc }
				}
				if (sqlite3.wasm.peekPtr(pzTail) !== 0) {
					throw new Error("sqlite3_prepare_v2 returned a non-zero tail pointer, which is unsupported")
				}
				return { ptr: sqlite3.wasm.peekPtr(ppStmt) }
			} finally {
				sqlite3.wasm.pstack.restore(stack)
			}
		},
		last_insert_rowid: (connPtr) => {
			return safeFormatBigint(sqlite3.capi.sqlite3_last_insert_rowid(connPtr))
		},
	}

	// Each open file takes one sync access handle from the pool. With
	// journal_mode=PERSIST the journal file stays around permanently, so the
	// database needs two, and the default capacity of 6 leaves little slack.
	sqlite3.PoolUtil = await sqlite3.installOpfsSAHPoolVfs({ initialCapacity: 12 })

	self.sqlite3 = sqlite3
}

export default init
