package api

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// StaticFS is the built frontend, injected by the binary at startup.
//
// It is a variable rather than a compile-time embed so the API still builds and
// runs without a frontend build present: local development serves the React app
// from Vite on its own port, and only the container image bundles web/dist.
var StaticFS fs.FS

// staticHandler serves the built single-page app.
//
// Two things matter here. Hashed asset filenames are immutable, so they get a
// long cache lifetime, while index.html must never be cached or a deploy would
// keep serving the old asset references. And any unknown path falls back to
// index.html rather than 404ing, because client-side routing means the server
// has no list of valid routes.
func staticHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")

		// Serve index.html ourselves rather than through http.FileServer, which
		// 301-redirects "/index.html" to "./" and would loop forever once the
		// path has been rewritten.
		if upath == "" || upath == "index.html" {
			serveIndex(w, r, fsys)
			return
		}

		f, err := fsys.Open(upath)
		if err != nil {
			if !os.IsNotExist(err) {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			// Unknown path: hand the SPA its entry point and let the router
			// decide. A missing asset is a real 404 though -- falling back to
			// HTML for a .js request would produce a confusing MIME error in the
			// browser rather than an honest failure.
			if strings.Contains(path.Base(upath), ".") {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, r, fsys)
			return
		}
		defer f.Close()

		if st, err := f.Stat(); err == nil && st.IsDir() {
			serveIndex(w, r, fsys)
			return
		}

		if strings.HasPrefix(upath, "assets/") {
			// Vite fingerprints these, so the content can never change under a
			// given name.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}

		r.URL.Path = "/" + upath
		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	f, err := fsys.Open("index.html")
	if err != nil {
		http.Error(w, "frontend not built", http.StatusNotFound)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		http.Error(w, "frontend not readable", http.StatusInternalServerError)
		return
	}
	rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		http.Error(w, "frontend not seekable", http.StatusInternalServerError)
		return
	}

	// index.html carries the hashed asset references, so a stale copy would
	// point at files that no longer exist after a deploy.
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", st.ModTime(), rs)
}
