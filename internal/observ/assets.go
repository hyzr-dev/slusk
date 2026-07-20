// Package observ: assets.go embeds and serves the built single-page app.
// Vite writes its output to web/dist; go:embed cannot read outside this
// package's directory, which is why the build output lands here rather than
// next to the frontend sources in web/.
package observ

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:web/dist
var distFS embed.FS

// newAssetHandler serves the SPA: hashed assets are cached forever, every
// unknown path returns index.html so client-side routes survive a reload, and
// /api paths are never swallowed — a mistyped API path must 404, not return
// HTML that a fetch() would then fail to parse.
func newAssetHandler() http.Handler {
	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		panic("observ: dist subtree missing: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")

		if strings.HasPrefix(path, "api/") {
			http.NotFound(w, r)
			return
		}

		// Hashed bundles: serve directly, cache forever, 404 if absent.
		if strings.HasPrefix(path, "assets/") {
			if _, err := fs.Stat(sub, path); err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			files.ServeHTTP(w, r)
			return
		}

		// Any other real file (favicon and friends) is served as-is.
		if path != "" {
			if _, err := fs.Stat(sub, path); err == nil {
				files.ServeHTTP(w, r)
				return
			}
		}

		serveIndex(w, sub)
	})
}

func serveIndex(w http.ResponseWriter, sub fs.FS) {
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
