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

// config.json sits next to index.html and is optional: a static wasm
// deployment uses it for admin defaults, and the server build doesn't serve
// one at all. Two places read it, the preference defaults in the state store
// and the wasm backend's tuning knobs, and it is on the critical path before
// the worker starts, so the fetch is shared instead of done twice per load.
export interface ConfigJSON {
	preferences?: Record<string, unknown>
	wasm?: Record<string, unknown>
}

let cached: Promise<ConfigJSON> | undefined

// Never rejects: everything in the file is optional, so a missing or broken
// one means defaults rather than a failure.
export default function getConfigJSON(): Promise<ConfigJSON> {
	cached ??= fetch("config.json", { cache: "no-cache" })
		.then(resp => resp.ok ? resp.json() as Promise<ConfigJSON> : {})
		.catch(err => {
			console.warn("Failed to load config.json", err)
			return {}
		})
	return cached
}
