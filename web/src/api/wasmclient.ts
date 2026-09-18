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
import { CachedEventDispatcher, NonNullCachedEventDispatcher } from "@/util/eventdispatcher.ts"
import RPCClient, { ConnectionEvent } from "./rpc.ts"
import type { BaseRPCCommand, MediaMessageEventContent, RPCCommand } from "./types"
import WasmuksWorker from "./wasm/wasmuks.ts?worker"

export interface StorageStatus {
	// null if the browser doesn't expose the storage API
	persisted: boolean | null
	usage?: number
	quota?: number
}

// Parameters passed to the worker via its name, read by cmd/wasmuks before it
// starts syncing. Using the name instead of postMessage avoids a race with the
// Go side registering its message listener.
export interface WasmuksInit {
	last_server_ts: number
	// From the "wasm" section of config.json, see docs/wasmuks.md.
	single_connection?: boolean
	memory_limit_mb?: number
	gc_ballast_mb?: number
	// zerolog level name: trace, debug, info, warn, error. Default debug.
	log_level?: string
}

// Reads the "wasm" section of config.json next to index.html. Preferences in
// the same file are handled by the state store; this only picks up the
// backend tuning knobs, validated by type.
async function loadWasmConfig(): Promise<Partial<WasmuksInit>> {
	try {
		const resp = await fetch("config.json", { cache: "no-cache" })
		if (!resp.ok) {
			return {}
		}
		const config = await resp.json() as { wasm?: Record<string, unknown> }
		const wasm = config?.wasm
		if (!wasm || typeof wasm !== "object") {
			return {}
		}
		const out: Partial<WasmuksInit> = {}
		if (typeof wasm.single_connection === "boolean") {
			out.single_connection = wasm.single_connection
		}
		if (typeof wasm.memory_limit_mb === "number" && wasm.memory_limit_mb > 0) {
			out.memory_limit_mb = Math.floor(wasm.memory_limit_mb)
		}
		if (typeof wasm.gc_ballast_mb === "number" && wasm.gc_ballast_mb >= 0) {
			out.gc_ballast_mb = Math.floor(wasm.gc_ballast_mb)
		}
		if (wasm.initial_timeline_limit !== undefined) {
			// Removed: the sync filter it fed is used for every sync, not just
			// the first, so a small window left busy rooms with no message to
			// sort or preview by, and any limited sync trimmed the stored
			// timeline down to it.
			console.warn("Ignoring initial_timeline_limit from config.json, it is no longer configurable")
		}
		const logLevels = ["trace", "debug", "info", "warn", "error"]
		if (typeof wasm.log_level === "string" && logLevels.includes(wasm.log_level)) {
			out.log_level = wasm.log_level
		}
		if (Object.keys(out).length) {
			console.info("Loaded wasm config:", out)
		}
		return out
	} catch (err) {
		console.warn("Failed to load config.json wasm section", err)
		return {}
	}
}

const LOCK_NAME = "gomuks-wasm"

interface WasmConnectionCommand extends BaseRPCCommand<ConnectionEvent> {
	command: "wasm-connection"
}

interface RawJSONCommand extends BaseRPCCommand<string> {
	command: RPCCommand["command"]
}

export default class WasmClient extends RPCClient {
	public readonly rpcMediaUpload = true
	public readonly rpcKeyRestore = true
	public readonly storageStatus = new CachedEventDispatcher<StorageStatus>()
	protected isConnected = true
	#worker?: Worker
	#releaseLock?: () => void
	// Commands sent before the worker has started (e.g. triggered by the
	// room list restored from IndexedDB) wait here until Go reports ready.
	#ready = false
	#pending: object[] = []
	// In the wasm build the backend lives in the tab, so backgrounding the
	// PWA suspends the /sync long poll and it fails once on resume. The UI
	// uses the resume time to give the sync a grace period before saying
	// anything about it (see WasmSyncBar).
	readonly lastResumedAt = new NonNullCachedEventDispatcher<number>(Date.now())

	async start() {
		// The OPFS SAH pool gives exclusive file handles to one worker, so a
		// second tab would fail inside SQLite with an unhelpful error. Take a
		// Web Lock before creating the worker (which installs the pool
		// immediately) and explain the situation instead.
		// The database lives in the origin private file system. Firefox private
		// windows (and some restrictive browser settings) don't provide it, and
		// SQLite's error for that is unhelpful, so check up front.
		try {
			await navigator.storage.getDirectory()
		} catch (err) {
			console.error("Origin private file system unavailable", err)
			this.connect.emit({
				connected: false,
				reconnecting: false,
				error: "This browser window doesn't allow persistent storage (private browsing?)."
					+ " gomuks keeps its database in the browser and needs it: open this page in a normal window.",
			})
			return
		}
		if (!await this.#acquireLock()) {
			this.connect.emit({
				connected: false,
				reconnecting: false,
				error: "gomuks is already open in another tab. Close it, then reload this page.",
			})
			return
		}
		const init: WasmuksInit = {
			...await loadWasmConfig(),
			last_server_ts: this.getCachedServerTimestamp?.() ?? 0,
		}
		this.#worker = new WasmuksWorker({ name: JSON.stringify(init) })
		this.#worker.addEventListener("message", this.#onMessage)
		document.addEventListener("visibilitychange", this.#onVisibilityChange)
		this.#checkStorage().catch(err => console.warn("Failed to check storage status", err))
		navigator.serviceWorker.register("wasmuks-media-sw.js").then(reg => {
			console.info("Media service worker registered", reg)
		}).catch(err => console.error("Failed to register media service worker", err))
	}

	#acquireLock(): Promise<boolean> {
		if (!navigator.locks) {
			return Promise.resolve(true)
		}
		return new Promise(resolve => {
			navigator.locks.request(LOCK_NAME, { ifAvailable: true }, lock => {
				if (!lock) {
					resolve(false)
					return
				}
				resolve(true)
				// Hold the lock until stop() releases it.
				return new Promise<void>(release => {
					this.#releaseLock = release
				})
			}).catch(err => {
				console.warn("Web Locks request failed, continuing without lock", err)
				resolve(true)
			})
		})
	}

	async #checkStorage() {
		await this.requestPersistentStorage()
	}

	// Asks the browser to make this origin's storage persistent. Firefox shows
	// a prompt; Chrome decides silently based on engagement, installation,
	// bookmarks and notification permission, so this is worth repeating after
	// those change. Safari only grants it to home screen apps.
	async requestPersistentStorage(): Promise<boolean | null> {
		if (!navigator.storage?.persist) {
			this.storageStatus.emit({ persisted: null })
			return null
		}
		if (this.storageStatus.current?.persisted === true) {
			return true
		}
		const persisted = await navigator.storage.persist()
		const estimate = await navigator.storage.estimate()
		console.info("Storage persistence:", persisted, "usage:", estimate.usage, "quota:", estimate.quota)
		this.storageStatus.emit({ persisted, usage: estimate.usage, quota: estimate.quota })
		return persisted
	}

	async doAuth(): Promise<void> {}

	async uploadMedia(file: Blob, filename: string, encrypt: boolean): Promise<MediaMessageEventContent> {
		const request_id = this.nextRequestID
		const payload = await file.bytes()
		return new Promise((resolve, reject) => {
			if (!this.#worker) {
				reject(new Error("Worker not initialized"))
				return
			}
			this.pendingRequests.set(request_id, { resolve: resolve as ((value: unknown) => void), reject })
			this.#worker.postMessage({
				command: "wasm-upload",
				request_id,
				data: "",
				encrypt,
				filename,
				payload,
			}, [payload.buffer])
		})
	}

	#onVisibilityChange = () => {
		if (document.visibilityState === "visible") {
			this.lastResumedAt.emit(Date.now())
		}
	}

	#onMessage = (evt: MessageEvent<RawJSONCommand | WasmConnectionCommand>) => {
		let realEvtData: RPCCommand | WasmConnectionCommand
		if (typeof evt.data.data === "string") {
			realEvtData = {
				...evt.data,
				data: JSON.parse(evt.data.data),
			}
		} else if (evt.data.command === "wasm-connection") {
			realEvtData = evt.data
		} else {
			console.error("Unexpected message data:", evt.data)
			return
		}
		// console.debug("[RPC] Go -> JS", realEvtData)
		if (realEvtData.command === "wasm-connection") {
			this.#ready = realEvtData.data.connected
			if (this.#ready) {
				const queued = this.#pending
				this.#pending = []
				for (const payload of queued) {
					this.#worker?.postMessage(payload)
				}
			}
			this.connect.emit(realEvtData.data)
		} else {
			this.onCommand(realEvtData)
		}
	}

	async stop() {
		document.removeEventListener("visibilitychange", this.#onVisibilityChange)
		this.#worker?.terminate()
		this.#worker = undefined
		this.#ready = false
		this.#pending = []
		this.#releaseLock?.()
		this.#releaseLock = undefined
	}

	protected send(data: RPCCommand) {
		const payload = {
			command: data.command ?? "",
			request_id: data.request_id ?? 0,
			data: JSON.stringify(data.data ?? {}),
		}
		if (!this.#worker || !this.#ready) {
			this.#pending.push(payload)
			return
		}
		// console.debug("[RPC] JS -> Go", payload)
		this.#worker.postMessage(payload)
	}
}
