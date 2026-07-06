package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestJobsInStateAndCooldown(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertDiscoveredJob(ctx, 100, now)

	inState, err := s.JobsInState(ctx, core.StateDiscovered, 10)
	if err != nil {
		t.Fatalf("JobsInState: %v", err)
	}
	if len(inState) != 1 || inState[0].ID != job.ID {
		t.Fatalf("expected the discovered job, got %+v", inState)
	}

	// Put it in cooldown with next_attempt in the past -> due.
	if err := s.SetJobCooldown(ctx, job.ID, now.Add(-time.Minute), now); err != nil {
		t.Fatalf("SetJobCooldown: %v", err)
	}
	due, err := s.DueCooldownJobs(ctx, now, 10)
	if err != nil {
		t.Fatalf("DueCooldownJobs: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due cooldown job, got %d", len(due))
	}
	// And not due if next_attempt is in the future.
	_ = s.SetJobCooldown(ctx, job.ID, now.Add(time.Hour), now)
	future, _ := s.DueCooldownJobs(ctx, now, 10)
	if len(future) != 0 {
		t.Errorf("job with future next_attempt should not be due")
	}
}

func TestJobsInStateOrdersDiscoveredByReleaseDateDesc(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	older, _ := s.UpsertDiscoveredJob(ctx, 400, now)
	if err := s.UpdateJobMetadata(ctx, older.ID, "Older", "Artist", "2020-01-01", 0, now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}
	newest, _ := s.UpsertDiscoveredJob(ctx, 401, now)
	if err := s.UpdateJobMetadata(ctx, newest.ID, "Newest", "Artist", "2026-06-01", 0, now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}
	blank, _ := s.UpsertDiscoveredJob(ctx, 402, now)
	middle, _ := s.UpsertDiscoveredJob(ctx, 403, now)
	if err := s.UpdateJobMetadata(ctx, middle.ID, "Middle", "Artist", "2023-05-15", 0, now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}

	jobs, err := s.JobsInState(ctx, core.StateDiscovered, 10)
	if err != nil {
		t.Fatalf("JobsInState: %v", err)
	}
	if len(jobs) != 4 {
		t.Fatalf("expected 4 discovered jobs, got %d", len(jobs))
	}
	// Newest release date first, then descending; blank release_date sorts
	// last (empty string is lexicographically smallest) and falls back to
	// oldest-updated-first among ties.
	want := []int64{newest.ID, middle.ID, older.ID, blank.ID}
	for i, id := range want {
		if jobs[i].ID != id {
			t.Errorf("jobs[%d].ID = %d, want %d (order: %+v)", i, jobs[i].ID, id, jobs)
		}
	}
}

func TestCountJobsInStates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if _, err := s.UpsertDiscoveredJob(ctx, 300, now); err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	downloading, _ := s.UpsertDiscoveredJob(ctx, 301, now)
	if err := s.AdvanceJobState(ctx, downloading.ID, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	verifying, _ := s.UpsertDiscoveredJob(ctx, 302, now)
	if err := s.AdvanceJobState(ctx, verifying.ID, core.StateVerifying, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	count, err := s.CountJobsInStates(ctx, core.StateDownloading, core.StateVerifying)
	if err != nil {
		t.Fatalf("CountJobsInStates: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 jobs in DOWNLOADING/VERIFYING, got %d", count)
	}

	countDiscovered, err := s.CountJobsInStates(ctx, core.StateDiscovered)
	if err != nil {
		t.Fatalf("CountJobsInStates: %v", err)
	}
	if countDiscovered != 1 {
		t.Errorf("expected 1 job in DISCOVERED, got %d", countDiscovered)
	}
}

func TestAttemptsAndTransfersForJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertDiscoveredJob(ctx, 200, now)
	attemptID, _ := s.CreateAttempt(ctx, job.ID, "bob", 2.0, now)
	_, _ = s.RecordEnqueueIntent(ctx, attemptID, "bob", "f.flac", now.Add(time.Hour), now)

	attempts, err := s.AttemptsForJob(ctx, job.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Username != "bob" {
		t.Fatalf("AttemptsForJob: %v %+v", err, attempts)
	}
	transfers, err := s.TransfersForAttempt(ctx, attemptID)
	if err != nil || len(transfers) != 1 {
		t.Fatalf("TransfersForAttempt: %v %+v", err, transfers)
	}

	failedAt := now.Add(10 * time.Minute)
	if err := s.FailAttempt(ctx, attemptID, "timeout", now.Add(10*time.Minute), failedAt); err != nil {
		t.Fatalf("FailAttempt: %v", err)
	}
	after, _ := s.AttemptsForJob(ctx, job.ID)
	if after[0].State != "FAILED" || after[0].FailReason != "timeout" {
		t.Errorf("attempt not marked failed: %+v", after[0])
	}
	if !after[0].UpdatedAt.Equal(failedAt) {
		t.Errorf("UpdatedAt = %v, want %v after FailAttempt", after[0].UpdatedAt, failedAt)
	}
	if err := s.IncrementCandidatesTried(ctx, job.ID, now); err != nil {
		t.Fatalf("IncrementCandidatesTried: %v", err)
	}
	jobs, _ := s.JobsInState(ctx, core.StateDiscovered, 10)
	if len(jobs) != 1 || jobs[0].CandidatesTried != 1 {
		t.Errorf("candidates_tried not incremented: %+v", jobs)
	}
}

func TestFailAndSucceedAttemptBumpUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertDiscoveredJob(ctx, 201, now)

	// FailAttempt sets updated_at to the given now, not created_at.
	failID, _ := s.CreateAttempt(ctx, job.ID, "alice", 1.0, now)
	failedAt := now.Add(5 * time.Minute)
	if err := s.FailAttempt(ctx, failID, "timeout", failedAt.Add(time.Hour), failedAt); err != nil {
		t.Fatalf("FailAttempt: %v", err)
	}
	attempts, err := s.AttemptsForJob(ctx, job.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("AttemptsForJob: %v %+v", err, attempts)
	}
	if !attempts[0].CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want unchanged %v", attempts[0].CreatedAt, now)
	}
	if !attempts[0].UpdatedAt.Equal(failedAt) {
		t.Errorf("UpdatedAt = %v, want %v after FailAttempt", attempts[0].UpdatedAt, failedAt)
	}

	// SucceedAttempt sets updated_at to the given now, not created_at.
	succeedID, _ := s.CreateAttempt(ctx, job.ID, "bob", 2.0, now)
	succeededAt := now.Add(10 * time.Minute)
	if err := s.SucceedAttempt(ctx, succeedID, succeededAt); err != nil {
		t.Fatalf("SucceedAttempt: %v", err)
	}
	attempts, err = s.AttemptsForJob(ctx, job.ID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("AttemptsForJob: %v %+v", err, attempts)
	}
	var succeeded core.CandidateAttempt
	for _, a := range attempts {
		if a.ID == succeedID {
			succeeded = a
		}
	}
	if succeeded.State != "SUCCEEDED" {
		t.Errorf("State = %q, want SUCCEEDED", succeeded.State)
	}
	if !succeeded.UpdatedAt.Equal(succeededAt) {
		t.Errorf("UpdatedAt = %v, want %v after SucceedAttempt", succeeded.UpdatedAt, succeededAt)
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
