// gomuks - A Matrix client written in Go.
// Copyright (C) 2025 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.
import { readFileSync } from "fs"
import vm from "vm"
import { expect, suite, test } from "vitest"

// The media service worker isn't a module, so it's loaded into a context with
// the browser globals it expects. Range handling is worth testing because the
// server build gets it from Go's http.ServeContent, while here it is ours, and
// Safari won't play audio or video without it.
interface ServiceWorkerScope {
	sliceRange: (hit: Response, range: string) => Promise<Response | null>
	withAcceptRanges: (hit: Response) => Response
}

function loadServiceWorker(): ServiceWorkerScope {
	const src = readFileSync(`${__dirname}/../../../public/wasmuks-media-sw.js`, "utf-8")
	const context = {
		self: { addEventListener: () => {}, location: { origin: "https://example.invalid" }},
		caches: { open: async () => ({}) },
		BroadcastChannel: class {
			addEventListener() {}
			postMessage() {}
		},
		clients: { claim: () => {} },
		console, URL, Response, Headers, Request, setTimeout, clearTimeout,
	}
	vm.createContext(context)
	vm.runInContext(src, context)
	return context as unknown as ServiceWorkerScope
}

const sw = loadServiceWorker()
const size = 1000
const body = new Uint8Array(size).map((_, i) => i % 256)
const cached = () => new Response(body, { headers: { "Content-Type": "video/mp4" }})

suite("media service worker range requests", () => {
	test("an open-ended range returns the whole file as partial content", async () => {
		const resp = (await sw.sliceRange(cached(), "bytes=0-"))!
		expect(resp.status).toBe(206)
		expect(resp.headers.get("Content-Range")).toBe(`bytes 0-${size - 1}/${size}`)
		expect(resp.headers.get("Content-Length")).toBe(String(size))
		expect(resp.headers.get("Accept-Ranges")).toBe("bytes")
		expect(resp.headers.get("Content-Type")).toBe("video/mp4")
	})

	test("a closed range returns exactly those bytes", async () => {
		const resp = (await sw.sliceRange(cached(), "bytes=100-199"))!
		expect(resp.status).toBe(206)
		expect(resp.headers.get("Content-Range")).toBe(`bytes 100-199/${size}`)
		const got = new Uint8Array(await resp.arrayBuffer())
		expect(got.length).toBe(100)
		expect(got[0]).toBe(100)
		expect(got[99]).toBe(199)
	})

	test("a suffix range counts back from the end", async () => {
		const resp = (await sw.sliceRange(cached(), "bytes=-50"))!
		expect(resp.headers.get("Content-Range")).toBe(`bytes 950-${size - 1}/${size}`)
	})

	test("an end past the file is clamped", async () => {
		const resp = (await sw.sliceRange(cached(), "bytes=900-5000"))!
		expect(resp.headers.get("Content-Range")).toBe(`bytes 900-${size - 1}/${size}`)
	})

	test("a start past the file is rejected with the size", async () => {
		const resp = (await sw.sliceRange(cached(), "bytes=2000-3000"))!
		expect(resp.status).toBe(416)
		expect(resp.headers.get("Content-Range")).toBe(`bytes */${size}`)
	})

	test("a range we don't understand falls through to the whole file", async () => {
		expect(await sw.sliceRange(cached(), "bytes=abc")).toBeNull()
		expect(await sw.sliceRange(cached(), "items=0-1")).toBeNull()
		expect(await sw.sliceRange(cached(), "bytes=-")).toBeNull()
	})

	test("the whole file still says ranges are available", () => {
		const resp = sw.withAcceptRanges(cached())
		expect(resp.status).toBe(200)
		expect(resp.headers.get("Accept-Ranges")).toBe("bytes")
		expect(resp.headers.get("Content-Type")).toBe("video/mp4")
	})
})
