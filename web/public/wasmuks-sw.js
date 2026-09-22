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
// Two jobs: serve decrypted media out of the Cache API (the second half of
// this file), and keep a copy of the app itself so that it opens without a
// network connection. Everything the page needs on a cold start (index.html,
// the JS and CSS, the wasm binaries, fonts) is stored as it is first fetched;
// nothing is downloaded ahead of time, because a normal start fetches all of
// it anyway.
self.addEventListener("install", () => self.skipWaiting())
self.addEventListener("activate", (event) => event.waitUntil(clients.claim()))

// Opened per request rather than held: logout deletes the whole cache, and a
// handle from before that keeps pointing at the deleted (now empty) cache.
const MEDIA_CACHE_NAME = "wasmuks-media-v1"
// The app shell. Kept across logout: it's the program, not user data.
const SHELL_CACHE_NAME = "wasmuks-shell-v1"
// Written by the build (see vite.config.ts): the hashed files under assets/
// of the current build, used to drop the previous build's from the cache.
const ASSET_MANIFEST = "wasmuks-assets.json"
// How long the entry points wait for the network before the cached copy is
// used instead. Only applies when there is a cached copy; a first load waits
// as long as it takes, exactly as it would without a service worker.
const NETWORK_TIMEOUT_MS = 8_000
// Set by the worker on a fallback avatar served because the download failed,
// holding the time another attempt is allowed. Must match wasmuks.ts.
const RETRY_AFTER_HEADER = "X-Gomuks-Retry-After"
const bc = new BroadcastChannel("wasmuks-media-download")
const mediaPromises = new Map()

self.addEventListener("fetch", (evt) => {
	const url = new URL(evt.request.url)
	if (url.origin !== self.location.origin) {
		return
	}
	if (url.pathname.includes("_gomuks/media/")) {
		evt.respondWith(serveFromCache(evt.request).catch(err => {
			console.error("Error serving media from cache", url, err)
			return new Response("Error serving media", {status: 500})
		}))
		return
	}
	const route = shellRoute(evt.request, self.registration.scope)
	if (route?.kind === "asset") {
		evt.respondWith(cacheFirst(evt))
	} else if (route) {
		evt.respondWith(networkFirst(evt, route.key))
	}
})

// The files the server marks no-cache, besides index.html: a new deployment
// changes them, so they come from the network whenever it answers.
const ENTRY_FILES = new Set(["config.json", "manifest.json", ASSET_MANIFEST])
// Unhashed files the page loads on demand.
const ENTRY_DIRS = ["sounds/", "images/", "_gomuks/codeblock/"]

// shellRoute decides how a same-origin GET is served, or null to leave it to
// the browser (the service workers themselves, the embedded call widget, and
// anything else that isn't part of the app). Exported for the tests.
// - "index": any navigation, and index.html itself. The page's update check
//   fetches index.html and compares it to what it loaded, so both must map
//   to the same cache entry, and a navigation to a client-side route gets
//   index.html the same as the server would give it.
// - "asset": files under assets/, content-hashed and immutable.
// - "entry": everything else the app needs, network first.
function shellRoute(request, scope) {
	if (request.method !== "GET") {
		return null
	}
	const url = new URL(request.url)
	const scopePath = new URL(scope).pathname
	if (!url.pathname.startsWith(scopePath)) {
		return null
	}
	const relative = url.pathname.slice(scopePath.length)
	if (relative.startsWith("element-call-embedded/")) {
		return null
	}
	const indexKey = new URL("index.html", scope).href
	// The server answers any path without a file extension with index.html,
	// for client-side routes.
	if (relative === "" || relative === "index.html" || (request.mode === "navigate" && !/\.[a-z0-9]+$/i.test(relative))) {
		return {kind: "index", key: indexKey}
	} else if (relative.startsWith("assets/")) {
		return {kind: "asset", key: url.origin + url.pathname}
	} else if (ENTRY_FILES.has(relative) || ENTRY_DIRS.some(dir => relative.startsWith(dir))) {
		return {kind: "entry", key: url.origin + url.pathname}
	}
	return null
}

// Hashed files never change, so a cached copy is always right. A miss goes
// to the network and is stored on the way through: the wasm binary is
// compiled from the response stream, so it must not be buffered first.
async function cacheFirst(evt) {
	const cache = await caches.open(SHELL_CACHE_NAME)
	const key = evt.request.url
	const hit = await cache.match(key)
	if (hit) {
		return hit
	}
	const resp = await fetch(evt.request)
	if (resp.ok) {
		evt.waitUntil(cache.put(key, resp.clone()).catch(err => {
			console.warn("Failed to cache", key, err)
		}))
	}
	return resp
}

// The network answer is what the browser would have gotten without a service
// worker, and it refreshes the cached copy. The cached copy is used when the
// network fails, or when it hasn't answered within the timeout and there is
// a copy to fall back on. A new index.html means a new build was deployed:
// the previous build's files under assets/ are dropped then.
async function networkFirst(evt, key) {
	const cache = await caches.open(SHELL_CACHE_NAME)
	const hit = await cache.match(key)
	const network = fetch(key, {cache: "no-cache", credentials: "same-origin"}).then(async resp => {
		if (!resp.ok) {
			// A server error while the deployment restarts is no reason to
			// show an error page when the app is right here. A 404 is an
			// answer (config.json is optional), so that goes through.
			if (hit && resp.status >= 500) {
				throw new Error(`HTTP ${resp.status}`)
			}
			return resp
		}
		if (key.endsWith("/index.html") && hit && await sameBody(hit, resp)) {
			return resp
		}
		await cache.put(key, resp.clone())
		if (key.endsWith("/index.html") && hit) {
			await pruneAssets(cache)
		}
		return resp
	})
	if (!hit) {
		return network
	}
	// Keep the worker alive until the refresh is done, even if the cached copy
	// has been returned already.
	evt.waitUntil(network.catch(() => {}))
	try {
		return await Promise.race([
			network,
			new Promise(resolve => setTimeout(() => resolve(hit), NETWORK_TIMEOUT_MS)),
		])
	} catch (err) {
		console.info("Serving", key, "from cache:", err.message)
		return hit
	}
}

// A service worker only sees requests made after it took over, so on the
// very first visit the page loaded everything before this existed. The page
// sends this once its backend is up: by then all of the core files have been
// downloaded, so the fetches below are answered from the HTTP cache (the
// files are immutable) rather than from the network. Later starts go through
// cacheFirst and this finds nothing to do, except after the browser evicted
// the cache.
self.addEventListener("message", evt => {
	if (evt.data?.type === "precache") {
		evt.waitUntil(precacheShell().then(result => evt.source?.postMessage({type: "precached", ...result})))
	}
})

async function precacheShell() {
	const scope = self.registration.scope
	const cache = await caches.open(SHELL_CACHE_NAME)
	const wanted = ["index.html", "config.json", "manifest.json"].map(name => new URL(name, scope).href)
	try {
		const resp = await fetch(new URL(ASSET_MANIFEST, scope).href, {cache: "no-cache"})
		if (resp.ok) {
			const manifest = await resp.json()
			wanted.push(...manifest.core.map(name => new URL(name, scope).href))
		}
	} catch (err) {
		console.warn("Failed to fetch the asset manifest", err)
	}
	let added = 0
	let failed = 0
	for (const key of wanted) {
		if (await cache.match(key)) {
			continue
		}
		try {
			const resp = await fetch(key, {credentials: "same-origin"})
			if (resp.ok) {
				await cache.put(key, resp)
				added++
			} else if (resp.status !== 404) {
				failed++
			}
		} catch {
			failed++
		}
	}
	return {added, failed, total: wanted.length}
}

async function sameBody(a, b) {
	const [textA, textB] = await Promise.all([a.clone().text(), b.clone().text()])
	return textA === textB
}

async function pruneAssets(cache) {
	try {
		const resp = await fetch(new URL(ASSET_MANIFEST, self.registration.scope).href, {cache: "no-cache"})
		if (!resp.ok) {
			return
		}
		const current = new Set((await resp.json()).all.map(name => new URL(name, self.registration.scope).href))
		let dropped = 0
		for (const req of await cache.keys()) {
			if (shellRoute(req, self.registration.scope)?.kind === "asset" && !current.has(req.url)) {
				await cache.delete(req)
				dropped++
			}
		}
		console.info("New build: dropped", dropped, "cached files of the previous one")
	} catch (err) {
		console.warn("Failed to prune the previous build's files", err)
	}
}

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
