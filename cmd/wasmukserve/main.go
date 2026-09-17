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
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

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
	// The build pre-compresses the large files; serve the sidecar when the
	// client accepts it instead of compressing 30 MB of wasm per request.
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		if gzFile, gzInfo, gzErr := s.open(name + ".gz"); gzErr == nil {
			defer func() {
				_ = gzFile.Close()
			}()
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Add("Vary", "Accept-Encoding")
			serveFile(w, r, name, gzInfo.ModTime(), gzFile)
			return
		}
	}
	serveFile(w, r, name, info.ModTime(), file)
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
	srv := &http.Server{
		Addr:              *listen,
		Handler:           &server{files: files, configPath: *configPath},
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("Serving gomuks web (wasm) on http://%s", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(fmt.Errorf("server closed with error: %w", err))
	}
}
