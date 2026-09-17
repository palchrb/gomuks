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
/// <reference types="golang-wasm-exec" />
import initGomuksWasm from "./_gomuks.wasm?init"
import "./go_wasm_exec.js"
import initSqlite from "./sqlite_bridge.ts"

interface MediaResponse {
	buffer: Uint8Array<ArrayBuffer>
	contentType: string
	contentDisposition: string
}

declare global {
	interface Window {
		wasmuksPickleKey: Uint8Array
		meowDownloadMedia: (
			path: string,
			query: string,
			callbacks: {
				resolve: (data: MediaResponse) => void,
				reject: () => void
			},
		) => void
	}
}

// Cache key for a media URL: the path plus the thumbnail parameter, so the
// thumbnail and the full image are separate entries but other parameters
// (encryption flag, fallback avatar text) don't cause duplicates. Must match
// mediaCacheKey in public/wasmuks-media-sw.js.
export function mediaCacheKey(url: URL): string {
	const key = new URL(url.href)
	const thumbnail = key.searchParams.get("thumbnail")
	key.search = thumbnail ? `?thumbnail=${encodeURIComponent(thumbnail)}` : ""
	return key.href
}

async function setupMediaChannel() {
	const bc = new BroadcastChannel("wasmuks-media-download")
	const cache = await caches.open("wasmuks-media-v1")
	bc.addEventListener("message", async evt => {
		if (evt.data.type !== "request") {
			return
		}
		const parsedURL = new URL(evt.data.url)
		const cacheKey = mediaCacheKey(parsedURL)
		try {
			const result = await new Promise<MediaResponse>((resolve, reject) => {
				self.meowDownloadMedia(parsedURL.pathname, parsedURL.search, { resolve, reject })
			})
			const headers: Record<string, string> = {
				"Content-Type": result.contentType,
			}
			if (result.contentDisposition) {
				headers["Content-Disposition"] = result.contentDisposition
			}
			await cache.put(cacheKey, new Response(result.buffer, { status: 200, headers }))
			bc.postMessage({ type: "response", url: evt.data.url })
		} catch (err) {
			console.error("Error handling media download request:", err)
			await cache.put(cacheKey, new Response("Failed to download", { status: 500 }))
			bc.postMessage({ type: "response", url: evt.data.url, failed: true })
		}
	})
}

const KV_DB_NAME = "gomuks-wasm"
const KV_STORE = "kv"
const PICKLE_KEY = "pickle_key"

function openKV(): Promise<IDBDatabase> {
	return new Promise((resolve, reject) => {
		const req = indexedDB.open(KV_DB_NAME, 1)
		req.onupgradeneeded = () => req.result.createObjectStore(KV_STORE)
		req.onsuccess = () => resolve(req.result)
		req.onerror = () => reject(req.error)
	})
}

function kvGet(db: IDBDatabase, key: string): Promise<unknown> {
	return new Promise((resolve, reject) => {
		const req = db.transaction(KV_STORE, "readonly").objectStore(KV_STORE).get(key)
		req.onsuccess = () => resolve(req.result)
		req.onerror = () => reject(req.error)
	})
}

function kvPut(db: IDBDatabase, key: string, value: unknown): Promise<void> {
	return new Promise((resolve, reject) => {
		const txn = db.transaction(KV_STORE, "readwrite")
		txn.objectStore(KV_STORE).put(value, key)
		txn.oncomplete = () => resolve()
		txn.onerror = () => reject(txn.error)
	})
}

// The olm/megolm state in the database is pickled with a key. Use a random
// per-installation key stored in IndexedDB instead of a constant. Databases
// created before this existed were pickled with "meow"; keep using that for
// them, since mautrix has no way to re-pickle.
async function loadPickleKey(): Promise<Uint8Array> {
	const db = await openKV()
	try {
		const existing = await kvGet(db, PICKLE_KEY)
		if (existing instanceof Uint8Array && existing.length > 0) {
			return existing
		}
		const legacyDB = self.sqlite3.PoolUtil?.getFileNames().includes("/gomuks.db")
		const key = legacyDB ? new TextEncoder().encode("meow") : crypto.getRandomValues(new Uint8Array(32))
		await kvPut(db, PICKLE_KEY, key)
		console.info(legacyDB ? "Using legacy pickle key for existing database" : "Generated new pickle key")
		return key
	} finally {
		db.close()
	}
}

;(async () => {
	const go = new Go()
	await initSqlite()
	self.wasmuksPickleKey = await loadPickleKey()
	const instance = await initGomuksWasm(go.importObject)
	await setupMediaChannel()
	await go.run(instance)
	self.postMessage({
		command: "wasm-connection",
		data: {
			connected: false,
			reconnecting: false,
			error: `Go process exited`,
		},
	})
})().catch(err => {
	console.error("Fatal error in wasm worker:", err)
	self.postMessage({
		command: "wasm-connection",
		data: {
			connected: false,
			reconnecting: false,
			error: `${err}`,
		},
	})
})
