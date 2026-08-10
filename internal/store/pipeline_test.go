package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCountJobsInStates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if _, err := s.UpsertWantedJob(ctx, 300, now); err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	downloading, _ := s.UpsertWantedJob(ctx, 301, now)
	if err := s.AdvanceJobState(ctx, downloading.ID, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	importing, _ := s.UpsertWantedJob(ctx, 302, now)
	if err := s.AdvanceJobState(ctx, importing.ID, core.StateImporting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	count, err := s.CountJobsInStates(ctx, core.StateDownloading, core.StateImporting)
	if err != nil {
		t.Fatalf("CountJobsInStates: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 jobs in DOWNLOADING/IMPORTING, got %d", count)
	}

	countWanted, err := s.CountJobsInStates(ctx, core.StateWanted)
	if err != nil {
		t.Fatalf("CountJobsInStates: %v", err)
	}
	if countWanted != 1 {
		t.Errorf("expected 1 job in WANTED, got %d", countWanted)
	}
}

func TestCandidatesAndTransfersForJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertWantedJob(ctx, 200, now)
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "bob", Score: 2.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	candidate, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	_, _, _ = s.RecordEnqueueIntent(ctx, candidate.ID, "bob", "f.flac", now.Add(time.Hour), now)

	candidates, err := s.CandidatesForJob(ctx, job.ID)
	if err != nil || len(candidates) != 1 || candidates[0].Username != "bob" {
		t.Fatalf("CandidatesForJob: %v %+v", err, candidates)
	}
	transfers, err := s.TransfersForCandidate(ctx, candidate.ID)
	if err != nil || len(transfers) != 1 {
		t.Fatalf("TransfersForCandidate: %v %+v", err, transfers)
	}

	failedAt := now.Add(10 * time.Minute)
	if err := s.FailCandidate(ctx, candidate.ID, "timeout", failedAt); err != nil {
		t.Fatalf("FailCandidate: %v", err)
	}
	after, _ := s.CandidatesForJob(ctx, job.ID)
	if after[0].State != core.CandidateFailed || after[0].FailReason != "timeout" {
		t.Errorf("candidate not marked failed: %+v", after[0])
	}
	if !after[0].UpdatedAt.Equal(failedAt) {
		t.Errorf("UpdatedAt = %v, want %v after FailCandidate", after[0].UpdatedAt, failedAt)
	}
}

func TestFailAndSucceedCandidateBumpUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertWantedJob(ctx, 201, now)

	// FailCandidate sets updated_at to the given now, not created_at.
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "alice", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	failCand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	failedAt := now.Add(5 * time.Minute)
	if err := s.FailCandidate(ctx, failCand.ID, "timeout", failedAt); err != nil {
		t.Fatalf("FailCandidate: %v", err)
	}
	candidates, err := s.CandidatesForJob(ctx, job.ID)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("CandidatesForJob: %v %+v", err, candidates)
	}
	if !candidates[0].CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want unchanged %v", candidates[0].CreatedAt, now)
	}
	if !candidates[0].UpdatedAt.Equal(failedAt) {
		t.Errorf("UpdatedAt = %v, want %v after FailCandidate", candidates[0].UpdatedAt, failedAt)
	}

	// SucceedCandidate sets updated_at to the given now, not created_at.
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "bob", Score: 2.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	succeedCand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	succeededAt := now.Add(10 * time.Minute)
	if err := s.SucceedCandidate(ctx, succeedCand.ID, succeededAt); err != nil {
		t.Fatalf("SucceedCandidate: %v", err)
	}
	candidates, err = s.CandidatesForJob(ctx, job.ID)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("CandidatesForJob: %v %+v", err, candidates)
	}
	var succeeded core.Candidate
	for _, c := range candidates {
		if c.ID == succeedCand.ID {
			succeeded = c
		}
	}
	if succeeded.State != core.CandidateSucceeded {
		t.Errorf("State = %q, want SUCCEEDED", succeeded.State)
	}
	if !succeeded.UpdatedAt.Equal(succeededAt) {
		t.Errorf("UpdatedAt = %v, want %v after SucceedCandidate", succeeded.UpdatedAt, succeededAt)
	}
}

func TestRunnableJobsFiltersNotBefore(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	older, err := s.UpsertWantedJob(ctx, 500, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.UpdateJobMetadata(ctx, older.ID, "Older", "Artist", "2020-01-01", 0, now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}

	newer, err := s.UpsertWantedJob(ctx, 501, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.UpdateJobMetadata(ctx, newer.ID, "Newer", "Artist", "2026-01-01", 0, now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}
	// Hide the newer-release job behind a not_before in the future.
	if err := s.SetJobBackoff(ctx, newer.ID, 1, now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetJobBackoff: %v", err)
	}

	runnable, err := s.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(runnable) != 1 || runnable[0].ID != older.ID {
		t.Fatalf("expected only the older (not-backed-off) job, got %+v", runnable)
	}

	// Once now passes not_before, both are runnable, newest release first.
	both, err := s.RunnableJobsInState(ctx, core.StateWanted, now.Add(2*time.Hour), 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(both) != 2 || both[0].ID != newer.ID || both[1].ID != older.ID {
		t.Fatalf("expected [newer, older] (release_date DESC), got %+v", both)
	}
}

func TestCancelJobsNotWanted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	wanted, err := s.UpsertWantedJob(ctx, 600, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	downloading, err := s.UpsertWantedJob(ctx, 601, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.AdvanceJobState(ctx, downloading.ID, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	done, err := s.UpsertWantedJob(ctx, 602, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.AdvanceJobState(ctx, done.ID, core.StateDone, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	count, err := s.CancelJobsNotWanted(ctx, []int64{}, now)
	if err != nil {
		t.Fatalf("CancelJobsNotWanted: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 jobs cancelled, got %d", count)
	}

	cancelled, err := s.RunnableJobsInState(ctx, core.StateCancelled, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	gotIDs := map[int64]bool{}
	for _, j := range cancelled {
		gotIDs[j.ID] = true
	}
	if !gotIDs[wanted.ID] || !gotIDs[downloading.ID] {
		t.Errorf("expected WANTED and DOWNLOADING jobs cancelled, got %+v", cancelled)
	}

	stillDone, err := s.RunnableJobsInState(ctx, core.StateDone, now, 10)
	if err != nil || len(stillDone) != 1 || stillDone[0].ID != done.ID {
		t.Fatalf("DONE job should be untouched, got %+v (%v)", stillDone, err)
	}
}

func TestReviveFailedJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)

	// FAILED 31 days ago, still wanted -> revived.
	oldFailed, err := s.UpsertWantedJob(ctx, 700, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	// A leftover empty-search streak from before the job failed must not
	// survive revival either - it means nothing once the job starts a
	// completely fresh cycle.
	if err := s.SetJobEmptySearchBackoff(ctx, oldFailed.ID, 7, now, now); err != nil {
		t.Fatalf("SetJobEmptySearchBackoff: %v", err)
	}
	if err := s.MarkJobFailed(ctx, oldFailed.ID, now.Add(-31*24*time.Hour)); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	// FAILED 1 day ago -> untouched (not old enough).
	recentFailed, err := s.UpsertWantedJob(ctx, 701, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.MarkJobFailed(ctx, recentFailed.ID, now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	// FAILED 31 days ago but no longer wanted -> untouched.
	unwanted, err := s.UpsertWantedJob(ctx, 702, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.MarkJobFailed(ctx, unwanted.ID, now.Add(-31*24*time.Hour)); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	count, err := s.ReviveFailedJobs(ctx, []int64{700, 701}, cutoff, now)
	if err != nil {
		t.Fatalf("ReviveFailedJobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 job revived, got %d", count)
	}

	revived, err := s.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil || len(revived) != 1 || revived[0].ID != oldFailed.ID {
		t.Fatalf("expected only oldFailed revived to WANTED, got %+v (%v)", revived, err)
	}
	if revived[0].Retries != 0 {
		t.Errorf("Retries = %d, want 0 after revival", revived[0].Retries)
	}
	if revived[0].EmptySearches != 0 {
		t.Errorf("EmptySearches = %d, want 0 after revival", revived[0].EmptySearches)
	}
	if revived[0].FailedAt != nil {
		t.Errorf("FailedAt = %v, want nil after revival", revived[0].FailedAt)
	}

	stillFailed, err := s.RunnableJobsInState(ctx, core.StateFailed, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	gotIDs := map[int64]bool{}
	for _, j := range stillFailed {
		gotIDs[j.ID] = true
	}
	if !gotIDs[recentFailed.ID] || !gotIDs[unwanted.ID] {
		t.Errorf("expected recentFailed and unwanted still FAILED, got %+v", stillFailed)
	}
}

func TestAdvanceJobStateFrom(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 800, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	ok, err := s.AdvanceJobStateFrom(ctx, job.ID, core.StateWanted, core.StateSearching, now)
	if err != nil || !ok {
		t.Fatalf("AdvanceJobStateFrom: ok=%v err=%v", ok, err)
	}

	// The from-state no longer matches -> no row changed.
	ok, err = s.AdvanceJobStateFrom(ctx, job.ID, core.StateWanted, core.StateSelecting, now)
	if err != nil {
		t.Fatalf("AdvanceJobStateFrom: %v", err)
	}
	if ok {
		t.Fatal("AdvanceJobStateFrom: expected false when current state no longer matches from")
	}
}

// TestReviveFailedJobsEmptyWantedRevivesNothing: with no wanted albums, the
// ANY($wantedIDs) filter matches nothing, so no FAILED job is revived.
func TestReviveFailedJobsEmptyWantedRevivesNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 900, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.MarkJobFailed(ctx, job.ID, now.Add(-31*24*time.Hour)); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	count, err := s.ReviveFailedJobs(ctx, []int64{}, now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatalf("ReviveFailedJobs: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 revived with empty wantedIDs, got %d", count)
	}
	failed, err := s.RunnableJobsInState(ctx, core.StateFailed, now, 10)
	if err != nil || len(failed) != 1 || failed[0].ID != job.ID {
		t.Fatalf("job must stay FAILED, got %+v (%v)", failed, err)
	}
}

// TestSetJobEmptySearchBackoff: bumps empty_searches and hides the job until
// notBefore, but must NOT touch retries or state (issue #334) - unlike
// SetJobBackoff this is never a failure-budget write.
func TestSetJobEmptySearchBackoff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 950, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	// Give the job a nonzero retries first, to prove SetJobEmptySearchBackoff
	// leaves it alone.
	if err := s.SetJobBackoff(ctx, job.ID, 2, now, now); err != nil {
		t.Fatalf("SetJobBackoff: %v", err)
	}

	notBefore := now.Add(24 * time.Hour)
	if err := s.SetJobEmptySearchBackoff(ctx, job.ID, 5, notBefore, now.Add(time.Minute)); err != nil {
		t.Fatalf("SetJobEmptySearchBackoff: %v", err)
	}

	// Hidden until notBefore passes.
	hidden, err := s.RunnableJobsInState(ctx, core.StateWanted, now.Add(time.Minute), 10)
	if err != nil || len(hidden) != 0 {
		t.Fatalf("expected job hidden by not_before, got %+v (%v)", hidden, err)
	}

	jobs, err := s.RunnableJobsInState(ctx, core.StateWanted, notBefore, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("RunnableJobsInState: %v %+v", err, jobs)
	}
	if jobs[0].EmptySearches != 5 {
		t.Errorf("EmptySearches = %d, want 5", jobs[0].EmptySearches)
	}
	if jobs[0].Retries != 2 {
		t.Errorf("Retries = %d, want 2 (untouched by SetJobEmptySearchBackoff)", jobs[0].Retries)
	}
	if jobs[0].State != core.StateWanted {
		t.Errorf("State = %v, want WANTED (untouched)", jobs[0].State)
	}
	if jobs[0].NotBefore == nil || !jobs[0].NotBefore.Equal(notBefore) {
		t.Errorf("NotBefore = %v, want %v", jobs[0].NotBefore, notBefore)
	}
}

// TestMarkJobFailedBouncesWhenCancelled: MarkJobFailed's UPDATE is guarded so a
// job WantedSync cancelled underneath a failing search cycle is never
// resurrected CANCELLED->FAILED.
func TestMarkJobFailedBouncesWhenCancelled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 901, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	if err := s.MarkJobFailed(ctx, job.ID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}
	if got := jobStateForStore(t, s, job.ID); got != core.StateCancelled {
		t.Errorf("job must stay CANCELLED, got %v", got)
	}
}

func TestSetJobTrackBand(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 400, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if job.MinTrackCount != 0 || job.MaxTrackCount != 0 {
		t.Fatalf("fresh job band = (%d,%d), want (0,0)", job.MinTrackCount, job.MaxTrackCount)
	}

	if err := s.SetJobTrackBand(ctx, job.ID, 10, 12); err != nil {
		t.Fatalf("SetJobTrackBand: %v", err)
	}

	jobs, err := s.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].MinTrackCount != 10 || jobs[0].MaxTrackCount != 12 {
		t.Errorf("band = (%d,%d), want (10,12)", jobs[0].MinTrackCount, jobs[0].MaxTrackCount)
	}
}

func TestParkJobForCandidate(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	t.Run("parks job and terminalizes transfer", func(t *testing.T) {
		s := newTestStore(t)
		ctx := context.Background()
		jobID, candID, transferID := seedParkJobFixture(t, s, 500, now)

		_, changed, err := s.ParkJobForCandidate(ctx, transferID, candID, core.TransferErrored, 25, 100, now.Add(time.Minute))
		if err != nil {
			t.Fatalf("ParkJobForCandidate: %v", err)
		}
		if !changed {
			t.Fatal("expected ParkJobForCandidate to return true for a DOWNLOADING job")
		}
		view, found, err := s.JobWithTransfer(ctx, jobID)
		if err != nil || !found {
			t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
		}
		if view.Job.State != core.StateParked {
			t.Errorf("job state = %q, want PARKED", view.Job.State)
		}
		tr := transferForParkFixture(t, s, candID)
		if tr.State != core.TransferErrored || tr.BytesDone != 25 || tr.BytesTotal != 100 {
			t.Errorf("transfer = state %q progress %d/%d, want ERRORED 25/100", tr.State, tr.BytesDone, tr.BytesTotal)
		}

		// The transfer is already terminal, so a stale second call is a quiet no-op.
		_, changed, err = s.ParkJobForCandidate(ctx, transferID, candID, core.TransferErrored, 30, 100, now.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("ParkJobForCandidate (already terminal): %v", err)
		}
		if changed {
			t.Error("expected ParkJobForCandidate to no-op for an already-terminal transfer")
		}
	})

	t.Run("job race commits errored transfer without parking", func(t *testing.T) {
		s := newTestStore(t)
		ctx := context.Background()
		jobID, candID, transferID := seedParkJobFixture(t, s, 501, now)
		if _, err := s.AdvanceJobStateFrom(ctx, jobID, core.StateDownloading, core.StateCancelled, now.Add(time.Minute)); err != nil {
			t.Fatalf("cancel job: %v", err)
		}

		_, changed, err := s.ParkJobForCandidate(ctx, transferID, candID, core.TransferErrored, 25, 100, now.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("ParkJobForCandidate: %v", err)
		}
		if changed {
			t.Error("expected guarded PARKED transition to no-op after cancellation")
		}
		if got := jobStateForStore(t, s, jobID); got != core.StateCancelled {
			t.Errorf("job state = %q, want CANCELLED", got)
		}
		if got := transferForParkFixture(t, s, candID).State; got != core.TransferErrored {
			t.Errorf("transfer state = %q, want ERRORED", got)
		}
	})
}

func TestParkJobForCandidateLocksJobBeforeTransferWhenCancellationOverlaps(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	jobID, candID, transferID := seedParkJobFixture(t, s, 503, now)

	// Hold an advisory lock from a separate transaction. The AFTER trigger
	// stops parking only after its transfer UPDATE owns the transfer row, which
	// gives the test a deterministic point at which to inspect the earlier job
	// lock and start cancellation without timing sleeps.
	lockTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin advisory lock transaction: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback() })
	if _, err := lockTx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(243, (
			SELECT oid::integer FROM pg_database WHERE datname = current_database()
		))`); err != nil {
		t.Fatalf("acquire advisory lock: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE FUNCTION block_park_transfer() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(243, (
				SELECT oid::integer FROM pg_database WHERE datname = current_database()
			));
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER block_park_transfer AFTER UPDATE ON transfers
			FOR EACH ROW WHEN (NEW.state = 'ERRORED') EXECUTE FUNCTION block_park_transfer()`); err != nil {
		t.Fatalf("install blocking trigger: %v", err)
	}

	type parkResult struct {
		changed bool
		err     error
	}
	parkDone := make(chan parkResult, 1)
	go func() {
		_, changed, err := s.ParkJobForCandidate(ctx, transferID, candID, core.TransferErrored, 25, 100, now.Add(time.Minute))
		parkDone <- parkResult{changed: changed, err: err}
	}()
	waitForStoreQueryWait(t, s, `%UPDATE transfers SET state = $1, bytes_done%`, "advisory")

	probeTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin job-lock probe: %v", err)
	}
	var state string
	err = probeTx.QueryRowContext(ctx,
		`SELECT state FROM album_jobs WHERE id = $1 FOR UPDATE NOWAIT`, jobID).Scan(&state)
	_ = probeTx.Rollback()
	jobLockedFirst := false
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("probe job lock: %v", err)
		}
		jobLockedFirst = true
	}

	type cancellationResult struct {
		found bool
		err   error
	}
	cancelDone := make(chan cancellationResult, 1)
	go func() {
		_, found, err := s.CancelJob(ctx, jobID, now.Add(2*time.Minute))
		cancelDone <- cancellationResult{found: found, err: err}
	}()
	waitForStoreQueryWait(t, s, `%FOR UPDATE%`, "")

	if err := lockTx.Rollback(); err != nil {
		t.Fatalf("release advisory lock: %v", err)
	}

	var parked parkResult
	select {
	case parked = <-parkDone:
	case <-ctx.Done():
		t.Fatalf("parking did not finish: %v", ctx.Err())
	}
	if parked.err != nil {
		t.Errorf("ParkJobForCandidate: %v", parked.err)
	} else if !parked.changed {
		t.Error("ParkJobForCandidate did not park the DOWNLOADING job")
	}

	var cancelled cancellationResult
	select {
	case cancelled = <-cancelDone:
	case <-ctx.Done():
		t.Fatalf("cancellation did not finish: %v", ctx.Err())
	}
	if cancelled.err != nil {
		t.Errorf("CancelJob: %v", cancelled.err)
	} else if !cancelled.found {
		t.Error("CancelJob did not find the parked job")
	}
	if !jobLockedFirst {
		t.Error("parking reached the transfer update without locking the owning job first")
	}
	if got := jobStateForStore(t, s, jobID); got != core.StateCancelled {
		t.Errorf("job state = %q, want CANCELLED", got)
	}
	if got := transferForParkFixture(t, s, candID).State; got != core.TransferErrored {
		t.Errorf("transfer state = %q, want ERRORED", got)
	}
}

func waitForStoreQueryWait(t *testing.T, s *Store, queryPattern, waitEvent string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		var waiting bool
		err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid != pg_backend_pid()
			  AND query LIKE $1
			  AND wait_event_type = 'Lock'
			  AND ($2 = '' OR wait_event = $2)
		)`, queryPattern, waitEvent).Scan(&waiting)
		if err != nil {
			t.Fatalf("inspect blocked store query: %v", err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("query %q did not reach database lock wait: %v", queryPattern, ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestParkJobForCandidateRollsBackOnEitherWriteFailure(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name    string
		trigger string
	}{
		{
			name: "transfer update failure",
			trigger: `CREATE FUNCTION fail_park_transfer() RETURNS trigger AS $$
				BEGIN RAISE EXCEPTION 'injected transfer failure'; END $$ LANGUAGE plpgsql;
				CREATE TRIGGER fail_park_transfer BEFORE UPDATE ON transfers
				FOR EACH ROW WHEN (NEW.state = 'ERRORED') EXECUTE FUNCTION fail_park_transfer()`,
		},
		{
			name: "job update failure",
			trigger: `CREATE FUNCTION fail_park_job() RETURNS trigger AS $$
				BEGIN RAISE EXCEPTION 'injected job failure'; END $$ LANGUAGE plpgsql;
				CREATE TRIGGER fail_park_job BEFORE UPDATE ON album_jobs
				FOR EACH ROW WHEN (NEW.state = 'PARKED') EXECUTE FUNCTION fail_park_job()`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			jobID, candID, transferID := seedParkJobFixture(t, s, 502, now)
			if _, err := s.db.ExecContext(ctx, tc.trigger); err != nil {
				t.Fatalf("install failure trigger: %v", err)
			}

			if _, _, err := s.ParkJobForCandidate(ctx, transferID, candID, core.TransferErrored, 25, 100, now.Add(time.Minute)); err == nil {
				t.Fatal("ParkJobForCandidate unexpectedly succeeded")
			}
			if got := jobStateForStore(t, s, jobID); got != core.StateDownloading {
				t.Errorf("job state after rollback = %q, want DOWNLOADING", got)
			}
			tr := transferForParkFixture(t, s, candID)
			if tr.State != core.TransferInProgress || tr.BytesDone != 10 || tr.BytesTotal != 100 {
				t.Errorf("transfer after rollback = state %q progress %d/%d, want IN_PROGRESS 10/100", tr.State, tr.BytesDone, tr.BytesTotal)
			}
		})
	}
}

func seedParkJobFixture(t *testing.T, s *Store, albumID int64, now time.Time) (jobID, candidateID, transferID int64) {
	t.Helper()
	ctx := context.Background()
	job, err := s.UpsertWantedJob(ctx, albumID, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "peer_one", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	transferID, ok, err := s.RecordEnqueueIntent(ctx, cand.ID, "peer_one", "track.flac", now.Add(time.Hour), now)
	if err != nil || !ok {
		t.Fatalf("RecordEnqueueIntent: ok=%v (%v)", ok, err)
	}
	if err := s.UpdateTransferProgress(ctx, transferID, core.TransferInProgress, 10, 100, now); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}
	return job.ID, cand.ID, transferID
}

func transferForParkFixture(t *testing.T, s *Store, candidateID int64) core.Transfer {
	t.Helper()
	transfers, err := s.TransfersForCandidate(context.Background(), candidateID)
	if err != nil {
		t.Fatalf("TransfersForCandidate: %v", err)
	}
	if len(transfers) != 1 {
		t.Fatalf("transfers = %d, want 1", len(transfers))
	}
	return transfers[0]
}
