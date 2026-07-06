package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
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
	_, _ = s.RecordEnqueueIntent(ctx, candidate.ID, "bob", "f.flac", now.Add(time.Hour), now)

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
