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
import { useState } from "react"
import type WasmClient from "@/api/wasmclient.ts"
import { useEventAsState } from "@/util/eventdispatcher.ts"
import { isPWA } from "@/util/ismobile.ts"

const DISMISSED_KEY = "wasm_storage_warning_dismissed"

function wasDismissed(): boolean {
	try {
		return localStorage.getItem(DISMISSED_KEY) === "true"
	} catch {
		return false
	}
}

// Shown in the wasm build when the browser refused to make storage
// persistent, in which case it may evict the OPFS database (Safari does so
// after a week without use unless the page is installed to the home screen).
const StorageWarning = ({ rpc }: { rpc: WasmClient }) => {
	const status = useEventAsState(rpc.storageStatus)
	const [dismissed, setDismissed] = useState(wasDismissed)
	if (dismissed || !status || status.persisted !== false || isPWA) {
		return null
	}
	const dismiss = () => {
		try {
			localStorage.setItem(DISMISSED_KEY, "true")
		} catch {
			// ignore
		}
		setDismissed(true)
	}
	return <div className="storage-warning">
		<span>
			The browser hasn't granted persistent storage, so it may delete your local
			message database and encryption keys if this site isn't used for a while.
			Adding gomuks to your home screen or bookmarks usually prevents that.
		</span>
		<button onClick={dismiss}>Dismiss</button>
	</div>
}

export default StorageWarning
