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
import { JSX, use, useEffect, useReducer, useState } from "react"
import { SyncLoader } from "react-spinners"
import type { SyncStatus } from "@/api/types"
import type WasmClient from "@/api/wasmclient.ts"
import { useEventAsState } from "@/util/eventdispatcher.ts"
import ClientContext from "./ClientContext.ts"

// In the wasm build the backend runs in the tab and all data is local, so a
// failing sync never blocks anything: it only means new messages are late.
// So: a thin line at the bottom, and nothing that moves unless something is
// actually being attempted.
//
// A failure that starts while the tab is already in front gets a quiet moment
// first, so a single hiccup mid-session doesn't flash a line. Right after the
// tab is brought forward is the opposite case: backgrounding the PWA fails the
// in-flight /sync, and that is exactly when saying "reconnecting" is useful,
// because the user is looking at the room list waiting for it to catch up.
const GRACE_MS = 5_000
const RESUME_MS = 15_000
// When the network is there but the server isn't answering, there is nothing to
// go on but the retries. hicli retries once a second and only starts backing
// off after five failures, so this is several seconds of trying rather than a
// hair trigger. The time limit covers a server that is slow instead of failing.
const DISCONNECTED_ERRORS = 5
const DISCONNECTED_MS = 30_000

// navigator.onLine is only trustworthy when it says false, which is the case
// worth having: the browser knows there is no network before any sync can
// fail, so there is no reason to wait for the retries to say it.
function useOnline(): boolean {
	const [online, setOnline] = useState(() => navigator.onLine)
	useEffect(() => {
		const update = () => setOnline(navigator.onLine)
		window.addEventListener("online", update)
		window.addEventListener("offline", update)
		update()
		return () => {
			window.removeEventListener("online", update)
			window.removeEventListener("offline", update)
		}
	}, [])
	return online
}

interface WasmSyncBarProps {
	rpc: WasmClient
	syncStatus: SyncStatus
}

const WasmSyncBar = ({ rpc, syncStatus }: WasmSyncBarProps): JSX.Element | null => {
	const client = use(ClientContext)!
	const lastResumedAt = useEventAsState(rpc.lastResumedAt)
	const workerUnresponsive = useEventAsState(rpc.workerUnresponsive)
	const roomList = useEventAsState(client.store.roomList)
	const online = useOnline()
	const [, tick] = useReducer((x: number) => x + 1, 0)
	const erroring = syncStatus.type === "erroring"
	useEffect(() => {
		if (!erroring) {
			return
		}
		const interval = setInterval(tick, 1000)
		return () => clearInterval(interval)
	}, [erroring])

	if (!online) {
		// A fact rather than a fault, so it gets neither a spinner nor the
		// error colour. Nothing else is worth saying until the network is back.
		return <div className="sync-bar offline">Offline</div>
	} else if (workerUnresponsive) {
		// The worker hasn't answered the ping sent when the tab resumed. It is
		// either busy with a large catch-up or gone, in which case the page
		// reloads when it gives up. Either way something is being attempted,
		// and the sync status alone would say everything is fine.
		return <div className="sync-bar reconnecting">
			<SyncLoader size={6} color="var(--primary-color)"/>
			Reconnecting to server...
		</div>
	} else if (syncStatus.type === "ok") {
		return null
	} else if (syncStatus.type === "waiting") {
		if (roomList.length === 0) {
			// Nothing local yet: same box as the native build.
			return <div className="sync-status waiting">
				<SyncLoader color="var(--primary-color)"/>
				Waiting for first sync...
			</div>
		}
		return <div className="sync-bar syncing">
			<SyncLoader size={6} color="var(--primary-color)"/>
			Syncing...
		</div>
	} else if (syncStatus.type === "permanently-failed") {
		return <div className="sync-bar disconnected" title={syncStatus.error}>
			Sync failed permanently
		</div>
	}
	const since = Math.max(syncStatus.last_sync ?? 0, lastResumedAt)
	const elapsed = Date.now() - since
	const justResumed = Date.now() - lastResumedAt < RESUME_MS
	const giveUp = syncStatus.error_count >= DISCONNECTED_ERRORS || elapsed >= DISCONNECTED_MS
	if (elapsed < GRACE_MS && !justResumed && !giveUp) {
		return null
	} else if (!giveUp) {
		// Something is being attempted, which is the one state that earns
		// movement on the screen.
		return <div className="sync-bar reconnecting" title={syncStatus.error}>
			<SyncLoader size={6} color="var(--primary-color)"/>
			Reconnecting to server...
		</div>
	}
	return <div className="sync-bar disconnected" title={syncStatus.error}>
		Disconnected, retrying...
	</div>
}

export default WasmSyncBar
