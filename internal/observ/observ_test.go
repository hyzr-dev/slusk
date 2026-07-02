package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestStatusEndpointReturnsReport(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) {
		return StatusReport{Queued: 3, Active: 1, Stalled: 0, Orphaned: 2}, nil
	}
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel)

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
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }, jobs, cancel)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
}

func TestJobsEndpointReturnsJobList(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job:      core.AlbumJob{ID: 7, Title: "Rounds", ArtistName: "Four Tet", State: core.StateDownloading},
				Transfer: &core.Transfer{State: core.TransferInProgress, BytesDone: 100, BytesTotal: 200},
				Peer:     "flac_hoarder",
			},
		}, nil
	}
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got []jobDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 job, got %d", len(got))
	}
	if got[0].ID != 7 || got[0].Title != "Rounds" || got[0].Artist != "Four Tet" {
		t.Errorf("unexpected job DTO: %+v", got[0])
	}
	if got[0].Status != "active" {
		t.Errorf("Status = %q, want active", got[0].Status)
	}
	if got[0].Peer != "flac_hoarder" {
		t.Errorf("Peer = %q, want flac_hoarder", got[0].Peer)
	}
	if got[0].BytesDone != 100 || got[0].BytesTotal != 200 {
		t.Errorf("bytes = %d/%d, want 100/200", got[0].BytesDone, got[0].BytesTotal)
	}
}

func TestJobsEndpointReturns500OnStoreError(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, errors.New("db exploded") }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
}

func TestCancelEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	var gotID int64
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) {
		gotID = jobID
		return CancelResultOK, nil
	}
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/42/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotID != 42 {
		t.Errorf("cancel called with id %d, want 42", gotID)
	}
}

func TestCancelEndpointNotFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultNotFound, nil }
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/999/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestCancelEndpointStoreFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultFailed, errors.New("advance failed") }
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status code = %d, want 502", rec.Code)
	}
}

func TestCancelEndpointBadID(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/not-a-number/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}
