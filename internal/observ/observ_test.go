package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestStatusEndpointReturnsReport(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) {
		return StatusReport{Queued: 3, Active: 1, Stalled: 0, Orphaned: 2}, nil
	}
	h := NewServer(reg, status)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	var got StatusReport
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Queued != 3 || got.Orphaned != 2 {
		t.Errorf("unexpected report: %+v", got)
	}
}

func TestMetricsEndpointServes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.ReconcileTotal.Inc()
	h := NewServer(reg, func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil })

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
}
