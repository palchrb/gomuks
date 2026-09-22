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
import getConfigJSON from "./configjson.ts"
import RPCClient, { ConnectionEvent } from "./rpc.ts"
import type {
	BaseRPCCommand, MediaEncodingOptions, MediaMessageEventContent, RPCCommand,
} from "./types"
import { probeMedia } from "./wasm/probe.ts"
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
	memory_limit_mb?: number
	gc_ballast_mb?: number
	// zerolog level name: trace, debug, info, warn, error. Default debug.
	log_level?: string
}

// Reads the "wasm" section of config.json next to index.html. Preferences in
// the same file are handled by the state store; this only picks up the
// backend tuning knobs, validated by type.
async function loadWasmConfig(): Promise<Partial<WasmuksInit>> {
	const wasm = (await getConfigJSON()).wasm
	if (!wasm || typeof wasm !== "object") {
		return {}
	}
	const out: Partial<WasmuksInit> = {}
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
}

const LOCK_NAME = "gomuks-wasm"
// How often a resumed tab may check whether a new build has been deployed,
// and how long after a reload for that reason another one is refused.
const UPDATE_CHECK_INTERVAL_MS = 60_000
const UPDATE_RELOAD_COOLDOWN_MS = 60_000
const UPDATE_RELOAD_KEY = "gomuks_wasm_update_reload"
// A tab hidden for at least this long gets its sync restarted on resume. The
// threshold only exists so that flipping between tabs on a desktop doesn't
// restart the sync every time; a restart costs one aborted long poll, and iOS
// freezes a backgrounded app within seconds, so there is no reason to wait.
const RESUME_RESTART_MS = 5_000
// How long the worker has to answer the ping sent on resume before the page
// gives up on it. Generous on purpose: Go on wasm has no time-based
// preemption, so a worker in the middle of a large catch-up sync can't answer
// until that work yields, and a false positive costs a reload that throws the
// catch-up away. A healthy worker answers in milliseconds, so after this much
// silence the wait is shown instead of hidden.
const RESUME_PING_TIMEOUT_MS = 15_000
const RESUME_PING_SHOW_MS = 2_000

interface WasmConnectionCommand extends BaseRPCCommand<ConnectionEvent> {
	command: "wasm-connection"
}

interface RawJSONCommand extends BaseRPCCommand<string> {
	command: RPCCommand["command"]
}

export default class WasmClient extends RPCClient {
	public readonly rpcMediaUpload = true
	public readonly rpcKeyRestore = true
	public readonly rpcServerCommands = true
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
	// True while the worker has been silent for a while after a resume ping,
	// so the sync bar can say so instead of the screen looking fine.
	readonly workerUnresponsive = new NonNullCachedEventDispatcher<boolean>(false)
	#hiddenAt = 0
	// index.html as it was when this page loaded, used to notice new builds.
	#loadedIndexHTML?: string
	#lastUpdateCheck = 0
	#updateCheckDisabled = false

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
		this.#checkForUpdate(true).catch(err => console.warn("Failed to record frontend version", err))
		this.#checkStorage().catch(err => console.warn("Failed to check storage status", err))
		navigator.serviceWorker.register("wasmuks-sw.js").then(reg => {
			console.info("Service worker registered", reg)
		}).catch(err => console.error("Failed to register service worker", err))
		navigator.serviceWorker.addEventListener("message", evt => {
			if (evt.data?.type === "precached") {
				console.info("Service worker precached the app shell:", evt.data)
			}
		})
	}

	// Asks the service worker to store the files a start needs, so the app
	// opens offline from the next start on. Sent once the backend is up: by
	// then the files are downloaded, so this costs no bandwidth.
	#precacheShell() {
		navigator.serviceWorker.ready.then(reg => {
			reg.active?.postMessage({ type: "precache" })
		}).catch(err => console.warn("Service worker not ready for precaching", err))
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

	async uploadMedia(
		file: Blob, filename: string, encrypt: boolean, encodingOpts?: MediaEncodingOptions,
	): Promise<MediaMessageEventContent> {
		const request_id = this.nextRequestID
		// What the server build gets from ffmpeg, read from the browser
		// instead: duration and dimensions, a thumbnail frame for video, and
		// the waveform of a voice message.
		const probe = await probeMedia(file, !!encodingOpts?.voice_message)
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
				// The same parameters the server build takes in the query
				// string, so the upload dialog's options apply here too.
				// Keys starting with _ are for the frontend, as in the HTTP path.
				data: JSON.stringify({
					filename,
					encrypt,
					...Object.fromEntries(
						Object.entries(encodingOpts ?? {}).filter(([key]) => !key.startsWith("_")),
					),
					duration_ms: probe.duration_ms,
					width: probe.width,
					height: probe.height,
					waveform: probe.waveform,
				}),
				payload,
				thumbnail: probe.thumbnail,
			}, probe.thumbnail
				? [payload.buffer, probe.thumbnail.buffer]
				: [payload.buffer])
		})
	}

	#onVisibilityChange = () => {
		if (document.visibilityState !== "visible") {
			this.#hiddenAt = Date.now()
			return
		}
		this.lastResumedAt.emit(Date.now())
		this.#checkForUpdate(false).catch(err => console.warn("Failed to check for a new build", err))
		// Only once the worker has connected. Before that there is no sync to
		// restart, and a ping would sit in the queue while the module is still
		// downloading, which on a slow connection takes longer than the
		// watchdog allows: it would reload the page, start the download over,
		// and never get anywhere.
		if (this.#ready && this.#hiddenAt && Date.now() - this.#hiddenAt >= RESUME_RESTART_MS) {
			this.#checkAfterResume()
		}
	}

	// iOS suspends a backgrounded PWA and resumes it inconsistently. Sometimes
	// the /sync that was in flight comes back as a zombie that neither fails
	// nor completes for minutes; sometimes the worker is gone altogether.
	// Neither produces an error, so the timeline sits there looking fine and
	// nothing new arrives. Have the worker restart its sync, and check that
	// it can answer at all: if it can't, the only way back is a reload, which
	// the room list cache makes cheap.
	#checkAfterResume() {
		// Not queued like an RPC: a worker that isn't ready has no sync to restart.
		this.#worker?.postMessage({
			command: "wasm-resume",
			request_id: 0,
			data: JSON.stringify({ hidden_at: this.#hiddenAt }),
		})
		const showWaiting = setTimeout(() => this.workerUnresponsive.emit(true), RESUME_PING_SHOW_MS)
		const giveUp = setTimeout(() => {
			// Same cooldown as the update check, and shared with it: whatever
			// the reason, this page must never reload itself in a loop.
			let lastReload = 0
			try {
				lastReload = Number(localStorage.getItem(UPDATE_RELOAD_KEY)) || 0
			} catch {
				// Storage can be unavailable; reloading once is still right.
			}
			if (Date.now() - lastReload < UPDATE_RELOAD_COOLDOWN_MS) {
				console.error("Worker isn't answering after resuming, but the page was reloaded recently, leaving it")
				return
			}
			try {
				localStorage.setItem(UPDATE_RELOAD_KEY, Date.now().toString())
			} catch {
				// Ignore, see above.
			}
			console.error(`Worker didn't answer within ${RESUME_PING_TIMEOUT_MS} ms of resuming, reloading`)
			window.location.reload()
		}, RESUME_PING_TIMEOUT_MS)
		const answered = () => {
			clearTimeout(showWaiting)
			clearTimeout(giveUp)
			this.workerUnresponsive.emit(false)
		}
		this.request("ping", {}).then(answered, err => {
			answered()
			console.warn("Ping after resume failed", err)
		})
	}

	// The server build tells clients its frontend version over the connection
	// and they reload when it differs. Here the backend is the page itself, so
	// compare the served index.html against the one this page was loaded from:
	// it names the hashed entry point, so it changes with every build. Any
	// static server works, as long as index.html isn't cached for long, which
	// it can't be anyway or the browser would never see a new build either.
	async #checkForUpdate(initial: boolean) {
		if (this.#updateCheckDisabled) {
			return
		}
		const now = Date.now()
		if (!initial && (now - this.#lastUpdateCheck < UPDATE_CHECK_INTERVAL_MS || !this.#loadedIndexHTML)) {
			return
		}
		this.#lastUpdateCheck = now
		let current: string
		try {
			const resp = await fetch("index.html", { cache: "no-cache" })
			if (!resp.ok) {
				return
			}
			current = await resp.text()
		} catch (err) {
			// Offline, or something else is serving the page. Try again later.
			console.debug("Couldn't fetch index.html to check for a new build", err)
			return
		}
		if (initial) {
			this.#loadedIndexHTML = current
			return
		}
		if (current === this.#loadedIndexHTML) {
			return
		}
		// Reloading right after the page became visible is the least
		// disruptive moment, and the one where an old build is most likely.
		let lastReload = 0
		try {
			lastReload = Number(localStorage.getItem(UPDATE_RELOAD_KEY)) || 0
		} catch {
			// Storage can be unavailable; reloading once is still right.
		}
		if (now - lastReload < UPDATE_RELOAD_COOLDOWN_MS) {
			// index.html differs but reloading didn't help, so it probably
			// varies per request. Stop, rather than reload in a loop.
			console.warn("index.html keeps changing between requests, stopping update checks")
			this.#updateCheckDisabled = true
			return
		}
		try {
			localStorage.setItem(UPDATE_RELOAD_KEY, now.toString())
		} catch {
			// Ignore, see above.
		}
		console.info("A new build is available, reloading")
		window.location.reload()
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
				this.#precacheShell()
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

	// Deletes the local database and everything else the backend stored, for
	// when the database can't be opened any more (SQLite reports it as
	// malformed). The server has the messages, so this amounts to a fresh
	// login, and the key backup brings back the keys for old messages. Done
	// from the main thread after stopping the worker, so it also works when
	// the worker is wedged: terminating it releases its file handles.
	async resetLocalData(): Promise<void> {
		await this.stop()
		const root = await navigator.storage.getDirectory()
		for (const name of [".opfs-sahpool", "pickle.key"]) {
			for (let attempt = 0; ; attempt++) {
				try {
					await root.removeEntry(name, { recursive: true })
					break
				} catch (err) {
					if ((err as DOMException).name === "NotFoundError") {
						break
					} else if (attempt >= 10) {
						throw err
					}
					// The terminated worker's access handles close asynchronously.
					await new Promise(resolve => setTimeout(resolve, 200))
				}
			}
		}
		await caches.delete("wasmuks-media-v1").catch(() => {})
		console.info("Deleted local database")
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
