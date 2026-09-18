import "./wasm_exec.js"
import initSqlite from "./sqlite_bridge.js"

;(async () => {
	const params = new URL(self.location.href).searchParams
	self.benchN = parseInt(params.get("n") ?? "2000")
	self.benchVariants = params.get("v") ?? ""
	await initSqlite()
	// The vendored upstream driver (see upstream/) reads 64-bit columns through
	// a bridge helper the current bridge no longer needs. Add it back here so
	// the production bridge stays as it is and both drivers can run in the
	// same page.
	const { capi } = self.sqlite3
	self.sqlite3.meow.read_int64_column = (rowPtr, columnIndex) => {
		const value = capi.sqlite3_column_int64(rowPtr, columnIndex)
		if (typeof value === "bigint" && (value > Number.MAX_SAFE_INTEGER || value < Number.MIN_SAFE_INTEGER)) {
			return value.toString()
		}
		return Number(value)
	}
	const go = new Go()
	const t0 = performance.now()
	const { instance } = await WebAssembly.instantiateStreaming(fetch("./bench.wasm"), go.importObject)
	self.postMessage({ type: "log", msg: `wasm instantiate: ${(performance.now() - t0).toFixed(0)} ms` })
	go.run(instance).catch(err => self.postMessage({ type: "error", error: String(err) }))
	const poll = setInterval(() => {
		if (self.benchResult) {
			clearInterval(poll)
			self.postMessage({ type: "result", result: JSON.parse(self.benchResult) })
		}
	}, 50)
})().catch(err => self.postMessage({ type: "error", error: String(err && err.stack || err) }))
