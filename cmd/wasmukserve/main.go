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

// wasmukserve serves gomuks web with the embedded wasm backend as a static
// site. The frontend is embedded in the binary (like the native gomuks
// server does), so the binary is all that's needed. See docs/wasmuks.md.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"go.mau.fi/gomuks/version"
	"go.mau.fi/gomuks/web"
)

func envOr(key, def string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return def
}

type server struct {
	files      fs.FS
	configPath string
	// index.html with the version meta tag filled in, see loadIndex. Nil if
	// the placeholder wasn't found, in which case the file is served as-is.
	index []byte
}

// The server build injects three meta tags here; only the version one applies
// to a static deployment. The frontend decides it is running the wasm backend
// by the ABSENCE of gomuks-frontend-etag, so that one must not be added, and
// there is no push key to advertise.
const versionMetaTemplate = "\t<meta name=\"gomuks-version-description\" content=\"%s\">"

// loadIndex fills in the version so the settings screen can show it, the same
// as pkg/gomuks does when it starts its HTTP server.
//
// It deliberately does not preload the wasm module, which looks like an easy
// win: the worker that fetches it is only created once the frontend has taken
// a Web Lock, read its configuration and restored the room list, so the
// largest download of the load starts last. Both <link rel="preload"
// as="fetch"> and <link rel="prefetch"> make it worse. On a fast connection
// the file is fetched twice, and on a throttled one the worker's
// instantiateStreaming attaches to the response the hint already downloaded
// and fails with "WebAssembly compilation aborted: Response body loading was
// aborted", so the backend never starts. Starting the download earlier means
// compiling it on the main thread and passing the module to the worker, so
// there is only ever one consumer of the stream.
func (s *server) loadIndex() {
	data, err := fs.ReadFile(s.files, "index.html")
	if err != nil {
		return
	}
	placeholder := []byte("<!-- etag placeholder -->")
	if !bytes.Contains(data, placeholder) {
		return
	}
	s.index = bytes.Replace(data, placeholder, []byte(fmt.Sprintf(
		versionMetaTemplate, html.EscapeString(version.Gomuks.VersionDescription),
	)), 1)
}

// noCacheFiles are entry points that must always be re-validated so a new
// deployment takes effect on the next load. Everything under assets/ is
// content-hashed and cached forever.
var noCacheFiles = map[string]bool{
	"index.html":          true,
	"config.json":         true,
	"manifest.json":       true,
	"wasmuks-media-sw.js": true,
	"pushmuks-sw.js":      true,
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if name == "config.json" {
		s.serveConfig(w, r)
		return
	}
	file, info, err := s.open(name)
	if errors.Is(err, fs.ErrNotExist) && path.Ext(name) == "" {
		// Client-side routes: serve the app shell.
		name = "index.html"
		file, info, err = s.open(name)
	}
	if err == nil && name == "index.html" && s.index != nil {
		_ = file.Close()
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(s.index))
		return
	}
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer func() {
		_ = file.Close()
	}()
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if noCacheFiles[name] {
		w.Header().Set("Cache-Control", "no-cache")
	} else if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if name == "wasmuks-media-sw.js" {
		w.Header().Set("Service-Worker-Allowed", "/")
	}
	w.Header().Set("Content-Type", contentType)
	// The build pre-compresses the large files; serve a sidecar when the
	// client accepts it instead of compressing 30 MB of wasm per request.
	// Brotli is tried first because it is about 30% smaller than gzip on the
	// wasm binary, which is the whole download that matters here.
	accept := r.Header.Get("Accept-Encoding")
	for _, enc := range precompressed {
		if !acceptsEncoding(accept, enc.name) {
			continue
		}
		encFile, encInfo, encErr := s.open(name + enc.ext)
		if encErr != nil {
			continue
		}
		defer func() {
			_ = encFile.Close()
		}()
		w.Header().Set("Content-Encoding", enc.name)
		w.Header().Add("Vary", "Accept-Encoding")
		serveFile(w, r, name, encInfo.ModTime(), encFile)
		return
	}
	serveFile(w, r, name, info.ModTime(), file)
}

// precompressed lists the sidecar files the build produces, best first.
var precompressed = []struct{ name, ext string }{
	{"br", ".br"},
	{"gzip", ".gz"},
}

// acceptsEncoding reports whether the client listed the encoding in
// Accept-Encoding. A plain substring check would be enough in practice, but
// the header is a token list and matching it as one is barely more work.
func acceptsEncoding(header, encoding string) bool {
	for part := range strings.SplitSeq(header, ",") {
		name, _, _ := strings.Cut(part, ";")
		if strings.EqualFold(strings.TrimSpace(name), encoding) {
			return true
		}
	}
	return false
}

func (s *server) open(name string) (fs.File, fs.FileInfo, error) {
	file, err := s.files.Open(name)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		_ = file.Close()
		if err == nil {
			err = fs.ErrNotExist
		}
		return nil, nil, err
	}
	return file, info, nil
}

func serveFile(w http.ResponseWriter, r *http.Request, name string, modTime time.Time, file fs.File) {
	if seeker, ok := file.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, modTime, seeker)
		return
	}
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, name, modTime, strings.NewReader(string(data)))
}

// serveConfig serves the optional config.json from disk (never embedded, so
// it can be changed without rebuilding).
func (s *server) serveConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	if s.configPath == "" {
		http.Error(w, "no config", http.StatusNotFound)
		return
	}
	info, err := os.Stat(s.configPath)
	if err != nil || info.IsDir() {
		http.Error(w, "no config", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	http.ServeFile(w, r, s.configPath)
}

func main() {
	listen := flag.String("listen", envOr("WASMUKS_LISTEN", "127.0.0.1:8181"), "address to listen on")
	dir := flag.String("dir", envOr("WASMUKS_DIR", ""), "serve this directory instead of the embedded frontend")
	configPath := flag.String("config", envOr("WASMUKS_CONFIG", ""), "path to config.json to serve (optional)")
	flag.Parse()

	var files fs.FS
	if *dir != "" {
		files = os.DirFS(*dir)
	} else {
		var err error
		files, err = fs.Sub(web.Frontend, "dist")
		if err != nil {
			log.Fatalf("failed to open embedded frontend: %v", err)
		}
		if _, err = fs.Stat(files, "index.html"); err != nil {
			log.Fatalf("no frontend embedded in this binary; build web/dist first or pass -dir")
		}
	}
	if *configPath != "" {
		if info, err := os.Stat(*configPath); err != nil {
			log.Printf("config %s not readable (%v); serving without config.json", *configPath, err)
		} else if info.IsDir() {
			log.Printf("config %s is a directory, not a file (Docker creates one when the bind mount source is missing); serving without config.json", *configPath)
		}
	}
	handler := &server{files: files, configPath: *configPath}
	handler.loadIndex()
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("Serving gomuks web (wasm) on http://%s", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(fmt.Errorf("server closed with error: %w", err))
	}
}
