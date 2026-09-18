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
import { JSX, use, useEffect, useReducer } from "react"
import { SyncLoader } from "react-spinners"
import type { SyncStatus } from "@/api/types"
import type WasmClient from "@/api/wasmclient.ts"
import { useEventAsState } from "@/util/eventdispatcher.ts"
import ClientContext from "./ClientContext.ts"

// In the wasm build the backend runs in the tab and all data is local, so a
// failing sync never blocks anything: it only means new messages are late.
// Backgrounding the PWA also fails the in-flight /sync once on resume.
// So: say nothing for a moment, then a thin line at the bottom, and only
// after a while call it disconnected.
const GRACE_MS = 5_000
const DISCONNECTED_MS = 30_000

interface WasmSyncBarProps {
	rpc: WasmClient
	syncStatus: SyncStatus
}

const WasmSyncBar = ({ rpc, syncStatus }: WasmSyncBarProps): JSX.Element | null => {
	const client = use(ClientContext)!
	const lastResumedAt = useEventAsState(rpc.lastResumedAt)
	const roomList = useEventAsState(client.store.roomList)
	const [, tick] = useReducer((x: number) => x + 1, 0)
	const erroring = syncStatus.type === "erroring"
	useEffect(() => {
		if (!erroring) {
			return
		}
		const interval = setInterval(tick, 1000)
		return () => clearInterval(interval)
	}, [erroring])

	if (syncStatus.type === "ok") {
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
	if (elapsed < GRACE_MS) {
		return null
	} else if (elapsed < DISCONNECTED_MS) {
		return <div className="sync-bar reconnecting" title={syncStatus.error}>
			<SyncLoader size={6} color="var(--primary-color)"/>
			Reconnecting to server...
		</div>
	}
	return <div className="sync-bar disconnected" title={syncStatus.error}>
		<SyncLoader size={6} color="var(--error-color)"/>
		Disconnected, retrying...
	</div>
}

export default WasmSyncBar
