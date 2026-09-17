// Playwright runner for the sqlite-wasm-js benchmark. Usage: node run.js [n] [variant,variant,...]
import { chromium } from "playwright"
import { serve } from "./serve.js"

const n = process.argv[2] ?? "2000"
const { server, port } = await serve(0)
const browser = await chromium.launch({
	executablePath: process.env.CHROMIUM_PATH || undefined,
	headless: true,
})
const page = await browser.newPage()
page.on("console", msg => process.stderr.write(`[browser] ${msg.text()}\n`))
page.on("pageerror", err => process.stderr.write(`[pageerror] ${err}\n`))
await page.goto(`http://127.0.0.1:${port}/?n=${n}&v=${encodeURIComponent(process.argv[3] ?? "")}`)
const result = await page.waitForFunction(() => window.benchResult || window.benchError, null, { timeout: 600000, polling: 200 })
const value = await result.jsonValue()
console.log(JSON.stringify(value, null, 2))
await browser.close()
server.close()
