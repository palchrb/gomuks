// Cold-start measurement for the wasm build: serves web/dist, opens it in a
// fresh browser (so both the HTTP cache and Chromium's compiled-code cache are
// cold) and times how long until the Go backend logs "Initialization
// complete". Prints the phases in between and counts how many times the wasm
// binary is fetched.
//
// The interesting case is a throttled connection, because that is where the
// download dominates and a laptop on loopback says nothing:
//
//   node startup.mjs 3                      # local, as fast as the disk
//   THROTTLE_MBPS=10 node startup.mjs 3     # like a phone on mobile data
//
// --hint preload|prefetch adds a <link> for the wasm binary to index.html.
// Both look like free wins and are not: see the comment on loadIndex in
// cmd/wasmukserve/main.go.
//
// Pre-compressed sidecars are served when they exist, so gzip or brotli the
// dist first if the download size is part of what you're measuring.
import http from "node:http"
import fs from "node:fs"
import path from "node:path"
import { launchChromium } from "./browser.js"

const runs = Number(process.argv[2] ?? 3)
const hintArg = process.argv.indexOf("--hint")
const hint = hintArg >= 0 ? process.argv[hintArg + 1] : null
const throttleMbps = Number(process.env.THROTTLE_MBPS ?? 0)
const latencyMS = Number(process.env.LATENCY_MS ?? 60)
const readyTimeoutMS = Number(process.env.READY_TIMEOUT_MS ?? 120000)
const root = path.resolve(process.env.DIST ?? "../../../web/dist")
const types = {
	".html": "text/html", ".js": "text/javascript", ".wasm": "application/wasm", ".json": "application/json",
	".css": "text/css", ".png": "image/png", ".svg": "image/svg+xml",
}
const encodings = [["br", ".br"], ["gzip", ".gz"]]
const wasmAsset = fs.readdirSync(path.join(root, "assets"))
	.find(f => f.startsWith("_gomuks-") && f.endsWith(".wasm"))
if (!wasmAsset) {
	console.error(`no _gomuks-*.wasm in ${root}/assets: build the frontend first`)
	process.exit(1)
}

let wasmRequests = 0
const server = http.createServer((req, res) => {
	const url = new URL(req.url, "http://x")
	const file = path.join(root, url.pathname === "/" ? "index.html" : url.pathname)
	if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) {
		res.writeHead(404)
		res.end("not found")
		return
	}
	const headers = { "Content-Type": types[path.extname(file)] ?? "application/octet-stream" }
	if (url.pathname.startsWith("/assets/")) {
		headers["Cache-Control"] = "public, max-age=31536000, immutable"
	}
	if (url.pathname.endsWith(".wasm") && url.pathname.includes("_gomuks")) {
		wasmRequests++
		const num = wasmRequests
		const start = Date.now()
		let sent = 0
		res.on("pipe", src => src.on("data", chunk => {
			sent += chunk.length
		}))
		res.on("close", () => console.log(
			`    wasm request ${num}: ${(sent / 1048576).toFixed(1)} MiB in ${Date.now() - start} ms`
			+ `, ${res.writableFinished ? "complete" : "CANCELLED"}`))
	}
	if (url.pathname === "/" || url.pathname === "/index.html") {
		let html = fs.readFileSync(file, "utf-8")
		if (hint) {
			const as = hint === "preload" ? ` as="fetch"` : ""
			html = html.replace("<!-- etag placeholder -->",
				`<link rel="${hint}"${as} href="./assets/${wasmAsset}">`)
		}
		res.writeHead(200, headers)
		res.end(html)
		return
	}
	const accept = req.headers["accept-encoding"] ?? ""
	for (const [name, ext] of encodings) {
		if (accept.includes(name) && fs.existsSync(file + ext)) {
			res.writeHead(200, { ...headers, "Content-Encoding": name, "Vary": "Accept-Encoding" })
			fs.createReadStream(file + ext).pipe(res)
			return
		}
	}
	res.writeHead(200, headers)
	fs.createReadStream(file).pipe(res)
})
await new Promise(resolve => server.listen(0, "127.0.0.1", resolve))
const baseURL = `http://127.0.0.1:${server.address().port}`

// Phases worth seeing on the way to a started backend.
const interesting = /wasm ready after|SQLite|pickle key|Initialization complete/i
const times = []
for (let run = 1; run <= runs; run++) {
	wasmRequests = 0
	const browser = await launchChromium()
	const context = await browser.newContext()
	const page = await context.newPage()
	if (throttleMbps) {
		const cdp = await context.newCDPSession(page)
		await cdp.send("Network.enable")
		await cdp.send("Network.emulateNetworkConditions", {
			offline: false,
			latency: latencyMS,
			downloadThroughput: (throttleMbps * 1e6) / 8,
			uploadThroughput: (throttleMbps * 1e6) / 8,
		})
	}
	const start = performance.now()
	const marks = []
	const errors = []
	page.on("pageerror", err => errors.push(`${err}`))
	const ready = new Promise(resolve => page.on("console", msg => {
		const text = msg.text()
		if (msg.type() === "error") {
			errors.push(text)
		}
		if (interesting.test(text)) {
			marks.push(`${Math.round(performance.now() - start)} ms: ${text.slice(0, 120)}`)
		}
		if (/Initialization complete/.test(text)) {
			resolve(performance.now())
		}
	}))
	await page.goto(`${baseURL}/`)
	const readyAt = await Promise.race([
		ready,
		new Promise(resolve => setTimeout(() => resolve(null), readyTimeoutMS)),
	])
	const ms = readyAt === null ? null : Math.round(readyAt - start)
	console.log(`run ${run}: ${ms === null ? "NEVER STARTED" : `${ms} ms`}, wasm requests: ${wasmRequests}`)
	for (const mark of marks) {
		console.log(`    ${mark}`)
	}
	for (const error of errors) {
		console.log(`    [error] ${error.slice(0, 200)}`)
	}
	if (ms !== null) {
		times.push(ms)
	}
	await browser.close()
}
server.close()
times.sort((a, b) => a - b)
if (times.length) {
	console.log(`\n${throttleMbps ? `${throttleMbps} Mbps` : "unthrottled"}`
		+ `${hint ? `, ${hint} hint` : ""}: min ${times[0]}`
		+ ` median ${times[Math.floor(times.length / 2)]} max ${times[times.length - 1]} ms`
		+ ` (${times.length}/${runs} runs started)`)
}
