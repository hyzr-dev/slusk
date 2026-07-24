package observ

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/app"
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
func noopModules() map[string]ModuleStatus                                     { return nil }
func noopRetry(ctx context.Context, jobID int64) error                         { return nil }
func noopConfig() AppConfig                                                    { return AppConfig{} }
func noopLiveTransfers(ctx context.Context) ([]core.RemoteTransfer, error)     { return nil, nil }
func noopCharts(ctx context.Context) (ChartsData, error)                       { return ChartsData{}, nil }
func noopConfigWriter(ConfigUpdate) error                                      { return nil }
func noopRestart()                                                             {}
func noopCreateJob(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.AlbumJob, error) {
	return core.AlbumJob{}, nil
}
func noopSearchJob(ctx context.Context, jobID int64) error { return nil }
func noopDeleteJob(ctx context.Context, jobID int64) error { return nil }

// newTestHandler builds a NewServer with no-op status/jobs/cancel funcs, for
// tests that only care about routes unrelated to those three.
func newTestHandler(reg *prometheus.Registry) http.Handler {
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	return NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)
}

func TestStatusEndpointReturnsReport(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) {
		return StatusReport{Queued: 3, Active: 1, Stalled: 0, Orphaned: 2}, nil
	}
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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

func TestStatusEndpointIncludesModuleRuntimeState(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	attempted := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	completed := attempted.Add(4 * time.Second)
	succeeded := attempted.Add(-time.Minute)
	errored := completed
	staleDeadline := attempted.Add(90 * time.Second)
	modules := func() map[string]ModuleStatus {
		return map[string]ModuleStatus{
			"wanted_sync": {
				LastAttempt: attempted, LastCompleted: completed, LastSuccess: succeeded, LastErrorAt: errored,
				LastError: "temporary failure", ConsecutiveFailures: 2,
				StaleDeadline: staleDeadline, Live: true, Ready: true,
			},
			"discovery": {},
		}
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, modules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	type moduleDetail struct {
		LastAttempt         string `json:"lastAttempt"`
		LastCompleted       string `json:"lastCompleted"`
		LastSuccess         string `json:"lastSuccess"`
		LastErrorAt         string `json:"lastErrorAt"`
		LastError           string `json:"lastError"`
		ConsecutiveFailures int    `json:"consecutiveFailures"`
		StaleDeadline       string `json:"staleDeadline"`
		Live                bool   `json:"live"`
		Ready               bool   `json:"ready"`
	}
	var got struct {
		Modules       map[string]string       `json:"modules"`
		ModuleDetails map[string]moduleDetail `json:"moduleDetails"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantCompatibility := map[string]string{
		"wanted_sync": completed.Format(timeFormat),
		"discovery":   "",
	}
	if !reflect.DeepEqual(got.Modules, wantCompatibility) {
		t.Errorf("modules compatibility map = %#v, want exactly %#v", got.Modules, wantCompatibility)
	}
	module := got.ModuleDetails["wanted_sync"]
	if module.LastAttempt != attempted.Format(timeFormat) || module.LastCompleted != completed.Format(timeFormat) || module.LastSuccess != succeeded.Format(timeFormat) || module.LastErrorAt != errored.Format(timeFormat) {
		t.Errorf("unexpected module timestamps: %+v", module)
	}
	if module.LastError != "temporary failure" || module.ConsecutiveFailures != 2 {
		t.Errorf("unexpected module failure state: %+v", module)
	}
	if module.StaleDeadline != staleDeadline.Format(timeFormat) || !module.Live || !module.Ready {
		t.Errorf("unexpected authoritative health: %+v", module)
	}
	if discovery := got.ModuleDetails["discovery"]; discovery.LastAttempt != "" || discovery.LastCompleted != "" {
		t.Errorf("never-attempted module should have empty timestamps: %+v", discovery)
	}
}

func TestHealthEndpointsSeparateLivenessAndReadiness(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }

	live, ready := true, false
	h := NewServerWithReadiness(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers,
		func() bool { return live }, func() bool { return ready }, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

	for _, tt := range []struct {
		path string
		want int
	}{{"/healthz", http.StatusOK}, {"/readyz", http.StatusServiceUnavailable}} {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s status = %d, want %d", tt.path, rec.Code, tt.want)
		}
	}

	live, ready = false, true
	for _, tt := range []struct {
		path string
		want int
	}{{"/healthz", http.StatusServiceUnavailable}, {"/readyz", http.StatusOK}} {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s status = %d, want %d", tt.path, rec.Code, tt.want)
		}
	}
}

func TestMetricsEndpointServes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.ReconcileTotal.Inc()
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	h := NewServer(reg, func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
				Job:      core.AlbumJob{ID: 7, Title: "Rounds", ArtistName: "Four Tet", State: core.StateDownloading, Source: core.SourceLidarr},
				Transfer: &core.Transfer{State: core.TransferInProgress, BytesDone: 100, BytesTotal: 200},
				Peer:     "flac_hoarder",
			},
		}, nil
	}
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	if got[0].Source != "lidarr" {
		t.Errorf("Source = %q, want lidarr", got[0].Source)
	}
}

// TestJobsEndpointReturnsCandidateMetadata verifies year/tracks/format
// serialize when set (a job past selection) and as JSON null when unset (a
// job with no candidate yet), see issue #156.
func TestJobsEndpointReturnsCandidateMetadata(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	year := 2024
	tracks := 12
	format := "FLAC"
	jobs := func(ctx context.Context) ([]core.JobView, error) {
		return []core.JobView{
			{
				Job: core.AlbumJob{
					ID: 9, Title: "With Metadata", ArtistName: "Someone", State: core.StateDownloading,
					Year: &year, Tracks: &tracks, Format: &format,
				},
			},
			{
				Job: core.AlbumJob{ID: 10, Title: "No Candidate Yet", ArtistName: "Nobody", State: core.StateWanted},
			},
		}, nil
	}
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	if len(got) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(got))
	}
	if got[0].Year == nil || *got[0].Year != 2024 {
		t.Errorf("Year = %v, want 2024", got[0].Year)
	}
	if got[0].Tracks == nil || *got[0].Tracks != 12 {
		t.Errorf("Tracks = %v, want 12", got[0].Tracks)
	}
	if got[0].Format == nil || *got[0].Format != "FLAC" {
		t.Errorf("Format = %v, want FLAC", got[0].Format)
	}
	if got[1].Year != nil || got[1].Tracks != nil || got[1].Format != nil {
		t.Errorf("expected nil year/tracks/format for job without candidate, got %+v", got[1])
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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
}

func TestCreateJobEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	var gotTitle, gotArtist, gotPeer string
	var gotFiles []core.CandidateFile
	create := func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.AlbumJob, error) {
		gotTitle, gotArtist, gotPeer, gotFiles = title, artist, peer, files
		return core.AlbumJob{ID: 42, Title: title, ArtistName: artist, State: core.StateDownloading, Source: core.SourceManual}, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, create, noopSearchJob, noopDeleteJob)

	body := `{"title":"Some Album","artist":"Some Artist","peer":"flac_hoarder","files":[{"filename":"a.flac","size":111}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got jobDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != 42 || got.Source != "manual" {
		t.Errorf("unexpected job DTO: %+v", got)
	}
	if gotTitle != "Some Album" || gotArtist != "Some Artist" || gotPeer != "flac_hoarder" {
		t.Errorf("create called with title=%q artist=%q peer=%q", gotTitle, gotArtist, gotPeer)
	}
	if len(gotFiles) != 1 || gotFiles[0].Filename != "a.flac" || gotFiles[0].Size != 111 {
		t.Errorf("create called with files=%+v", gotFiles)
	}
}

func TestCreateJobEndpointMissingPeerReturns422(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	called := false
	create := func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.AlbumJob, error) {
		called = true
		return core.AlbumJob{}, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, create, noopSearchJob, noopDeleteJob)

	body := `{"files":[{"filename":"a.flac","size":111}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got.FieldErrors["peer"]; !ok {
		t.Errorf("fieldErrors = %+v, want a \"peer\" entry", got.FieldErrors)
	}
	if called {
		t.Error("create must not be called on a validation failure")
	}
}

func TestCreateJobEndpointEmptyFilesReturns422(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	create := func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.AlbumJob, error) {
		return core.AlbumJob{}, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, create, noopSearchJob, noopDeleteJob)

	body := `{"peer":"flac_hoarder","files":[]}`
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got.FieldErrors["files"]; !ok {
		t.Errorf("fieldErrors = %+v, want a \"files\" entry", got.FieldErrors)
	}
}

func TestCreateJobEndpointMalformedJSONReturns400(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	create := func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.AlbumJob, error) {
		return core.AlbumJob{}, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, create, noopSearchJob, noopDeleteJob)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateJobEndpointRemoteFileBusyReturns409(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	create := func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.AlbumJob, error) {
		return core.AlbumJob{}, app.ErrRemoteFileBusy
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, create, noopSearchJob, noopDeleteJob)

	body := `{"peer":"flac_hoarder","files":[{"filename":"a.flac","size":111}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCancelEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	var gotID int64
	cancel := func(ctx context.Context, jobID int64) error {
		gotID = jobID
		return nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return app.ErrJobNotFound }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error {
		return errors.New("advance failed")
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	var gotID int64
	retry := func(ctx context.Context, jobID int64) error {
		gotID = jobID
		return nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, retry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	retry := func(ctx context.Context, jobID int64) error { return app.ErrJobNotFound }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, retry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	retry := func(ctx context.Context, jobID int64) error { return app.ErrJobNotRetryable }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, retry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	retry := func(ctx context.Context, jobID int64) error {
		return errors.New("db exploded")
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, retry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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

func TestSearchEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	var gotID int64
	search := func(ctx context.Context, jobID int64) error {
		gotID = jobID
		return nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, search, noopDeleteJob)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/42/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotID != 42 {
		t.Errorf("search called with id %d, want 42", gotID)
	}
}

func TestSearchEndpointNotFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	search := func(ctx context.Context, jobID int64) error { return app.ErrJobNotFound }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, search, noopDeleteJob)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/999/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestSearchEndpointConflictWhenActive(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	search := func(ctx context.Context, jobID int64) error { return app.ErrJobActive }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, search, noopDeleteJob)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status code = %d, want 409", rec.Code)
	}
}

func TestSearchEndpointStoreFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	search := func(ctx context.Context, jobID int64) error {
		return errors.New("db exploded")
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, search, noopDeleteJob)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/1/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
}

func TestSearchEndpointBadID(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/not-a-number/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}

func TestSearchEndpointMethodNotAllowed(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/1/search", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status code = %d, want 405", rec.Code)
	}
}

func TestDeleteEndpointSuccess(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	var gotID int64
	del := func(ctx context.Context, jobID int64) error {
		gotID = jobID
		return nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, del)

	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/42", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotID != 42 {
		t.Errorf("delete called with id %d, want 42", gotID)
	}
}

func TestDeleteEndpointNotFound(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	del := func(ctx context.Context, jobID int64) error { return app.ErrJobNotFound }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, del)

	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/999", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestDeleteEndpointConflictWhenImporting(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	del := func(ctx context.Context, jobID int64) error { return app.ErrJobImporting }
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, del)

	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status code = %d, want 409", rec.Code)
	}
}

func TestDeleteEndpointStoreFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	del := func(ctx context.Context, jobID int64) error {
		return errors.New("db exploded")
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, del)

	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
}

func TestDeleteEndpointBadID(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/not-a-number", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}

func TestJobDetailEndpointReturnsDetail(t *testing.T) {
	reg := prometheus.NewRegistry()
	status := func(ctx context.Context) (StatusReport, error) { return StatusReport{}, nil }
	jobs := func(ctx context.Context) ([]core.JobView, error) { return nil, nil }
	cancel := func(ctx context.Context, jobID int64) error { return nil }
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
	h := NewServer(reg, status, jobs, cancel, jobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	jobDetail := func(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
		return core.JobDetail{}, false, nil
	}
	h := NewServer(reg, status, jobs, cancel, jobDetail, noopJobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	when := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobEvents := func(ctx context.Context, jobID int64) ([]core.JobEvent, error) {
		return []core.JobEvent{
			{ID: 1, AlbumJobID: jobID, Event: core.EventSearch, Detail: "searched album", CreatedAt: when},
		}, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, jobEvents, noopRecentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	var gotLimit int
	recentEvents := func(ctx context.Context, limit int) ([]core.JobEvent, error) {
		gotLimit = limit
		return nil, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, recentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
	var gotLimit int
	recentEvents := func(ctx context.Context, limit int) ([]core.JobEvent, error) {
		gotLimit = limit
		return nil, nil
	}
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, recentEvents, noopPeers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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
	cancel := func(ctx context.Context, jobID int64) error { return nil }
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
	h := NewServer(reg, status, jobs, cancel, noopJobDetail, noopJobEvents, noopRecentEvents, peers, noopHealthy, noopModules, noopRetry, testFailedRetryAfter, testMaxCandidates, noopConfig, noopLiveTransfers, ConnectionTester{}, noopCharts, noopConfigWriter, noopRestart, noopCreateJob, noopSearchJob, noopDeleteJob)

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

// TestMuxDoesNotSwallowUnregisteredAPIPaths exercises the real ServeMux built
// by NewServer, not the asset handler directly. It guards against a
// regression where a future "/api/" prefix handler swallows unregistered API
// paths and answers them with the SPA shell instead of 404 — assets_test.go
// only ever calls newAssetHandler() in isolation, so it can't catch that.
func TestMuxDoesNotSwallowUnregisteredAPIPaths(t *testing.T) {
	h := newTestHandler(prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, must not be HTML for an unregistered API path", ct)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/: status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("/: Content-Type = %q, want text/html", ct)
	}
}
