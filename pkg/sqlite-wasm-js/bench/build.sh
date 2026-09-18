#!/bin/sh
# Builds the benchmark: the wasm binary, Go's wasm_exec.js, and a bundled copy
# of the real sqlite bridge (web/src/api/wasm/sqlite_bridge.ts) so the bench
# always exercises the same JS code as gomuks web.
set -e
cd "$(dirname "$0")"
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" .
# The bridge lives in web/, so a bare import in it resolves against web's
# node_modules, which only exists if the frontend has been installed. Point it
# at the copy this benchmark installed instead, so building here needs nothing
# from web/.
node_modules/.bin/esbuild ../../../web/src/api/wasm/sqlite_bridge.ts \
	--bundle --format=esm --platform=browser --outfile=sqlite_bridge.js --log-level=warning \
	"--alias:@sqlite.org/sqlite-wasm=$(pwd)/node_modules/@sqlite.org/sqlite-wasm"
cp node_modules/@sqlite.org/sqlite-wasm/dist/sqlite3.wasm .
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -o bench.wasm .
