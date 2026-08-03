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
	"github.com/samuelenocsson/slusk/internal/core"
)

func newChartsTestHandler(reg *prometheus.Registry, charts ChartsFunc) http.Handler {
	deps := testServerDeps(reg)
	deps.Charts = charts
	return NewServer(deps)
}

func newChartsTestHandlerWithThroughput(reg *prometheus.Registry, charts ChartsFunc, throughput ThroughputFunc) http.Handler {
	deps := testServerDeps(reg)
	deps.Charts = charts
	deps.Throughput = throughput
	return NewServer(deps)
}

// TestChartsEndpointServesBidirectionalThroughputOldestFirst asserts both
// directional series are served as-is under their additive wire keys.
func TestChartsEndpointServesBidirectionalThroughputOldestFirst(t *testing.T) {
	reg := prometheus.NewRegistry()
	charts := func(ctx context.Context) (ChartsData, error) { return ChartsData{}, nil }
	older := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Second)
	throughput := func(ctx context.Context) (core.ThroughputSeries, error) {
		return core.ThroughputSeries{
			Download: []core.ThroughputSample{
				{At: older, BytesPerSecond: 100, ActiveTransfers: 1},
				{At: newer, BytesPerSecond: 200, ActiveTransfers: 2},
			},
			Upload: []core.ThroughputSample{
				{At: older, BytesPerSecond: 300, ActiveTransfers: 3},
				{At: newer, BytesPerSecond: 400, ActiveTransfers: 4},
			},
		}, nil
	}
	h := newChartsTestHandlerWithThroughput(reg, charts, throughput)

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
	if len(got.Throughput) != 2 || got.Throughput[0].BytesPerSecond != 100 || got.Throughput[1].BytesPerSecond != 200 {
		t.Errorf("download throughput not oldest-first: %+v", got.Throughput)
	}
	if len(got.UploadThroughput) != 2 || got.UploadThroughput[0].BytesPerSecond != 300 || got.UploadThroughput[1].BytesPerSecond != 400 {
		t.Errorf("upload throughput not oldest-first: %+v", got.UploadThroughput)
	}
}

// TestChartsEndpointThroughputEmitsEmptyArraysNotNull asserts a nil
// ThroughputFunc (the slskd path) serves both directional keys as arrays.
func TestChartsEndpointThroughputEmitsEmptyArraysNotNull(t *testing.T) {
	reg := prometheus.NewRegistry()
	charts := func(ctx context.Context) (ChartsData, error) { return ChartsData{}, nil }
	h := newChartsTestHandlerWithThroughput(reg, charts, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/charts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"throughput":[]`) || !strings.Contains(body, `"uploadThroughput":[]`) {
		t.Errorf("expected both empty throughput arrays in body, got %s", body)
	}
}

// TestChartsEndpointServesPassesWhenThroughputErrors asserts a ThroughputFunc
// error degrades to an empty throughput series without failing the request —
// passes/completedByHour are still served (issue #157: a native-backend-only
// data source failing must not blank the whole Overview view).
func TestChartsEndpointServesPassesWhenThroughputErrors(t *testing.T) {
	reg := prometheus.NewRegistry()
	passes := []core.SearchPass{{StartedAt: time.Now(), FinishedAt: time.Now(), Searched: 1, Matched: 1}}
	charts := func(ctx context.Context) (ChartsData, error) { return ChartsData{Passes: passes}, nil }
	throughput := func(ctx context.Context) (core.ThroughputSeries, error) {
		return core.ThroughputSeries{}, errors.New("throughput boom")
	}
	h := newChartsTestHandlerWithThroughput(reg, charts, throughput)

	req := httptest.NewRequest(http.MethodGet, "/api/charts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (best-effort degrade), body = %s", rec.Code, rec.Body.String())
	}
	var got chartsDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Passes) != 1 {
		t.Errorf("expected passes still served, got %+v", got.Passes)
	}
	if len(got.Throughput) != 0 || len(got.UploadThroughput) != 0 {
		t.Errorf("expected both throughput directions empty on error, got download=%+v upload=%+v", got.Throughput, got.UploadThroughput)
	}
}

// TestToChartsDTOZeroFillsHoursAndOrdersPassesOldestFirst asserts the bucket
// math and pass-ordering logic directly against a fixed now, rather than
// through the HTTP handler (which stamps time.Now()): calling toChartsDTO
// with an injected now keeps this test's assertions - which pin exact bucket
// boundaries relative to now - independent of wall-clock time.
func TestToChartsDTOZeroFillsHoursAndOrdersPassesOldestFirst(t *testing.T) {
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

	throughput := core.ThroughputSeries{
		Download: []core.ThroughputSample{{At: now, BytesPerSecond: 500, ActiveTransfers: 1}},
		Upload:   []core.ThroughputSample{{At: now, BytesPerSecond: 600, ActiveTransfers: 2}},
	}
	got := toChartsDTO(ChartsData{Passes: passes, CompletedByHour: counts}, throughput, now)

	if len(got.Throughput) != 1 || got.Throughput[0].BytesPerSecond != 500 {
		t.Errorf("throughput = %+v, want the injected download sample", got.Throughput)
	}
	if len(got.UploadThroughput) != 1 || got.UploadThroughput[0].BytesPerSecond != 600 {
		t.Errorf("uploadThroughput = %+v, want the injected upload sample", got.UploadThroughput)
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

// TestChartsEndpointServesPassesAndZeroFilledHourBuckets is an HTTP-level
// sanity check limited to shape (status, array presence, bucket count) since
// the handler stamps time.Now() itself - anything pinned to exact bucket
// boundaries belongs in TestToChartsDTOZeroFillsHoursAndOrdersPassesOldestFirst
// instead, which controls now directly.
func TestChartsEndpointServesPassesAndZeroFilledHourBuckets(t *testing.T) {
	reg := prometheus.NewRegistry()
	passes := []core.SearchPass{
		{StartedAt: time.Now(), FinishedAt: time.Now(), Searched: 1, Matched: 1},
		{StartedAt: time.Now(), FinishedAt: time.Now(), Searched: 1, Matched: 0},
	}
	charts := func(ctx context.Context) (ChartsData, error) {
		return ChartsData{Passes: passes}, nil
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
	if len(got.CompletedByHour) != 24 {
		t.Fatalf("expected 24 zero-filled hour buckets, got %d", len(got.CompletedByHour))
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
