import http from "node:http"
import fs from "node:fs"
import path from "node:path"

const root = path.dirname(new URL(import.meta.url).pathname)
const types = { ".html": "text/html", ".js": "text/javascript", ".mjs": "text/javascript", ".wasm": "application/wasm", ".json": "application/json" }

// BENCH_HOST=0.0.0.0 makes the page reachable from another device, which is
// how you measure a phone or the machine you actually use as a client.
const host = process.env.BENCH_HOST ?? "127.0.0.1"

export function serve(port = 0) {
	const server = http.createServer((req, res) => {
		const url = new URL(req.url, "http://x")
		let file = path.join(root, url.pathname === "/" ? "index.html" : url.pathname)
		if (!file.startsWith(root) || !fs.existsSync(file)) {
			res.writeHead(404); res.end("not found"); return
		}
		res.writeHead(200, {
			"Content-Type": types[path.extname(file)] ?? "application/octet-stream",
			"Cross-Origin-Opener-Policy": "same-origin",
			"Cross-Origin-Embedder-Policy": "require-corp",
		})
		fs.createReadStream(file).pipe(res)
	})
	return new Promise(resolve => server.listen(port, host, () => resolve({ server, port: server.address().port })))
}

if (process.argv[1] === new URL(import.meta.url).pathname) {
	serve(29399).then(({ port }) => console.log(`http://${host}:${port}/`))
}
