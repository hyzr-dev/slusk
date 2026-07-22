package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

func newChartsTestHandler(reg *prometheus.Registry, charts ChartsFunc) http.Handler {
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	return NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers,
		noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers,
		ConnectionTester{}, charts)
}

// TestChartsEndpointZeroFillsHoursAndOrdersPassesOldestFirst asserts the JSON
// shape: exactly 24 zero-filled hour buckets ending at "now" (oldest first),
// and passes reversed from the store's newest-first order into oldest-first.
func TestChartsEndpointZeroFillsHoursAndOrdersPassesOldestFirst(t *testing.T) {
	reg := prometheus.NewRegistry()
	now := time.Date(2026, 7, 22, 15, 30, 0, 0, time.UTC)
	newer := now.Add(-time.Minute)
	older := now.Add(-2 * time.Minute)
	// RecentSearchPasses returns newest first.
	passes := []core.SearchPass{
		{StartedAt: newer, FinishedAt: newer, Searched: 1, Matched: 1},
		{StartedAt: older, FinishedAt: older, Searched: 1, Matched: 0},
	}
	// Only one hour bucket has data - the rest must be zero-filled.
	sparseHour := now.Truncate(time.Hour).Add(-3 * time.Hour)
	counts := []core.HourCount{{Hour: sparseHour, Count: 7}}
	charts := func(ctx context.Context) (ChartsData, error) {
		return ChartsData{Passes: passes, CompletedByHour: counts}, nil
	}
	h := newChartsTestHandler(reg, charts)

	req := httptest.NewRequest(http.MethodGet, "/api/charts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got chartsDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.Passes) != 2 {
		t.Fatalf("expected 2 passes, got %d: %+v", len(got.Passes), got.Passes)
	}
	if got.Passes[0].Matched != 0 || got.Passes[1].Matched != 1 {
		t.Errorf("passes not reversed to oldest-first: %+v", got.Passes)
	}

	if len(got.CompletedByHour) != 24 {
		t.Fatalf("expected 24 zero-filled hour buckets, got %d", len(got.CompletedByHour))
	}
	var nonZero int
	for _, b := range got.CompletedByHour {
		if b.Count != 0 {
			nonZero++
			if b.Count != 7 {
				t.Errorf("unexpected non-zero bucket count %d at %s", b.Count, b.Hour)
			}
		}
	}
	if nonZero != 1 {
		t.Errorf("expected exactly 1 non-zero bucket, got %d", nonZero)
	}
	// Buckets are oldest first, ending at the current hour.
	wantFirst := now.Truncate(time.Hour).Add(-23 * time.Hour).Format(timeFormat)
	wantLast := now.Truncate(time.Hour).Format(timeFormat)
	if got.CompletedByHour[0].Hour != wantFirst {
		t.Errorf("first bucket = %s, want %s", got.CompletedByHour[0].Hour, wantFirst)
	}
	if got.CompletedByHour[23].Hour != wantLast {
		t.Errorf("last bucket = %s, want %s", got.CompletedByHour[23].Hour, wantLast)
	}
}

// TestChartsEndpointEmptyDataEmitsEmptyArraysNotNull asserts an empty
// ChartsData still serves "passes": [] (never null), while completedByHour
// is always the full 24 zero-filled buckets.
func TestChartsEndpointEmptyDataEmitsEmptyArraysNotNull(t *testing.T) {
	reg := prometheus.NewRegistry()
	charts := func(ctx context.Context) (ChartsData, error) { return ChartsData{}, nil }
	h := newChartsTestHandler(reg, charts)

	req := httptest.NewRequest(http.MethodGet, "/api/charts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"passes":[]`) {
		t.Errorf("expected \"passes\":[] in body, got %s", body)
	}
	var got chartsDTO
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.CompletedByHour) != 24 {
		t.Fatalf("expected 24 zero-filled hour buckets, got %d", len(got.CompletedByHour))
	}
}

// TestChartsEndpointReturns500OnError asserts a ChartsFunc error maps to a
// 500, matching sibling handlers (e.g. /api/events, /api/peers).
func TestChartsEndpointReturns500OnError(t *testing.T) {
	reg := prometheus.NewRegistry()
	charts := func(ctx context.Context) (ChartsData, error) { return ChartsData{}, errors.New("boom") }
	h := newChartsTestHandler(reg, charts)

	req := httptest.NewRequest(http.MethodGet, "/api/charts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status code = %d, want 500", rec.Code)
	}
}
