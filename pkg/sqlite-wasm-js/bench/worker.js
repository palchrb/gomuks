import "./wasm_exec.js"
import initSqlite from "./sqlite_bridge.js"

;(async () => {
	const params = new URL(self.location.href).searchParams
	self.benchN = parseInt(params.get("n") ?? "2000")
	self.benchVariants = params.get("v") ?? ""
	await initSqlite()
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
