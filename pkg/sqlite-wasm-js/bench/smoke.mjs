// Startup smoke test for the wasm build: serves web/dist statically, checks
// that the backend initializes and shows the login screen, that config.json
// defaults are applied, and that a second tab is refused by the Web Lock.
// Usage: node smoke.mjs [path/to/web/dist]
import http from "node:http"
import fs from "node:fs"
import path from "node:path"
import { chromium } from "playwright"

const root = path.resolve(process.argv[2] ?? "../../../web/dist")
const types = {
	".html": "text/html", ".js": "text/javascript", ".wasm": "application/wasm", ".json": "application/json",
	".css": "text/css", ".png": "image/png", ".svg": "image/svg+xml",
}
const configJSON = { preferences: { show_membership_events: false, bogus_key: 1, display_read_receipts: "no" } }

const server = http.createServer((req, res) => {
	const url = new URL(req.url, "http://x")
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
await new Promise(resolve => server.listen(0, "127.0.0.1", resolve))
const port = server.address().port

const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || undefined, headless: true })
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
	page.on("console", msg => logs.push(`[${msg.type()}] ${msg.text()}`))
	page.on("pageerror", err => logs.push(`[pageerror] ${err}`))
}

const tab1 = await context.newPage()
attach(tab1)
await tab1.goto(`http://127.0.0.1:${port}/`)
await tab1.waitForSelector("#mxlogin-username", { timeout: 60000 }).catch(() => null)
const state = await tab1.evaluate(() => ({
	client: window.client?.state?.current,
	conn: window.client?.rpc?.connect?.current,
	config: window.client?.store?.configPreferenceCache,
	membership: window.client?.store?.preferences?.show_membership_events,
	receipts: window.client?.store?.preferences?.display_read_receipts,
	storage: window.client?.rpc?.storageStatus?.current,
	loginForm: Boolean(document.querySelector("#mxlogin-username")),
}))
check(state.conn?.connected === true && !state.conn?.error, "worker connected without error")
check(state.client?.is_initialized === true && state.client?.is_logged_in === false, "backend initialized, not logged in")
check(state.loginForm, "login form rendered")
check(state.membership === false, "config.json default applied (show_membership_events=false)")
check(state.receipts === true, "config.json value with wrong type ignored")
check(typeof state.storage?.persisted === "boolean" || state.storage?.persisted === null, "storage status reported")
check(logs.some(l => /Initialization complete/.test(l)), "backend logged initialization complete")
check(!logs.some(l => /panic|pageerror|deadlock/i.test(l)), "no panics or page errors")
check(logs.some(l => /Generated new pickle key/.test(l)), "fresh install generated a pickle key")
check(!logs.some(l => /No pickle key provided/.test(l)), "backend received the pickle key")

const tab2 = await context.newPage()
attach(tab2)
await tab2.goto(`http://127.0.0.1:${port}/`)
const locked = await tab2.waitForSelector("text=already open in another tab", { timeout: 20000 }).catch(() => null)
check(Boolean(locked), "second tab refused by the Web Lock")
await tab2.close()

// Closing the locked-out tab must not affect the first one.
await tab1.waitForTimeout(500)
check((await tab1.evaluate(() => window.client?.rpc?.connect?.current?.connected)) === true, "first tab still connected")

await browser.close()
server.close()
if (failures.length) {
	console.log("---- logs ----")
	for (const l of logs) {
		console.log(l.slice(0, 300))
	}
	console.log(`SMOKE FAILED: ${failures.length} check(s)`)
	process.exit(1)
}
console.log("SMOKE OK")
