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

// newAssetHandler serves the SPA under four rules, in order:
//
//  1. /api/*        → 404, never the SPA fallback. A mistyped API path must
//     fail loudly, not return HTML that a fetch() would then fail to parse.
//  2. /assets/*      → Vite's content-hashed bundles, cached forever
//     (immutable — the hash in the filename changes whenever the content
//     does, so there is nothing to invalidate).
//  3. any other real file in the dist root → served as-is with
//     Cache-Control: no-cache. These come from Vite's public/ directory
//     (favicon.ico, robots.txt, ...) and are NOT content-hashed: their
//     contents can change while the filename stays the same, so caching
//     them immutably would serve stale files forever. Without this rule the
//     request would fall through to rule 4 and get back index.html, which
//     only shows up as a broken favicon/tab icon.
//  4. everything else → index.html (falling back to placeholder.html if the
//     frontend hasn't been built), so client-side routes survive a reload.
func newAssetHandler() http.Handler {
	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		panic("observ: dist subtree missing: " + err.Error())
	}
	return newAssetHandlerFS(sub)
}

// newAssetHandlerFS builds the handler over an arbitrary fs.FS, so tests can
// exercise all four rules against a fixture without needing a real Vite
// build in the embedded dist directory.
func newAssetHandlerFS(sub fs.FS) http.Handler {
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

		// Unhashed public/ assets (favicon and friends): serve as-is, but
		// never cache immutably since the filename won't change when the
		// content does.
		if path != "" {
			if _, err := fs.Stat(sub, path); err == nil {
				w.Header().Set("Cache-Control", "no-cache")
				files.ServeHTTP(w, r)
				return
			}
		}

		serveIndex(w, sub)
	})
}

// serveIndex returns the SPA shell. If the frontend hasn't been built,
// index.html won't exist in a fresh clone — fall back to the tracked
// placeholder.html so the response is a clear "run make ui" page instead of
// a 500.
func serveIndex(w http.ResponseWriter, sub fs.FS) {
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		b, err = fs.ReadFile(sub, "placeholder.html")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
