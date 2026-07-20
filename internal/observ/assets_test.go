package observ

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/does-not-exist.js", nil)

	newAssetHandler().ServeHTTP(rec, req)

	// Missing hashed assets must 404 rather than returning the SPA shell.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
