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
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func serve(t *testing.T, files fstest.MapFS, target string, header http.Header) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for key, values := range header {
		req.Header[key] = values
	}
	rec := httptest.NewRecorder()
	(&server{files: files}).ServeHTTP(rec, req)
	return rec.Result()
}

// The browser only reuses compiled WebAssembly across page loads when the
// module was instantiated from a stream, and the frontend only takes that
// path when the response says application/wasm. Serving it as a generic
// binary still works, but silently costs every visitor the compile time of a
// 30 MB module on every load, so it is worth a test.
func TestWasmContentType(t *testing.T) {
	files := fstest.MapFS{
		"index.html":                 {Data: []byte("<html></html>")},
		"assets/_gomuks-abc.wasm":    {Data: []byte("\x00asm")},
		"assets/_gomuks-abc.wasm.gz": {Data: []byte("gzipped")},
	}
	for _, accept := range []string{"", "gzip"} {
		resp := serve(t, files, "/assets/_gomuks-abc.wasm", http.Header{"Accept-Encoding": {accept}})
		if got := resp.Header.Get("Content-Type"); got != "application/wasm" {
			t.Errorf("Accept-Encoding %q: Content-Type is %q, want application/wasm", accept, got)
		}
	}
}

func TestCacheHeaders(t *testing.T) {
	files := fstest.MapFS{
		"index.html":          {Data: []byte("<html></html>")},
		"assets/index-abc.js": {Data: []byte("console.log(1)")},
	}
	for target, want := range map[string]string{
		"/":                    "no-cache",
		"/index.html":          "no-cache",
		"/assets/index-abc.js": "public, max-age=31536000, immutable",
	} {
		resp := serve(t, files, target, nil)
		if got := resp.Header.Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control is %q, want %q", target, got, want)
		}
	}
}

// Anything that isn't a file is the single-page app's own routing.
func TestSPAFallback(t *testing.T) {
	files := fstest.MapFS{"index.html": {Data: []byte("<html></html>")}}
	resp := serve(t, files, "/some/room", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status is %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type is %q, want html", got)
	}
}
