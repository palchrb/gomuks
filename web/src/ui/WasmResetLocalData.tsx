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
import type Client from "@/api/client.ts"
import WasmClient from "@/api/wasmclient.ts"

// SQLite's wording for a database file it can't read: SQLITE_CORRUPT and
// SQLITE_NOTADB. Nothing but deleting the file gets past those, and on a
// phone there is no console to do it from.
const unreadableDatabase = /database disk image is malformed|file is not a database|error 11:|error 26:/

interface WasmResetLocalDataProps {
	client: Client
	error: string
}

const WasmResetLocalData = ({ client, error }: WasmResetLocalDataProps) => {
	const [state, setState] = useState<"idle" | "working" | string>("idle")
	if (!(client.rpc instanceof WasmClient) || !unreadableDatabase.test(error)) {
		return null
	}
	const rpc = client.rpc
	const reset = () => {
		if (!window.confirm(
			"The local database can't be read and has to be deleted. Messages come back from the server,"
			+ " but you have to log in and verify this session again. Continue?",
		)) {
			return
		}
		setState("working")
		rpc.resetLocalData().then(async () => {
			localStorage.clear()
			await client.store.deleteCache().catch(err => console.warn("Failed to delete state cache", err))
			window.location.reload()
		}, err => {
			console.error("Failed to delete local data", err)
			setState(`Failed to delete local data: ${err}`)
		})
	}
	return <>
		<button onClick={reset} disabled={state === "working"}>
			{state === "working" ? "Deleting local data..." : "Delete local data and log in again"}
		</button>
		{state !== "idle" && state !== "working" && <div>{state}</div>}
	</>
}

export default WasmResetLocalData
