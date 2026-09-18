// Repeats the benchmark in a fresh browser process several times and
// summarises the spread, because a single run on a loaded machine is not
// worth much. Usage: node runs.js [runs] [rows] [variant,variant,...]
//
// Each run is a separate `node run.js`, so nothing is shared between them:
// new browser, new database files, cold caches. That is the variance that
// matters when comparing two configurations.
import { execFile } from "node:child_process"
import { writeFileSync } from "node:fs"
import { promisify } from "node:util"

const execFileAsync = promisify(execFile)

const runs = Number(process.argv[2] ?? 10)
const rows = process.argv[3] ?? "2000"
const variants = process.argv[4] ?? ""
const metrics = ["insert_ms", "select_all_ms", "point_lookup_ms"]
const metricNames = {
	insert_ms: `insert ${rows} rows`,
	select_all_ms: `read ${rows} rows`,
	point_lookup_ms: "500 lookups",
}

if (!Number.isInteger(runs) || runs < 1) {
	console.error("First argument must be the number of runs")
	process.exit(2)
}

function median(values) {
	const sorted = [...values].sort((a, b) => a - b)
	const mid = sorted.length >> 1
	return sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2
}

const results = []
for (let i = 1; i <= runs; i++) {
	process.stderr.write(`run ${i}/${runs}... `)
	const started = Date.now()
	try {
		const { stdout } = await execFileAsync(
			process.execPath, ["run.js", rows, variants],
			{ maxBuffer: 16 << 20, env: process.env },
		)
		results.push(JSON.parse(stdout))
		process.stderr.write(`${((Date.now() - started) / 1000).toFixed(1)}s\n`)
	} catch (err) {
		process.stderr.write("failed\n")
		const detail = `${err.stderr ?? ""}`.trim().split("\n").slice(-6).join("\n")
		process.stderr.write(`${detail || err.message}\n`)
	}
}

if (results.length === 0) {
	console.error("Every run failed. Did you run ./build.sh, and is Chromium available?")
	console.error("On a machine without Playwright's own browser, set CHROMIUM_PATH to the system one.")
	process.exit(1)
}

const stamp = new Date().toISOString().replace(/[:.]/g, "-")
const rawFile = `results-${stamp}.json`
writeFileSync(rawFile, JSON.stringify({ runs: results.length, rows, variants, results }, null, 2))

const keys = [...new Set(results.flatMap(r => Object.keys(r)))]
	.filter(key => key.includes("/"))
	.sort()

console.log(`\n${results.length} runs of ${rows} rows, milliseconds, lower is better`)
console.log(`raw results written to ${rawFile}\n`)
const pad = Math.max(...keys.map(k => k.length))
console.log(
	"variant".padEnd(pad),
	"operation".padEnd(16),
	"min".padStart(7), "median".padStart(7), "mean".padStart(7), "max".padStart(7), "spread".padStart(7),
)
for (const key of keys) {
	for (const metric of metrics) {
		const values = results
			.map(r => r[key]?.[metric])
			.filter(v => typeof v === "number")
		if (values.length === 0) {
			continue
		}
		const min = Math.min(...values)
		const max = Math.max(...values)
		const mean = values.reduce((a, b) => a + b, 0) / values.length
		console.log(
			key.padEnd(pad),
			metricNames[metric].padEnd(16),
			min.toFixed(1).padStart(7),
			median(values).toFixed(1).padStart(7),
			mean.toFixed(1).padStart(7),
			max.toFixed(1).padStart(7),
			`${(max / min).toFixed(2)}x`.padStart(7),
		)
	}
}

const crossings = results.map(r => r.crossing).filter(Boolean)
if (crossings.length > 0) {
	const ints = crossings.map(c => c.call_int_ns).filter(v => typeof v === "number")
	const strings = crossings.map(c => c.call_string_ns).filter(v => typeof v === "number")
	console.log("\nOne call from Go into JavaScript, nanoseconds (median of runs):")
	if (ints.length) {
		console.log(`  with a number: ${median(ints).toFixed(0)}`)
	}
	if (strings.length) {
		console.log(`  with a string: ${median(strings).toFixed(0)}`)
	}
}

const failed = runs - results.length
if (failed > 0) {
	console.log(`\n${failed} of ${runs} runs failed and are not included.`)
}
