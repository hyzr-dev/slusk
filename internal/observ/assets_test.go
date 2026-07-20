package observ

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestAssetHandlerServesIndexAtRoot(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	newAssetHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
}

// Client-side routes must return the SPA shell so deep links and reloads work.
func TestAssetHandlerServesIndexForClientRoutes(t *testing.T) {
	for _, path := range []string{"/jobs", "/jobs/42", "/health", "/settings", "/peers"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)

		newAssetHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
	}
}

// A mistyped API path must 404, never fall through to HTML — an HTML body in
// response to a fetch() is a hostile thing to debug.
func TestAssetHandlerDoesNotSwallowAPIPaths(t *testing.T) {
	for _, path := range []string{"/api/nope", "/api/jobs/1/bogus"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)

		newAssetHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestAssetHandlerCachesHashedAssetsImmutably(t *testing.T) {
	fixture := fstest.MapFS{
		"assets/index-abc123.js": {Data: []byte("console.log('hi')")},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/index-abc123.js", nil)

	newAssetHandlerFS(fixture).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q, want public, max-age=31536000, immutable", cc)
	}

	// Missing hashed assets must 404 rather than returning the SPA shell.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/assets/does-not-exist.js", nil)

	newAssetHandlerFS(fixture).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// Unhashed public/ assets (rule 3) must be served as-is but never cached
// immutably, since their filename can stay the same while their content
// changes. dist/ is gitignored except for placeholder.html, so this is
// exercised against an in-memory fs.FS fixture rather than the embedded dist
// tree — a fresh clone has no favicon to test against.
func TestAssetHandlerServesRootFilesWithNoCache(t *testing.T) {
	fixture := fstest.MapFS{
		"index.html":  {Data: []byte("<html>shell</html>")},
		"favicon.ico": {Data: []byte("fake-icon-bytes")},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)

	newAssetHandlerFS(fixture).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if body := rec.Body.String(); body != "fake-icon-bytes" {
		t.Errorf("body = %q, want fake-icon-bytes", body)
	}
}

// When the frontend hasn't been built (no index.html), the SPA fallback
// must serve placeholder.html instead of 500ing.
func TestAssetHandlerFallsBackToPlaceholder(t *testing.T) {
	fixture := fstest.MapFS{
		"placeholder.html": {Data: []byte("<html>frontend not built</html>")},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	newAssetHandlerFS(fixture).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if body := rec.Body.String(); body != "<html>frontend not built</html>" {
		t.Errorf("body = %q, want placeholder content", body)
	}
}
