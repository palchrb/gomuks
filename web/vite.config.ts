import react from "@vitejs/plugin-react"
import { type Plugin, defineConfig } from "vite"
import svgr from "vite-plugin-svgr"
import elementCallPlugin from "./vite-element-call.ts"

const splitDeps = ["katex", "leaflet", "monaco-editor", "matrix-widget-api", "@dnd-kit"]

// The wasm build's service worker (public/wasmuks-sw.js) caches the hashed
// files under assets/ as they are fetched, so the app opens offline. This
// lists the current build's files, so it can drop the previous build's, and
// which of them every start needs ("core": what index.html loads, what those
// import statically, their fonts, the wasm worker and binaries) so a first
// visit can store those without waiting for a second load. The rest (katex,
// monaco, the map, image packs) is stored when first used.
const assetManifestPlugin: Plugin = {
	name: "wasmuks-asset-manifest",
	generateBundle(_, bundle) {
		const all = Object.keys(bundle).filter(name => name.startsWith("assets/")).sort()
		type Item = { type: string, name?: string, isEntry?: boolean, imports?: string[], source?: string | Uint8Array }
		const core = new Set<string>()
		const add = (name: string) => {
			const item = bundle[name] as Item | undefined
			if (!item || core.has(name)) {
				return
			}
			core.add(name)
			if (item.type === "chunk") {
				item.imports?.forEach(add)
				// index.html isn't in the bundle yet at this point, so the
				// stylesheet is found by name: a chunk's CSS is named after it.
				all.filter(css => css.startsWith(`assets/${item.name}-`) && css.endsWith(".css")).forEach(add)
			} else if (name.endsWith(".css") && typeof item.source === "string") {
				for (const [, font] of item.source.matchAll(/url\(\.\/([^)]+\.woff2?)\)/g)) {
					add(`assets/${font}`)
				}
			}
		}
		for (const [name, item] of Object.entries(bundle) as [string, Item][]) {
			if (item.type === "chunk" && item.isEntry) {
				add(name)
			}
		}
		for (const name of all) {
			if (name.endsWith(".wasm") || name.startsWith("assets/wasmuks-")) {
				add(name)
			}
		}
		this.emitFile({
			type: "asset",
			fileName: "wasmuks-assets.json",
			source: JSON.stringify({ core: [...core].sort(), all }),
		})
	},
}

export default defineConfig({
	base: "./",
	build: {
		target: ["esnext", "firefox140", "chrome140", "safari18"],
		chunkSizeWarningLimit: 4000,
		rollupOptions: {
			output: {
				manualChunks: id => {
					if (id.includes("node_modules") && !splitDeps.some(dep => id.includes(dep))) {
						return "vendor"
					} else if (id.endsWith("/emoji/data.json")) {
						return "emoji"
					}
				},
			},
		},
	},
	plugins: [
		react(),
		svgr({
			svgrOptions: {
				replaceAttrValues: {
					"#5f6368": "currentColor",
				},
			},
		}),
		elementCallPlugin,
		assetManifestPlugin,
	],
	resolve: {
		alias: {
			"@": "/src",
		},
	},
	server: {
		allowedHosts: true,
		proxy: {
			"/_gomuks/websocket": {
				target: "http://localhost:29325",
				ws: true,
			},
			"/_gomuks": {
				target: "http://localhost:29325",
			},
		},
	},
})
