package observ

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRootServesDashboardHTML(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`id="view-overview"`, `id="view-queue"`, "/dashboard.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestDashboardJSServed(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/dashboard.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want javascript", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data.moduleDetails") || !strings.Contains(body, "status.live") {
		t.Error("dashboard must use server-provided module liveness details")
	}
	if strings.Contains(body, "MODULE_STALE_AFTER_MS") || strings.Contains(body, "60000") {
		t.Error("dashboard must not use a hard-coded module staleness window")
	}
}
