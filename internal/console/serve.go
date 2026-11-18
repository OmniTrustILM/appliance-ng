// Package console serves the web console of the Appliance Manager.
package console

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

const index = "index.html"

// Handler serves the console files of fsys. A path that names no file gets index.html, so the console can route
// on the client; paths under /api/ are left to the API and answered with 404 here.
func Handler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")

		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if info, err := fs.Stat(fsys, name); err == nil && info.Mode().IsRegular() && name != index {
			http.ServeFileFS(w, r, fsys, name)
			return
		}
		serveIndex(w, r, fsys)
	})
}

// serveIndex writes index.html itself: the file server would redirect a request for it to the directory.
func serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, index)
	if err != nil {
		http.Error(w, "console not available", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, index, time.Time{}, bytes.NewReader(data))
}
