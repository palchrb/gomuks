// gomuks - A Matrix client written in Go.
// Copyright (C) 2024 Tulir Asokan
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
import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import App from "./App.tsx"
import { watchSoftKeyboard } from "./util/softkeyboard.ts"
import "./index.css"

// Views like the image pack editor are loaded on demand, from files whose
// names contain a build hash. Deploying a new build removes the old files, so
// a page that was already open when that happened asks for something that is
// gone and the view fails to render. Reload instead, which picks up the new
// build. The timestamp guard keeps a chunk that is missing for some other
// reason from turning into a reload loop.
const RELOAD_KEY = "gomuks_stale_frontend_reload"
const RELOAD_COOLDOWN_MS = 60_000
window.addEventListener("vite:preloadError", evt => {
	let lastReload = 0
	try {
		lastReload = Number(sessionStorage.getItem(RELOAD_KEY)) || 0
	} catch {
		// Storage can be unavailable; reloading once is still better than failing.
	}
	if (Date.now() - lastReload < RELOAD_COOLDOWN_MS) {
		console.error("Failed to load part of the app again, not reloading", evt.payload)
		return
	}
	try {
		sessionStorage.setItem(RELOAD_KEY, Date.now().toString())
	} catch {
		// Ignore, see above.
	}
	console.warn("Failed to load part of the app, probably an old build, reloading", evt.payload)
	evt.preventDefault()
	window.location.reload()
})

watchSoftKeyboard()

createRoot(document.getElementById("root")!).render(
	<StrictMode>
		<App/>
	</StrictMode>,
)
