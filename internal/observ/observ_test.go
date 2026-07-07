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

// noopJobDetail, noopJobEvents, noopRecentEvents, and noopPeers are no-op
// implementations of the newer dashboard funcs, for tests that only care
// about routes unrelated to job detail/events/peers.
func noopJobDetail(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
	return core.JobDetail{}, false, nil
}
func noopJobEvents(ctx context.Context, jobID int64) ([]core.JobEvent, error)  { return nil, nil }
func noopRecentEvents(ctx context.Context, limit int) ([]core.JobEvent, error) { return nil, nil }
func noopPeers(ctx context.Context) ([]core.PeerRow, error)                    { return nil, nil }
func noopHealthy() bool                                                        { return true }
func noopModules() map[string]time.Time                                        { return nil }
func noopRetry(ctx context.Context, jobID int64) (RetryResult, error)          { return RetryResultOK, nil }

// newTestHandler builds a NewServer with no-op status/jobs/cancel funcs, for
// tests that only care about routes unrelated to those three.
func newTestHandler(reg *prometheus.Registry) http.Handler {
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	return NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)
}

func TestStatusEndpointReturnsReport(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) {
		return StatusReport{Queued: 3, Active: 1, Stalled: 0, Orphaned: 2}, nil
	}
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

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

// TestStatusEndpointIncludesModules verifies /status surfaces each pipeline
// module's last-tick time (RFC3339) alongside the StatusReport fields, and
// renders a never-ticked module (zero time.Time) as an empty string rather
// than a zero-value timestamp.
func TestStatusEndpointIncludesModules(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	ticked := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	modules := func() map[string]time.Time {
		return map[string]time.Time{"wanted_sync": ticked, "discovery": {}}
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, modules, noopRetry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	var got struct {
		Modules map[string]string `json:"modules"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Modules["wanted_sync"] != ticked.Format(timeFormat) {
		t.Errorf("Modules[wanted_sync] = %q, want %q", got.Modules["wanted_sync"], ticked.Format(timeFormat))
	}
	if got.Modules["discovery"] != "" {
		t.Errorf("Modules[discovery] = %q, want empty string for a never-ticked module", got.Modules["discovery"])
	}
}

func TestHealthzEndpointReflectsLiveness(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }

	healthy := true
	healthyFn := func() bool { return healthy }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, healthyFn, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy: status code = %d", rec.Code)
	}

	healthy = false
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stalled: status code = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestMetricsEndpointServes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.ReconcileTotal.Inc()
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

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
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

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
				Attempt: &core.Candidate{FailReason: "transfer failed"},
			},
		}, nil
	}
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

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

func TestJobsEndpointReturnsRetriesAndNotBeforeForWantedJob(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	notBefore := time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job: core.AlbumJob{
					ID: 9, Title: "Waiting", ArtistName: "Someone",
					State: core.StateWanted, Retries: 2, NotBefore: &notBefore,
				},
			},
		}, nil
	}
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got []jobDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantNotBefore := notBefore.Format(timeFormat)
	if got[0].NotBefore != wantNotBefore {
		t.Errorf("NotBefore = %q, want %q", got[0].NotBefore, wantNotBefore)
	}
	if got[0].Retries != 2 {
		t.Errorf("Retries = %d, want 2", got[0].Retries)
	}
	if got[0].NextAttemptAt != "" {
		t.Errorf("NextAttemptAt = %q, want empty (only set for FAILED jobs)", got[0].NextAttemptAt)
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
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

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
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

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
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

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
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) {
		return CancelResultFailed, errors.New("advance failed")
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

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
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/not-a-number/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}

func TestRetryEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	var gotID int64
	retry := func(ctx context.Context, jobID int64) (RetryResult, error) {
		gotID = jobID
		return RetryResultOK, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, retry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/42/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotID != 42 {
		t.Errorf("retry called with id %d, want 42", gotID)
	}
}

func TestRetryEndpointNotFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	retry := func(ctx context.Context, jobID int64) (RetryResult, error) { return RetryResultNotFound, nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, retry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/999/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestRetryEndpointConflictWhenNotFailed(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	retry := func(ctx context.Context, jobID int64) (RetryResult, error) { return RetryResultConflict, nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, retry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status code = %d, want 409", rec.Code)
	}
}

func TestRetryEndpointStoreFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	retry := func(ctx context.Context, jobID int64) (RetryResult, error) {
		return RetryResultOK, errors.New("db exploded")
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, retry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
}

func TestRetryEndpointBadID(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/not-a-number/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}

func TestRetryEndpointMethodNotAllowed(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/1/retry", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status code = %d, want 405", rec.Code)
	}
}

func TestJobDetailEndpointReturnsDetail(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	lastProgress := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)
	jobDetail := func(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
		return core.JobDetail{
			Job: core.AlbumJob{ID: jobID, Title: "Rounds", ArtistName: "Four Tet", State: core.StateFailed},
			Attempts: []core.AttemptDetail{
				{
					Attempt: core.Candidate{ID: 1, Username: "peer_one", State: core.CandidateFailed, FailReason: "transfer failed"},
					Transfers: []core.Transfer{
						{Filename: "01.flac", State: core.TransferErrored, BytesDone: 10, BytesTotal: 100, Retries: 2, LastProgressAt: &lastProgress},
					},
				},
			},
		}, true, nil
	}
	h := NewServer(reg, status, jobs, cancel, jobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/7/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got jobDetailDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != 7 || got.Title != "Rounds" || got.Artist != "Four Tet" {
		t.Errorf("unexpected job detail: %+v", got)
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(got.Attempts))
	}
	a := got.Attempts[0]
	if a.Username != "peer_one" || a.FailReason != "transfer failed" || a.FileCount != 1 {
		t.Errorf("unexpected attempt: %+v", a)
	}
	if len(a.Transfers) != 1 || a.Transfers[0].Filename != "01.flac" || a.Transfers[0].Retries != 2 {
		t.Errorf("unexpected transfers: %+v", a.Transfers)
	}
	if a.Transfers[0].LastProgressAt != lastProgress.Format(timeFormat) {
		t.Errorf("LastProgressAt = %q, want %q", a.Transfers[0].LastProgressAt, lastProgress.Format(timeFormat))
	}
}

func TestJobDetailEndpointNotFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	jobDetail := func(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
		return core.JobDetail{}, false, nil
	}
	h := NewServer(reg, status, jobs, cancel, jobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/999/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestJobDetailEndpointBadID(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/not-a-number/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}

func TestJobEventsEndpointReturnsEvents(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	when := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobEvents := func(ctx context.Context, jobID int64) ([]core.JobEvent, error) {
		return []core.JobEvent{
			{ID: 1, AlbumJobID: jobID, Event: core.EventSearch, Detail: "searched album", CreatedAt: when},
		}, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, jobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/3/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got []eventDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Event != "search" || got[0].JobID != 3 {
		t.Errorf("unexpected events: %+v", got)
	}
}

func TestEventsEndpointDefaultLimit(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	var gotLimit int
	recentEvents := func(ctx context.Context, limit int) ([]core.JobEvent, error) {
		gotLimit = limit
		return nil, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, recentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotLimit != eventsLimitDefault {
		t.Errorf("limit = %d, want default %d", gotLimit, eventsLimitDefault)
	}
}

func TestEventsEndpointClampsLimit(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	var gotLimit int
	recentEvents := func(ctx context.Context, limit int) ([]core.JobEvent, error) {
		gotLimit = limit
		return nil, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, recentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodGet, "/api/events?limit=99999", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if gotLimit != eventsLimitMax {
		t.Errorf("limit = %d, want clamped max %d", gotLimit, eventsLimitMax)
	}
}

func TestPeersEndpointReturnsPeersWithScore(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) (CancelResult, error) { return CancelResultOK, nil }
	now := time.Now()
	peers := func(ctx context.Context) ([]core.PeerRow, error) {
		return []core.PeerRow{
			{
				Username: "reliable_peer",
				Global:   core.ReliabilityCounters{SuccessCount: 5, LastSuccessAt: &now},
				Artists:  map[int64]core.ReliabilityCounters{1: {SuccessCount: 2, LastSuccessAt: &now}},
			},
		}, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, peers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates)

	req := httptest.NewRequest(http.MethodGet, "/api/peers", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got []peerDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Username != "reliable_peer" {
		t.Fatalf("unexpected peers: %+v", got)
	}
	if got[0].Score <= 0.5 {
		t.Errorf("Score = %v, want > 0.5 (peer has only successes)", got[0].Score)
	}
	if len(got[0].Artists) != 1 || got[0].Artists[0].ArtistID != 1 {
		t.Errorf("unexpected artist breakdown: %+v", got[0].Artists)
	}
}
