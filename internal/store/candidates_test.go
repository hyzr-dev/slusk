package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestCandidateLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 100, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	cands := []NewCandidate{
		{Username: "alice", Score: 1.0, Files: []core.CandidateFile{{Filename: "a.flac", Size: 111}}},
		{Username: "carol", Score: 3.0, Files: []core.CandidateFile{{Filename: "c1.flac", Size: 222}, {Filename: "c2.flac", Size: 333}}},
		{Username: "bob", Score: 2.0, Files: []core.CandidateFile{{Filename: "b.flac", Size: 444}}},
	}
	if err := s.InsertCandidates(ctx, job.ID, cands, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}

	top, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}
	if top.Username != "carol" || top.Score != 3.0 {
		t.Fatalf("expected carol (score 3.0) first, got %+v", top)
	}

	ok, err := s.ActivateCandidate(ctx, top.ID, job.ID, 5, now)
	if err != nil {
		t.Fatalf("ActivateCandidate: %v", err)
	}
	if !ok {
		t.Fatal("ActivateCandidate: expected true (cap not reached, job in SELECTING)")
	}

	jobs, err := s.RunnableJobsInState(ctx, core.StateDownloading, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job now DOWNLOADING, got %v (%v)", jobs, err)
	}

	active, found, err := s.ActiveCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	if active.State != core.CandidateActive {
		t.Errorf("candidate state = %q, want ACTIVE", active.State)
	}
	if len(active.Files) != 2 || active.Files[0].Filename != "c1.flac" || active.Files[0].Size != 222 || active.Files[1].Filename != "c2.flac" || active.Files[1].Size != 333 {
		t.Errorf("Files did not round-trip through JSONB intact: %+v", active.Files)
	}

	if err := s.FailCandidate(ctx, active.ID, "timeout", now); err != nil {
		t.Fatalf("FailCandidate: %v", err)
	}
	next, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate after fail: %v found=%v", err, found)
	}
	if next.Username != "bob" || next.Score != 2.0 {
		t.Fatalf("expected bob (score 2.0) next, got %+v", next)
	}
}

// TestInsertCandidatesResetsSearchCycle verifies InsertCandidates clears the
// job's backoff state in the same transaction as the insert: a successful
// search starts a fresh cycle, since retries/not_before track search
// failures, not per-candidate failures.
func TestInsertCandidatesResetsSearchCycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 101, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	notBefore := now.Add(time.Hour)
	if err := s.SetJobBackoff(ctx, job.ID, 3, notBefore, now); err != nil {
		t.Fatalf("SetJobBackoff: %v", err)
	}

	cands := []NewCandidate{{Username: "alice", Score: 1.0, Files: []core.CandidateFile{{Filename: "a.flac", Size: 1}}}}
	if err := s.InsertCandidates(ctx, job.ID, cands, now.Add(time.Minute)); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}

	jobs, err := s.RunnableJobsInState(ctx, core.StateWanted, now.Add(time.Minute), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("RunnableJobsInState: %v %+v", err, jobs)
	}
	if jobs[0].Retries != 0 {
		t.Errorf("Retries = %d, want 0 after InsertCandidates", jobs[0].Retries)
	}
	if jobs[0].NotBefore != nil {
		t.Errorf("NotBefore = %v, want nil after InsertCandidates", jobs[0].NotBefore)
	}
}

func TestActivateCandidateRespectsMaxActive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// Two jobs already occupying the "active" slots (DOWNLOADING).
	for _, albumID := range []int64{200, 201} {
		j, err := s.UpsertWantedJob(ctx, albumID, now)
		if err != nil {
			t.Fatalf("UpsertWantedJob: %v", err)
		}
		if err := s.AdvanceJobState(ctx, j.ID, core.StateDownloading, now); err != nil {
			t.Fatalf("AdvanceJobState: %v", err)
		}
	}

	job, err := s.UpsertWantedJob(ctx, 202, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "alice", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}

	ok, err := s.ActivateCandidate(ctx, cand.ID, job.ID, 2, now)
	if err != nil {
		t.Fatalf("ActivateCandidate: %v", err)
	}
	if ok {
		t.Fatal("ActivateCandidate: expected false when maxActive cap is already reached")
	}

	jobs, err := s.RunnableJobsInState(ctx, core.StateSelecting, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("job should still be SELECTING, got %v (%v)", jobs, err)
	}
	stillNew, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found || stillNew.ID != cand.ID {
		t.Fatalf("candidate should still be NEW, got %+v found=%v (%v)", stillNew, found, err)
	}
}

func TestActivateCandidateBouncesWhenJobLeftSelecting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 300, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "alice", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}

	// The job left SELECTING (e.g. WantedSync cancelled it) between the read
	// and the activation attempt.
	if err := s.AdvanceJobState(ctx, job.ID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	ok, err := s.ActivateCandidate(ctx, cand.ID, job.ID, 5, now)
	if err != nil {
		t.Fatalf("ActivateCandidate: %v", err)
	}
	if ok {
		t.Fatal("ActivateCandidate: expected false when job left SELECTING")
	}
}

func TestResetJobToWantedDeletesCandidates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 400, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{
		{Username: "alice", Score: 1.0},
		{Username: "bob", Score: 2.0},
	}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}

	notBefore := now.Add(time.Hour)
	if err := s.ResetJobToWanted(ctx, job.ID, 3, &notBefore, now); err != nil {
		t.Fatalf("ResetJobToWanted: %v", err)
	}

	jobs, err := s.RunnableJobsInState(ctx, core.StateWanted, now.Add(2*time.Hour), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("RunnableJobsInState: %v %+v", err, jobs)
	}
	if jobs[0].Retries != 3 {
		t.Errorf("Retries = %d, want 3", jobs[0].Retries)
	}
	if jobs[0].NotBefore == nil || !jobs[0].NotBefore.Equal(notBefore) {
		t.Errorf("NotBefore = %v, want %v", jobs[0].NotBefore, notBefore)
	}

	if _, found, err := s.NextNewCandidate(ctx, job.ID); err != nil || found {
		t.Fatalf("expected zero candidates after ResetJobToWanted, found=%v (%v)", found, err)
	}
}
