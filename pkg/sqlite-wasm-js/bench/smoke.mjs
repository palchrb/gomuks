// Startup smoke test for the wasm build: serves web/dist statically, checks
// that the backend initializes and shows the login screen, that config.json
// defaults are applied, and that a second tab is refused by the Web Lock.
// Usage: node smoke.mjs [path/to/web/dist]
//        node smoke.mjs --url http://127.0.0.1:8181   (test an already running server;
//        it must serve a config.json equivalent to configJSON below)
import http from "node:http"
import fs from "node:fs"
import path from "node:path"
import { launchChromium } from "./browser.js"

const urlArg = process.argv.indexOf("--url")
const externalURL = urlArg >= 0 ? process.argv[urlArg + 1] : null
const root = path.resolve(externalURL ? "." : (process.argv[2] ?? "../../../web/dist"))
const types = {
	".html": "text/html", ".js": "text/javascript", ".wasm": "application/wasm", ".json": "application/json",
	".css": "text/css", ".png": "image/png", ".svg": "image/svg+xml",
}
export const configJSON = {
	preferences: { show_membership_events: false, bogus_key: 1, display_read_receipts: "no" },
	wasm: { memory_limit_mb: 256, initial_timeline_limit: 10 },
}

const noConfig = Boolean(process.env.SMOKE_NO_CONFIG)
const server = http.createServer((req, res) => {
	const url = new URL(req.url, "http://x")
	if (url.pathname === "/config.json" && noConfig) {
		res.writeHead(404)
		res.end("not found")
		return
	}
	if (url.pathname === "/config.json") {
		res.writeHead(200, { "Content-Type": "application/json" })
		res.end(JSON.stringify(configJSON))
		return
	}
	const file = path.join(root, url.pathname === "/" ? "index.html" : url.pathname)
	if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) {
		res.writeHead(404)
		res.end("not found")
		return
	}
	res.writeHead(200, { "Content-Type": types[path.extname(file)] ?? "application/octet-stream" })
	fs.createReadStream(file).pipe(res)
})
let baseURL = externalURL
if (!baseURL) {
	await new Promise(resolve => server.listen(0, "127.0.0.1", resolve))
	baseURL = `http://127.0.0.1:${server.address().port}`
}

const browser = await launchChromium()
const context = await browser.newContext()
const failures = []
const check = (cond, msg) => {
	console.log(`${cond ? "ok  " : "FAIL"} ${msg}`)
	if (!cond) {
		failures.push(msg)
	}
}
const logs = []
const attach = page => {
	page.on("console", msg => {
		logs.push(`[${msg.type()}] ${msg.text()}`)
		if (/precached the app shell/.test(msg.text())) {
			page.evaluate(() => { window.__smokePrecached = true }).catch(() => {})
		}
	})
	page.on("pageerror", err => logs.push(`[pageerror] ${err}`))
}

const tab1 = await context.newPage()
attach(tab1)
await tab1.goto(`${baseURL}/`)
await tab1.waitForSelector("#mxlogin-username", { timeout: 60000 }).catch(() => null)
const state = await tab1.evaluate(async () => ({
	client: window.client?.state?.current,
	conn: window.client?.rpc?.connect?.current,
	config: window.client?.store?.configPreferenceCache,
	membership: window.client?.store?.preferences?.show_membership_events,
	receipts: window.client?.store?.preferences?.display_read_receipts,
	storage: window.client?.rpc?.storageStatus?.current,
	loginForm: Boolean(document.querySelector("#mxlogin-username")),
	pickleKeyInOPFS: await navigator.storage.getDirectory()
		.then(root => root.getFileHandle("pickle.key")).then(() => true, () => false),
}))
check(state.conn?.connected === true && !state.conn?.error, "worker connected without error")
check(state.client?.is_initialized === true && state.client?.is_logged_in === false, "backend initialized, not logged in")
check(state.loginForm, "login form rendered")
if (noConfig) {
	check(state.membership === true, "no config.json: built-in default kept")
	check(logs.some(l => /wasm configuration.*memory_limit_mb: 512/.test(l)), "no config.json: default wasm settings")
} else {
	check(state.membership === false, "config.json default applied (show_membership_events=false)")
	check(state.receipts === true, "config.json value with wrong type ignored")
}
check(typeof state.storage?.persisted === "boolean" || state.storage?.persisted === null, "storage status reported")
check(logs.some(l => /Initialization complete/.test(l)), "backend logged initialization complete")
check(!logs.some(l => /panic|pageerror|deadlock/i.test(l)), "no panics or page errors")
if (!noConfig) {
	check(logs.some(l => /wasm configuration.*memory_limit_mb: 256/.test(l)),
		"config.json wasm section reached the backend")
	check(logs.some(l => /Ignoring initial_timeline_limit/.test(l)),
		"removed config.json setting is reported, not applied")
}
check(!logs.some(l => /still in use after startup/.test(l)), "no database connection left in use after startup")
check(state.pickleKeyInOPFS === true, "pickle key stored in OPFS")
check(logs.some(l => /Generated new pickle key/.test(l)), "fresh install generated a pickle key")
check(!logs.some(l => /No pickle key provided/.test(l)), "backend received the pickle key")

// Offline start: the service worker stored the app shell on this first visit,
// so with the server gone a reload must still bring up the login screen. The
// server is really stopped rather than emulated, so the worker's own fetches
// fail too. Not possible against an external server.
if (!externalURL) {
	const precached = await tab1.waitForFunction(
		() => window.__smokePrecached === true, null, { timeout: 60000 },
	).catch(() => null)
	check(Boolean(precached), "service worker precached the app shell")
	server.closeAllConnections()
	await new Promise(resolve => server.close(resolve))
	logs.length = 0
	await tab1.reload()
	await tab1.waitForSelector("#mxlogin-username", { timeout: 60000 }).catch(() => null)
	const offline = await tab1.evaluate(async () => ({
		conn: window.client?.rpc?.connect?.current,
		client: window.client?.state?.current,
		loginForm: Boolean(document.querySelector("#mxlogin-username")),
		controlled: navigator.serviceWorker.controller !== null,
		cached: (await (await caches.open("wasmuks-shell-v1")).keys()).map(req => new URL(req.url).pathname),
	}))
	check(offline.controlled, "offline: page controlled by the service worker")
	check(offline.cached.includes("/index.html") && offline.cached.some(p => /_gomuks-.*\.wasm$/.test(p)),
		`offline: shell cache holds index.html and the wasm binary (${offline.cached.length} files)`)
	check(offline.conn?.connected === true && !offline.conn?.error, "offline: worker connected without error")
	check(offline.client?.is_initialized === true, "offline: backend initialized")
	check(offline.loginForm, "offline: login form rendered")
	await new Promise(resolve => server.listen(new URL(baseURL).port, "127.0.0.1", resolve))
}

const tab2 = await context.newPage()
attach(tab2)
await tab2.goto(`${baseURL}/`)
const locked = await tab2.waitForSelector("text=already open in another tab", { timeout: 20000 }).catch(() => null)
check(Boolean(locked), "second tab refused by the Web Lock")
await tab2.close()

// Closing the locked-out tab must not affect the first one.
await tab1.waitForTimeout(500)
check((await tab1.evaluate(() => window.client?.rpc?.connect?.current?.connected)) === true, "first tab still connected")

await browser.close()
if (!externalURL) {
	server.close()
}
if (failures.length) {
	console.log("---- logs ----")
	for (const l of logs) {
		console.log(l.slice(0, 300))
	}
	console.log(`SMOKE FAILED: ${failures.length} check(s)`)
	process.exit(1)
}
console.log("SMOKE OK")
