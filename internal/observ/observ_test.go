package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

const testFailedRetryAfter = time.Hour
const testMaxCandidates = 5

// newTestHandler builds a NewServer with no-op status/jobs/cancel funcs, for
// tests that only care about routes unrelated to those three.
func newTestHandler(reg *prometheus.Registry) http.Handler {
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	return NewServer(reg, status, jobs, cancel, testFailedRetryAfter, testMaxCandidates)
}

func TestStatusEndpointReturnsReport(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) {
		return StatusReport{Queued: 3, Active: 1, Stalled: 0, Orphaned: 2}, nil
	}
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel, testFailedRetryAfter, testMaxCandidates)

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
	h := NewServer(reg, func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }, jobs, cancel, testFailedRetryAfter, testMaxCandidates)

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
	h := NewServer(reg, status, jobs, cancel, testFailedRetryAfter, testMaxCandidates)

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
	if got[0].State != "DOWNLOADING" {
		t.Errorf("State = %q, want DOWNLOADING", got[0].State)
	}
	if got[0].MaxCandidates != testMaxCandidates {
		t.Errorf("MaxCandidates = %d, want %d", got[0].MaxCandidates, testMaxCandidates)
	}
	if got[0].FailReason != "" {
		t.Errorf("FailReason = %q, want empty", got[0].FailReason)
	}
	if got[0].NextAttemptAt != "" {
		t.Errorf("NextAttemptAt = %q, want empty", got[0].NextAttemptAt)
	}
}

func TestJobsEndpointReturnsFailReasonAndNextAttemptForFailedJob(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	updatedAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job: core.AlbumJob{
					ID: 8, Title: "Doomed", ArtistName: "Nobody",
					State: core.StateFailed, CandidatesTried: 5, UpdatedAt: updatedAt,
				},
				Attempt: &core.CandidateAttempt{FailReason: "transfer failed"},
			},
		}, nil
	}
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got []jobDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got[0].FailReason != "transfer failed" {
		t.Errorf("FailReason = %q, want %q", got[0].FailReason, "transfer failed")
	}
	if got[0].CandidatesTried != 5 {
		t.Errorf("CandidatesTried = %d, want 5", got[0].CandidatesTried)
	}
	wantNextAttempt := updatedAt.Add(testFailedRetryAfter).Format(timeFormat)
	if got[0].NextAttemptAt != wantNextAttempt {
		t.Errorf("NextAttemptAt = %q, want %q", got[0].NextAttemptAt, wantNextAttempt)
	}
}

func TestJobsEndpointReturnsNextAttemptForCooldownJob(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	nextAttempt := time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job: core.AlbumJob{
					ID: 9, Title: "Waiting", ArtistName: "Someone",
					State: core.StateCooldown, NextAttemptAt: &nextAttempt,
				},
			},
		}, nil
	}
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got []jobDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantNextAttempt := nextAttempt.Format(timeFormat)
	if got[0].NextAttemptAt != wantNextAttempt {
		t.Errorf("NextAttemptAt = %q, want %q", got[0].NextAttemptAt, wantNextAttempt)
	}
	if got[0].FailReason != "" {
		t.Errorf("FailReason = %q, want empty", got[0].FailReason)
	}
}

func TestJobsEndpointReturns500OnStoreError(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, errors.New("db exploded") }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel, testFailedRetryAfter, testMaxCandidates)

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
	h := NewServer(reg, status, jobs, cancel, testFailedRetryAfter, testMaxCandidates)

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
	h := NewServer(reg, status, jobs, cancel, testFailedRetryAfter, testMaxCandidates)

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
	h := NewServer(reg, status, jobs, cancel, testFailedRetryAfter, testMaxCandidates)

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
	h := NewServer(reg, status, jobs, cancel, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/not-a-number/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}
