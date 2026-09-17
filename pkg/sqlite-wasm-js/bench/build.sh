#!/bin/sh
# Builds the benchmark wasm binary and copies Go's wasm_exec.js next to it.
set -e
cd "$(dirname "$0")"
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" .
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -o bench.wasm .
