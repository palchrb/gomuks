// Playwright runner for the sqlite-wasm-js benchmark. Usage: node run.js [n] [variant,variant,...]
import { existsSync } from "node:fs"
import { launchChromium } from "./browser.js"
import { serve } from "./serve.js"

for (const required of ["bench.wasm", "wasm_exec.js", "sqlite_bridge.js", "sqlite3.wasm"]) {
	if (!existsSync(new URL(required, import.meta.url))) {
		process.stderr.write(`${required} is missing, run ./build.sh first\n`)
		process.exit(2)
	}
}

const n = process.argv[2] ?? "2000"
const { server, port } = await serve(0)
const browser = await launchChromium()
const page = await browser.newPage()
page.on("console", msg => process.stderr.write(`[browser] ${msg.text()}\n`))
page.on("pageerror", err => process.stderr.write(`[pageerror] ${err}\n`))
await page.goto(`http://127.0.0.1:${port}/?n=${n}&v=${encodeURIComponent(process.argv[3] ?? "")}`)
await page.waitForFunction(() => window.benchResult || window.benchError, null, { timeout: 600000, polling: 200 })
const { result, error } = await page.evaluate(() => ({
	result: window.benchResult ?? null,
	error: window.benchError ? String(window.benchError) : null,
}))
await browser.close()
server.close()
if (!result) {
	process.stderr.write(`benchmark failed in the browser: ${error ?? "no result"}\n`)
	process.exit(1)
}
console.log(JSON.stringify(result, null, 2))
