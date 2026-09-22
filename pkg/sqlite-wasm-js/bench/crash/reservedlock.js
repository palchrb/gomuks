// Shared with web/src/api/wasm/sqlite_bridge.ts; see the comment there.
export function patchReservedLock(sqlite3, pool) {
	const { capi, wasm } = sqlite3
	const probe = new pool.OpfsSAHPoolDb("/.reserved-lock-probe")
	let pMethods
	const stack = wasm.pstack.pointer
	try {
		const pOut = wasm.pstack.allocPtr()
		const rc = capi.sqlite3_file_control(probe.pointer, "main", capi.SQLITE_FCNTL_FILE_POINTER, pOut)
		if (rc !== 0) {
			throw new Error(`SQLITE_FCNTL_FILE_POINTER failed: ${rc}`)
		}
		pMethods = wasm.peekPtr(wasm.peekPtr(pOut))
	} finally {
		wasm.pstack.restore(stack)
		probe.close()
		pool.unlink("/.reserved-lock-probe")
	}
	const io = new capi.sqlite3_io_methods(pMethods)
	const origLock = wasm.functionEntry(io.$xLock)
	const origUnlock = wasm.functionEntry(io.$xUnlock)
	const origClose = wasm.functionEntry(io.$xClose)
	const locks = new Map()
	io.installMethods({
		xLock(pFile, lockType) {
			locks.set(pFile, lockType)
			return origLock(pFile, lockType)
		},
		xUnlock(pFile, lockType) {
			locks.set(pFile, lockType)
			return origUnlock(pFile, lockType)
		},
		xClose(pFile) {
			locks.delete(pFile)
			return origClose(pFile)
		},
		xCheckReservedLock(pFile, pOut) {
			let held = 0
			for (const [other, lockType] of locks) {
				if (other !== pFile && lockType >= capi.SQLITE_LOCK_RESERVED) {
					held = 1
					break
				}
			}
			wasm.poke32(pOut, held)
			return 0
		},
	}, false)
}
