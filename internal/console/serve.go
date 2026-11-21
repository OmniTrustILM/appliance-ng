// Package console serves the web console of the Appliance Manager.
package console

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
)

const index = "index.html"

// Mount serves the console files of fsys under auth.BasePath on mux and sends a request for any path outside it,
// such as a bookmark from before the console moved there, to the same path under it.
func Mount(mux *http.ServeMux, fsys fs.FS) {
	mux.Handle(auth.BasePath+"/", http.StripPrefix(auth.BasePath, Handler(fsys)))
	mux.Handle("/", toBase())
}

// toBase answers 308, which keeps the method and body, to the request's path under auth.BasePath with its query.
// The target is built from the path alone and always starts with auth.BasePath, so it never leaves the site.
func toBase() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := auth.BasePath + "/" + strings.TrimLeft(r.URL.EscapedPath(), "/")
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
}

// Handler serves the console files of fsys. A path that names no file gets index.html, so the console can route
// on the client; paths under /api/ are left to the API and answered with 404 here. Paths are relative to the
// console's base.
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
