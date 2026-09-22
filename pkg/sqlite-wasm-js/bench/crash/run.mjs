// Kills a worker in the middle of SQLite write transactions on the OPFS SAH
// pool, over and over, and checks the database after each kill. A page reload
// or iOS killing a backgrounded app does the same to the wasm build.
// Usage: node run.mjs [iterations] [--patch] [--upstream-pragmas]
import http from "node:http"
import fs from "node:fs"
import path from "node:path"
import { launchChromium } from "../browser.js"

const here = path.dirname(new URL(import.meta.url).pathname)
const sqliteDist = path.resolve(here, "../../../../web/node_modules/@sqlite.org/sqlite-wasm/dist")
const iterations = Number(process.argv.find(a => /^\d+$/.test(a)) ?? 30)
const patch = process.argv.includes("--patch")
const upstreamPragmas = process.argv.includes("--upstream-pragmas")
const types = { ".js": "text/javascript", ".mjs": "text/javascript", ".wasm": "application/wasm", ".html": "text/html" }
const server = http.createServer((req, res) => {
	const url = new URL(req.url, "http://x")
	const file = url.pathname.startsWith("/sqlite-wasm/")
		? path.join(sqliteDist, url.pathname.slice("/sqlite-wasm/".length))
		: path.join(here, url.pathname === "/" ? "index.html" : url.pathname)
	if (url.pathname === "/") {
		res.writeHead(200, { "Content-Type": "text/html" })
		res.end("<!doctype html><title>crash</title>")
		return
	}
	if (!fs.existsSync(file)) {
		res.writeHead(404)
		res.end()
		return
	}
	res.writeHead(200, { "Content-Type": types[path.extname(file)] ?? "application/octet-stream" })
	fs.createReadStream(file).pipe(res)
})
await new Promise(resolve => server.listen(0, "127.0.0.1", resolve))
const browser = await launchChromium()
const page = await (await browser.newContext()).newPage()
if (process.env.VERBOSE) {
	page.on("console", m => console.log(m.text()))
	page.on("requestfailed", r => console.log("failed", r.url()))
	page.on("response", r => r.status() >= 400 && console.log(r.status(), r.url()))
	page.on("worker", w => w.on("console", m => console.log("[worker]", m.text())))
}
await page.goto(`http://127.0.0.1:${server.address().port}/`)
const results = await page.evaluate(async ({ iterations, patch, upstreamPragmas }) => {
	const startOnce = mode => {
		const w = new Worker(`worker.js?mode=${mode}&patch=${patch ? 1 : 0}&pragmas=${upstreamPragmas ? "upstream" : "ours"}`, { type: "module" })
		const first = new Promise(resolve => {
			w.onerror = e => resolve({ integrity: `worker error: ${e.message}` })
			w.onmessage = e => resolve(e.data)
		})
		return { w, first }
	}
	const start = async mode => {
		for (let attempt = 0; ; attempt++) {
			const started = startOnce(mode)
			const first = await started.first
			if (!first.busy || attempt >= 100) {
				return { w: started.w, first }
			}
			started.w.terminate()
			await new Promise(resolve => setTimeout(resolve, 100))
		}
	}
	const out = []
	for (let i = 0; i < iterations; i++) {
		const writer = await start("write")
		const hello = writer.first
		if (!hello.started) {
			writer.w.terminate()
			out.push({ ...hello, phase: "write" })
			break
		}
		let commits = 0
		let writeError
		writer.w.onmessage = e => {
			commits = e.data.commits ?? commits
			writeError ??= e.data.writeError
		}
		await new Promise(resolve => setTimeout(resolve, 100 + Math.random() * 900))
		writer.w.terminate()
		if (writeError) {
			out.push({ integrity: writeError, phase: "write" })
			break
		}
		await new Promise(resolve => setTimeout(resolve, 50))
		const checker = await start("check")
		const check = checker.first
		checker.w.terminate()
		out.push({ ...check, commits })
		console.log(JSON.stringify(check))
		if (check.integrity !== "ok" || check.sum % 1000 !== 0) {
			break
		}
	}
	return out
}, { iterations, patch, upstreamPragmas }).catch(err => [{ integrity: `harness: ${err}` }])
await browser.close()
server.close()
const bad = results.filter(r => r && (r.integrity !== "ok" || r.sum % 1000 !== 0))
console.log(`${patch ? "patched" : "unpatched"}${upstreamPragmas ? " (upstream pragmas)" : ""}: ${results.length} kills, ${bad.length} corrupted`)
if (bad.length) {
	console.log("first failure after kill", results.indexOf(bad[0]) + 1, JSON.stringify(bad[0]).slice(0, 300))
}
process.exit(bad.length ? 1 : 0)
