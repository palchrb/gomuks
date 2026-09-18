// Finds a Chromium to run the benchmark in. CHROMIUM_PATH wins; otherwise
// Playwright's own download is tried first and the distribution's browser
// second, so this works on a machine where Playwright has no build for the
// architecture (a Raspberry Pi, for example).
import { existsSync } from "node:fs"
import { chromium } from "playwright"

const browsersPath = process.env.PLAYWRIGHT_BROWSERS_PATH
const systemPaths = [
	...(browsersPath ? [
		`${browsersPath}/chromium`,
		`${browsersPath}/chromium/chrome-linux/chrome`,
	] : []),
	"/usr/bin/chromium",
	"/usr/bin/chromium-browser",
	"/usr/bin/google-chrome",
	"/usr/bin/google-chrome-stable",
	"/snap/bin/chromium",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
]

export async function launchChromium(options = {}) {
	const launch = opts => chromium.launch({ headless: true, ...options, ...opts })
	if (process.env.CHROMIUM_PATH) {
		return launch({ executablePath: process.env.CHROMIUM_PATH })
	}
	try {
		return await launch({})
	} catch (err) {
		const fallback = systemPaths.find(p => existsSync(p))
		if (!fallback) {
			throw err
		}
		process.stderr.write(`Playwright's Chromium is unavailable, using ${fallback}\n`)
		return launch({ executablePath: fallback })
	}
}
