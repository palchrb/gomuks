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
self.addEventListener("install", () => self.skipWaiting())
self.addEventListener("activate", (event) => event.waitUntil(clients.claim()))

// Opened per request rather than held: logout deletes the whole cache, and a
// handle from before that keeps pointing at the deleted (now empty) cache.
const MEDIA_CACHE_NAME = "wasmuks-media-v1"
// Set by the worker on a fallback avatar served because the download failed,
// holding the time another attempt is allowed. Must match wasmuks.ts.
const RETRY_AFTER_HEADER = "X-Gomuks-Retry-After"
const bc = new BroadcastChannel("wasmuks-media-download")
const mediaPromises = new Map()

self.addEventListener("fetch", (evt) => {
	const url = new URL(evt.request.url)
	if (url.origin === self.location.origin && url.pathname.includes("_gomuks/media/")) {
		evt.respondWith(serveFromCache(evt.request).catch(err => {
			console.error("Error serving media from cache", url, err)
			return new Response("Error serving media", {status: 500})
		}))
	}
})

bc.addEventListener("message", evt => {
	if (evt.data.type === "response") {
		const waiter = mediaPromises.get(evt.data.url)
		if (waiter) {
			waiter.resolve()
			mediaPromises.delete(evt.data.url)
		}
	}
})

// If no tab with the wasm worker is open, nobody will ever answer the request.
// Generous because the homeserver may have to fetch federated media first.
const MEDIA_REQUEST_TIMEOUT_MS = 90_000

async function requestAndWaitForMedia(url) {
	if (mediaPromises.has(url)) {
		return mediaPromises.get(url).promise
	}
	let resolve, reject
	const promise = new Promise((innerResolve, innerReject) => {
		resolve = innerResolve
		reject = innerReject
	})
	const timeout = setTimeout(() => {
		mediaPromises.delete(url)
		reject(new Error("Timed out waiting for the wasm worker to fetch media"))
	}, MEDIA_REQUEST_TIMEOUT_MS)
	// The worker still caches the response when it eventually arrives, so a
	// reload after a timeout shows the media.
	mediaPromises.set(url, {resolve: () => {
		clearTimeout(timeout)
		resolve()
	}, promise})
	try {
		bc.postMessage({type: "request", url})
	} catch (err) {
		clearTimeout(timeout)
		mediaPromises.delete(url)
		throw new Error("PostMessage failed")
	}
	return promise
}

// Must match mediaCacheKey in src/api/wasm/wasmuks.ts.
function mediaCacheKey(url) {
	const key = new URL(url)
	const thumbnail = key.searchParams.get("thumbnail")
	key.search = thumbnail ? `?thumbnail=${encodeURIComponent(thumbnail)}` : ""
	return key.href
}

// A fallback avatar is only good until the backoff on the failed download
// expires, which is how long the server build tells browsers to cache it for.
function isExpired(response) {
	const retryAfter = Number(response.headers.get(RETRY_AFTER_HEADER))
	return retryAfter > 0 && Date.now() >= retryAfter
}

// Media elements ask for byte ranges, and Safari refuses to play audio or
// video at all if the response isn't range-capable. The server build gets
// this from http.ServeContent; here the whole body is in the cache already,
// so answer from it. Returns null when the header is one we don't handle, in
// which case the caller falls back to the whole file.
async function sliceRange(hit, rangeHeader) {
	const parsed = /^bytes=(\d*)-(\d*)$/.exec(rangeHeader.trim())
	if (!parsed) {
		return null
	}
	const hasStart = parsed[1] !== ""
	const hasEnd = parsed[2] !== ""
	if (!hasStart && !hasEnd) {
		return null
	}
	const body = await hit.arrayBuffer()
	const total = body.byteLength
	let start, end
	if (!hasStart) {
		// "bytes=-500" means the last 500 bytes.
		start = Math.max(total - Number(parsed[2]), 0)
		end = total - 1
	} else {
		start = Number(parsed[1])
		end = hasEnd ? Math.min(Number(parsed[2]), total - 1) : total - 1
	}
	const headers = new Headers(hit.headers)
	headers.set("Accept-Ranges", "bytes")
	if (start > end || start >= total) {
		headers.set("Content-Range", `bytes */${total}`)
		return new Response(null, {status: 416, headers})
	}
	headers.set("Content-Range", `bytes ${start}-${end}/${total}`)
	headers.set("Content-Length", String(end - start + 1))
	return new Response(body.slice(start, end + 1), {
		status: 206,
		statusText: "Partial Content",
		headers,
	})
}

// withAcceptRanges re-announces that ranges are available. Headers on a
// response that came out of the cache are immutable, so this builds a new one
// around the same body rather than buffering it.
function withAcceptRanges(hit) {
	const headers = new Headers(hit.headers)
	headers.set("Accept-Ranges", "bytes")
	return new Response(hit.body, {status: hit.status, statusText: hit.statusText, headers})
}

async function serveFromCache(request) {
	const cache = await caches.open(MEDIA_CACHE_NAME)
	const cacheKey = mediaCacheKey(request.url)
	let hit = await cache.match(cacheKey)
	if (hit && isExpired(hit)) {
		console.log("Cached fallback expired for", request.url)
		await cache.delete(cacheKey)
		hit = undefined
	}
	if (hit && !hit.ok) {
		// An older version stored failed downloads as an error response, which
		// made one bad download permanent. Drop those and try again.
		console.log("Discarding cached error for", request.url)
		await cache.delete(cacheKey)
		hit = undefined
	}
	if (!hit) {
		await requestAndWaitForMedia(request.url)
		// Re-open: the worker may have recreated the cache in the meantime.
		hit = await (await caches.open(MEDIA_CACHE_NAME)).match(cacheKey)
		if (hit && !hit.ok) {
			hit = undefined
		}
		if (!hit) {
			console.log("Cache entry not found after request for", request.url)
			return new Response("Cache entry not found", {status: 404})
		} else {
			console.log("Found cache entry after request for", request.url)
		}
	} else {
		console.log("Cache hit for", request.url)
	}
	const rangeHeader = request.headers.get("Range")
	if (rangeHeader) {
		const ranged = await sliceRange(hit, rangeHeader)
		if (ranged) {
			return ranged
		}
	}
	return withAcceptRanges(hit)
}
