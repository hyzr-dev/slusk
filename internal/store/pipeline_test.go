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

	if err := s.FailAttempt(ctx, attemptID, "timeout", now.Add(10*time.Minute), now); err != nil {
		t.Fatalf("FailAttempt: %v", err)
	}
	after, _ := s.AttemptsForJob(ctx, job.ID)
	if after[0].State != "FAILED" || after[0].FailReason != "timeout" {
		t.Errorf("attempt not marked failed: %+v", after[0])
	}
	if err := s.IncrementCandidatesTried(ctx, job.ID, now); err != nil {
		t.Fatalf("IncrementCandidatesTried: %v", err)
	}
	jobs, _ := s.JobsInState(ctx, core.StateDiscovered, 10)
	if len(jobs) != 1 || jobs[0].CandidatesTried != 1 {
		t.Errorf("candidates_tried not incremented: %+v", jobs)
	}
}
