package store

import (
	"context"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// bulkRetryQuery is the shape observ hands the store: the same filter axes the
// jobs list renders, with the paging/sort fields left at their defaults since
// BulkRetryJobs ignores them.
func bulkRetryQuery(filter string) DashboardJobsQuery {
	return DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: filter, Source: "all", PageSize: DashboardJobsPageSize}
}

func jobState(t *testing.T, s *Store, id int64) core.AlbumJobState {
	t.Helper()
	var state string
	if err := s.db.QueryRowContext(context.Background(), `SELECT state FROM album_jobs WHERE id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("read job %d state: %v", id, err)
	}
	return core.AlbumJobState(state)
}

func candidateCount(t *testing.T, s *Store, jobID int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM candidates WHERE album_job_id = $1`, jobID).Scan(&n); err != nil {
		t.Fatalf("count candidates for %d: %v", jobID, err)
	}
	return n
}

func transferCount(t *testing.T, s *Store, jobID int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM transfers t JOIN candidates c ON c.id = t.candidate_id WHERE c.album_job_id = $1`, jobID).Scan(&n); err != nil {
		t.Fatalf("count transfers for %d: %v", jobID, err)
	}
	return n
}

// TestBulkRetryJobsRevivesLidarrJobs is RetryFailedJob's semantics applied to a
// set: WANTED, counters zeroed, candidates and their transfers gone.
func TestBulkRetryJobsRevivesLidarrJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	first := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateFailed, core.TransferErrored, "One", "A", "peer_one", 3, now)
	second := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateFailed, core.TransferErrored, "Two", "B", "peer_two", 1, now)

	got, err := s.BulkRetryJobs(ctx, bulkRetryQuery("failed"), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("BulkRetryJobs: %v", err)
	}
	if got.Retried != 2 || got.Skipped != 0 {
		t.Fatalf("BulkRetryJobs = %+v, want 2 retried 0 skipped", got)
	}

	for _, id := range []int64{first, second} {
		if state := jobState(t, s, id); state != core.StateWanted {
			t.Errorf("job %d state = %q, want WANTED", id, state)
		}
		if n := candidateCount(t, s, id); n != 0 {
			t.Errorf("job %d has %d candidates, want them deleted", id, n)
		}
		if n := transferCount(t, s, id); n != 0 {
			t.Errorf("job %d has %d transfers, want them deleted", id, n)
		}
	}

	var retries, emptySearches int
	if err := s.db.QueryRowContext(ctx, `SELECT retries, empty_searches FROM album_jobs WHERE id = $1`, first).Scan(&retries, &emptySearches); err != nil {
		t.Fatalf("read counters: %v", err)
	}
	if retries != 0 || emptySearches != 0 {
		t.Errorf("counters = retries %d empty_searches %d, want both 0", retries, emptySearches)
	}
}

// TestBulkRetryJobsRevivesManualJobs is RetryManualJob's semantics applied to a
// set: SELECTING, the user's chosen candidate revived to NEW rather than
// deleted, its stale transfers gone, fail_reason and import_submitted_at
// cleared (issue #347).
func TestBulkRetryJobsRevivesManualJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	job, err := s.CreateManualJob(ctx, "Album", "Artist", "peer_one", "",
		[]ManualJobFile{{Filename: "f1.flac", Size: 10}}, now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	cand, found, err := s.ActiveCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: found=%v err=%v", found, err)
	}
	if _, err := s.FailCandidateAndAdvance(ctx, cand.ID, job.ID, "transfer failed", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("FailCandidateAndAdvance: %v", err)
	}
	if err := s.MarkJobFailed(ctx, job.ID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	got, err := s.BulkRetryJobs(ctx, bulkRetryQuery("failed"), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("BulkRetryJobs: %v", err)
	}
	if got.Retried != 1 || got.Skipped != 0 {
		t.Fatalf("BulkRetryJobs = %+v, want 1 retried 0 skipped", got)
	}
	if state := jobState(t, s, job.ID); state != core.StateSelecting {
		t.Errorf("job state = %q, want SELECTING", state)
	}

	cands, err := s.CandidatesForJob(ctx, job.ID)
	if err != nil || len(cands) != 1 {
		t.Fatalf("CandidatesForJob = %d (%v), want the original candidate revived, not deleted", len(cands), err)
	}
	if cands[0].ID != cand.ID || cands[0].State != core.CandidateNew {
		t.Errorf("candidate = id %d state %q, want id %d state NEW", cands[0].ID, cands[0].State, cand.ID)
	}
	if cands[0].FailReason != "" {
		t.Errorf("candidate fail_reason = %q, want cleared", cands[0].FailReason)
	}
	var importSubmitted *time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT import_submitted_at FROM candidates WHERE id = $1`, cand.ID).Scan(&importSubmitted); err != nil {
		t.Fatalf("read import_submitted_at: %v", err)
	}
	if importSubmitted != nil {
		t.Errorf("candidate import_submitted_at = %v, want NULL", importSubmitted)
	}
	if n := transferCount(t, s, job.ID); n != 0 {
		t.Errorf("job has %d transfers, want the stale set cleared", n)
	}
}

// TestBulkRetryJobsRoutesBothSourcesInOneCall covers the #347 source conflict
// in bulk: one call, two different revival semantics, counted as one sum.
func TestBulkRetryJobsRoutesBothSourcesInOneCall(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	lidarr := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateFailed, core.TransferErrored, "Lidarr", "A", "peer_one", 0, now)
	manual := insertDashboardTestJob(t, s, 2, core.SourceManual, core.StateFailed, core.TransferErrored, "Manual", "B", "peer_two", 0, now)

	got, err := s.BulkRetryJobs(ctx, bulkRetryQuery("failed"), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("BulkRetryJobs: %v", err)
	}
	if got.Retried != 2 || got.Skipped != 0 {
		t.Fatalf("BulkRetryJobs = %+v, want 2 retried 0 skipped", got)
	}
	if state := jobState(t, s, lidarr); state != core.StateWanted {
		t.Errorf("lidarr job state = %q, want WANTED", state)
	}
	if state := jobState(t, s, manual); state != core.StateSelecting {
		t.Errorf("manual job state = %q, want SELECTING", state)
	}
	if n := candidateCount(t, s, lidarr); n != 0 {
		t.Errorf("lidarr job kept %d candidates, want them deleted", n)
	}
	if n := candidateCount(t, s, manual); n != 1 {
		t.Errorf("manual job has %d candidates, want the user's choice kept", n)
	}
}

// TestBulkRetryJobsSkipsMidRetryDownloadingJob is the safety property: the
// `failed` status matches a DOWNLOADING job whose current candidate's
// transfers have all errored and which the pipeline will retry with its next
// candidate (dashboardJobStatusSQL). The write's own state guard must refuse
// it, so a filter that over-matches costs a skip, never a wrong revival.
func TestBulkRetryJobsSkipsMidRetryDownloadingJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	midRetry := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDownloading, core.TransferErrored, "MidRetry", "A", "peer_one", 0, now)
	terminal := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateFailed, core.TransferErrored, "Terminal", "B", "peer_two", 0, now)

	got, err := s.BulkRetryJobs(ctx, bulkRetryQuery("failed"), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("BulkRetryJobs: %v", err)
	}
	if got.Retried != 1 || got.Skipped != 1 {
		t.Fatalf("BulkRetryJobs = %+v, want 1 retried 1 skipped", got)
	}
	if state := jobState(t, s, midRetry); state != core.StateDownloading {
		t.Errorf("mid-retry job state = %q, want DOWNLOADING (untouched)", state)
	}
	if n := transferCount(t, s, midRetry); n != 1 {
		t.Errorf("mid-retry job has %d transfers, want its own left alone", n)
	}
	if state := jobState(t, s, terminal); state != core.StateWanted {
		t.Errorf("terminal job state = %q, want WANTED", state)
	}
}

// TestBulkRetryJobsSkipsManualJobWithoutCandidates mirrors RetryManualJob's
// rollback as a selection predicate: a manual job whose candidate rows are
// gone has no peer left to retry, so it is skipped rather than parked in
// SELECTING with an empty cache.
func TestBulkRetryJobsSkipsManualJobWithoutCandidates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	// peer "" and no transfer state means no candidate row at all.
	bare := insertDashboardTestJob(t, s, 1, core.SourceManual, core.StateFailed, "", "Bare", "A", "", 0, now)

	got, err := s.BulkRetryJobs(ctx, bulkRetryQuery("failed"), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("BulkRetryJobs: %v", err)
	}
	if got.Retried != 0 || got.Skipped != 1 {
		t.Fatalf("BulkRetryJobs = %+v, want 0 retried 1 skipped", got)
	}
	if state := jobState(t, s, bare); state != core.StateFailed {
		t.Errorf("job state = %q, want FAILED (untouched)", state)
	}
}

// TestBulkRetryJobsParkedFilterCoversBothSpellings: the `parked` status is
// state IN ('PARKED','ORPHANED'), and both must revive.
func TestBulkRetryJobsParkedFilterCoversBothSpellings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	parked := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateParked, core.TransferErrored, "Parked", "A", "peer_one", 0, now)
	orphaned := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateOrphaned, core.TransferErrored, "Orphaned", "B", "peer_two", 0, now)
	failed := insertDashboardTestJob(t, s, 3, core.SourceLidarr, core.StateFailed, core.TransferErrored, "Failed", "C", "peer_three", 0, now)

	got, err := s.BulkRetryJobs(ctx, bulkRetryQuery("parked"), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("BulkRetryJobs: %v", err)
	}
	if got.Retried != 2 || got.Skipped != 0 {
		t.Fatalf("BulkRetryJobs = %+v, want 2 retried 0 skipped", got)
	}
	for _, id := range []int64{parked, orphaned} {
		if state := jobState(t, s, id); state != core.StateWanted {
			t.Errorf("job %d state = %q, want WANTED", id, state)
		}
	}
	if state := jobState(t, s, failed); state != core.StateFailed {
		t.Errorf("FAILED job state = %q, want it left outside the parked filter", state)
	}
}

// TestBulkRetryJobsHonorsSourceAndSearchScope: the operation acts on the view
// the user is looking at, which is all three filter axes, not just the status.
func TestBulkRetryJobsHonorsSourceAndSearchScope(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	matching := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateFailed, core.TransferErrored, "Rounds", "Four Tet", "peer_one", 0, now)
	otherArtist := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateFailed, core.TransferErrored, "Endtroducing", "DJ Shadow", "peer_two", 0, now)
	otherSource := insertDashboardTestJob(t, s, 3, core.SourceManual, core.StateFailed, core.TransferErrored, "Rounds", "Four Tet", "peer_three", 0, now)

	q := bulkRetryQuery("failed")
	q.Source = "lidarr"
	q.Query = "four tet"
	got, err := s.BulkRetryJobs(ctx, q, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("BulkRetryJobs: %v", err)
	}
	if got.Retried != 1 || got.Skipped != 0 {
		t.Fatalf("BulkRetryJobs = %+v, want 1 retried 0 skipped", got)
	}
	if state := jobState(t, s, matching); state != core.StateWanted {
		t.Errorf("in-scope job state = %q, want WANTED", state)
	}
	for _, id := range []int64{otherArtist, otherSource} {
		if state := jobState(t, s, id); state != core.StateFailed {
			t.Errorf("out-of-scope job %d state = %q, want FAILED (untouched)", id, state)
		}
	}
}

// TestBulkRetryJobsRejectsUnknownFilter: the store validates the query with
// the same allowlist the read path uses, so an unknown filter can never reach
// the write as a silently-ignored predicate.
func TestBulkRetryJobsRejectsUnknownFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	failed := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateFailed, core.TransferErrored, "Failed", "A", "peer_one", 0, now)

	if _, err := s.BulkRetryJobs(ctx, bulkRetryQuery("bogus"), now); err == nil {
		t.Fatal("BulkRetryJobs accepted an unknown filter")
	}
	if state := jobState(t, s, failed); state != core.StateFailed {
		t.Errorf("job state = %q, want FAILED (untouched)", state)
	}
}

// TestBulkRetryJobsEmptyScope: nothing matching is a zero result, not an error.
func TestBulkRetryJobsEmptyScope(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDone, "", "Done", "A", "", 0, now)

	got, err := s.BulkRetryJobs(ctx, bulkRetryQuery("failed"), now)
	if err != nil {
		t.Fatalf("BulkRetryJobs: %v", err)
	}
	if got.Retried != 0 || got.Skipped != 0 {
		t.Fatalf("BulkRetryJobs = %+v, want a zero result", got)
	}
}
