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

// helperActivate seeds a SELECTING job with one activated candidate, leaving
// the job DOWNLOADING and the candidate ACTIVE - the exact precondition
// FailCandidateAndAdvance/SucceedCandidateAndAdvance expect.
func helperActivate(t *testing.T, s *Store, albumID int64, now time.Time) (jobID, candID int64) {
	t.Helper()
	ctx := context.Background()
	job, err := s.UpsertWantedJob(ctx, albumID, now)
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
	ok, err := s.ActivateCandidate(ctx, cand.ID, job.ID, 100, now)
	if err != nil || !ok {
		t.Fatalf("ActivateCandidate: %v ok=%v", err, ok)
	}
	return job.ID, cand.ID
}

// TestFailCandidateAndAdvanceHappyPath: an ACTIVE candidate is failed and its
// job advanced in one transaction. The candidate leaves ACTIVE and the job
// reaches the target state.
func TestFailCandidateAndAdvanceHappyPath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivate(t, s, 500, now)

	transitioned, err := s.FailCandidateAndAdvance(ctx, candID, jobID, "transfer failed", core.StateDownloading, core.StateSelecting, now)
	if err != nil {
		t.Fatalf("FailCandidateAndAdvance: %v", err)
	}
	if !transitioned {
		t.Fatal("expected transitioned=true on the happy path")
	}
	if _, found, err := s.ActiveCandidate(ctx, jobID); err != nil || found {
		t.Errorf("candidate should no longer be ACTIVE, found=%v (%v)", found, err)
	}
	jobs, err := s.RunnableJobsInState(ctx, core.StateSelecting, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != jobID {
		t.Fatalf("expected job in SELECTING, got %+v (%v)", jobs, err)
	}
}

// TestFailCandidateAndAdvanceJobAlreadyLeftState is the atomicity guarantee
// that replaces the old wedge: if the job left its from-state underneath us
// (e.g. WantedSync cancelled it), the whole transaction rolls back - the
// candidate is NOT failed, so no job is ever left in DOWNLOADING/IMPORTING with
// no ACTIVE candidate.
func TestFailCandidateAndAdvanceJobAlreadyLeftState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivate(t, s, 501, now)

	// The job leaves DOWNLOADING (cancelled) between our read and the write.
	if err := s.AdvanceJobState(ctx, jobID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	transitioned, err := s.FailCandidateAndAdvance(ctx, candID, jobID, "transfer failed", core.StateDownloading, core.StateSelecting, now)
	if err != nil {
		t.Fatalf("FailCandidateAndAdvance: %v", err)
	}
	if transitioned {
		t.Fatal("expected transitioned=false when the job already left its from-state")
	}
	// BOTH writes rolled back: the candidate must still be ACTIVE.
	if _, found, err := s.ActiveCandidate(ctx, jobID); err != nil || !found {
		t.Errorf("candidate must stay ACTIVE when the tx rolled back, found=%v (%v)", found, err)
	}
}

// TestFailCandidateAndAdvanceCandidateNotActive: if the candidate is no longer
// ACTIVE (already processed), nothing is written and the job is untouched.
func TestFailCandidateAndAdvanceCandidateNotActive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivate(t, s, 502, now)

	// The candidate is already FAILED (double-processing guard).
	if err := s.FailCandidate(ctx, candID, "already", now); err != nil {
		t.Fatalf("FailCandidate: %v", err)
	}

	transitioned, err := s.FailCandidateAndAdvance(ctx, candID, jobID, "transfer failed", core.StateDownloading, core.StateSelecting, now)
	if err != nil {
		t.Fatalf("FailCandidateAndAdvance: %v", err)
	}
	if transitioned {
		t.Fatal("expected transitioned=false when the candidate is not ACTIVE")
	}
	// Job must stay DOWNLOADING (not advanced).
	jobs, err := s.RunnableJobsInState(ctx, core.StateDownloading, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != jobID {
		t.Fatalf("expected job still DOWNLOADING, got %+v (%v)", jobs, err)
	}
}

// TestSucceedCandidateAndAdvanceHappyPath: an ACTIVE candidate is succeeded and
// its job advanced to DONE in one transaction.
func TestSucceedCandidateAndAdvanceHappyPath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivate(t, s, 503, now)
	// Move the job to IMPORTING so DONE is a legal transition to assert.
	if err := s.AdvanceJobState(ctx, jobID, core.StateImporting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	transitioned, err := s.SucceedCandidateAndAdvance(ctx, candID, jobID, core.StateImporting, core.StateDone, now)
	if err != nil {
		t.Fatalf("SucceedCandidateAndAdvance: %v", err)
	}
	if !transitioned {
		t.Fatal("expected transitioned=true on the happy path")
	}
	if _, found, err := s.ActiveCandidate(ctx, jobID); err != nil || found {
		t.Errorf("candidate should no longer be ACTIVE, found=%v (%v)", found, err)
	}
	if got := jobStateForStore(t, s, jobID); got != core.StateDone {
		t.Errorf("job state = %v, want DONE", got)
	}
}

// TestSucceedCandidateAndAdvanceJobAlreadyLeftState: same rollback guarantee as
// the fail path - the candidate is not succeeded if the job left its from-state.
func TestSucceedCandidateAndAdvanceJobAlreadyLeftState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivate(t, s, 504, now)
	if err := s.AdvanceJobState(ctx, jobID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	transitioned, err := s.SucceedCandidateAndAdvance(ctx, candID, jobID, core.StateImporting, core.StateDone, now)
	if err != nil {
		t.Fatalf("SucceedCandidateAndAdvance: %v", err)
	}
	if transitioned {
		t.Fatal("expected transitioned=false when the job already left its from-state")
	}
	if _, found, err := s.ActiveCandidate(ctx, jobID); err != nil || !found {
		t.Errorf("candidate must stay ACTIVE when the tx rolled back, found=%v (%v)", found, err)
	}
}

// jobStateForStore scans every pipeline state to find a job's current state,
// since the store has no get-by-id-any-state helper.
func jobStateForStore(t *testing.T, s *Store, jobID int64) core.AlbumJobState {
	t.Helper()
	ctx := context.Background()
	farFuture := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, state := range []core.AlbumJobState{
		core.StateWanted, core.StateSelecting, core.StateDownloading,
		core.StateImporting, core.StateDone, core.StateFailed, core.StateCancelled,
	} {
		jobs, err := s.RunnableJobsInState(ctx, state, farFuture, 100)
		if err != nil {
			t.Fatalf("RunnableJobsInState(%v): %v", state, err)
		}
		for _, j := range jobs {
			if j.ID == jobID {
				return state
			}
		}
	}
	t.Fatalf("job %d not found in any state", jobID)
	return ""
}

func TestResetJobToWantedDeletesCandidates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 400, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	// ResetJobToWanted is only ever called on a SELECTING job (see its callers).
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{
		{Username: "alice", Score: 1.0},
		{Username: "bob", Score: 2.0},
	}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}

	// A transfer left behind by the wiped cycle must go too: transfers has a
	// global UNIQUE(username, filename), so a leftover row would collide when
	// a later cycle re-attempts the same peer+file.
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	if err := s.RecordPendingTransfer(ctx, cand.ID, cand.Username, "Music\\a.flac", 123, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}

	notBefore := now.Add(time.Hour)
	if err := s.ResetJobToWanted(ctx, job.ID, core.StateSelecting, 3, &notBefore, now); err != nil {
		t.Fatalf("ResetJobToWanted: %v", err)
	}

	if trs, err := s.TransfersForCandidate(ctx, cand.ID); err != nil || len(trs) != 0 {
		t.Fatalf("expected zero transfers after ResetJobToWanted, got %d (%v)", len(trs), err)
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

// TestResetJobToWantedBouncesWhenCancelled: the single-writer invariant says a
// transition UPDATE must be conditional on the from-state. If WantedSync
// cancelled the job underneath us, ResetJobToWanted must NOT resurrect it to
// WANTED and must NOT delete its candidates/transfers - the whole tx rolls back.
func TestResetJobToWantedBouncesWhenCancelled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 600, now)
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
	if err := s.RecordPendingTransfer(ctx, cand.ID, cand.Username, "Music\\a.flac", 123, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}

	// WantedSync cancels the job while we still think it's SELECTING.
	if err := s.AdvanceJobState(ctx, job.ID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	// The reset targets the stale SELECTING state; it must bounce (no error).
	if err := s.ResetJobToWanted(ctx, job.ID, core.StateSelecting, 3, nil, now); err != nil {
		t.Fatalf("ResetJobToWanted: %v", err)
	}

	if got := jobStateForStore(t, s, job.ID); got != core.StateCancelled {
		t.Errorf("job must stay CANCELLED, got %v", got)
	}
	if trs, err := s.TransfersForCandidate(ctx, cand.ID); err != nil || len(trs) != 1 {
		t.Errorf("candidate transfers must NOT be deleted on a bounced reset, got %d (%v)", len(trs), err)
	}
}

// TestUpsertWantedJobReentersCancelled: a previously-CANCELLED job whose album
// reappears on the wanted list must re-enter WANTED with a clean slate
// (retries=0, not_before/failed_at cleared, candidates+transfers wiped).
func TestUpsertWantedJobReentersCancelled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 700, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	if err := s.SetJobBackoff(ctx, job.ID, 4, now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetJobBackoff: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "alice", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}
	if err := s.RecordPendingTransfer(ctx, cand.ID, cand.Username, "Music\\a.flac", 123, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	// Album back on the wanted list: UpsertWantedJob must re-enter it.
	reentered, err := s.UpsertWantedJob(ctx, 700, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("UpsertWantedJob (re-enter): %v", err)
	}
	if reentered.State != core.StateWanted {
		t.Errorf("re-entered job state = %v, want WANTED", reentered.State)
	}
	if reentered.Retries != 0 {
		t.Errorf("re-entered job retries = %d, want 0", reentered.Retries)
	}
	if reentered.NotBefore != nil {
		t.Errorf("re-entered job not_before = %v, want nil", reentered.NotBefore)
	}
	if reentered.FailedAt != nil {
		t.Errorf("re-entered job failed_at = %v, want nil", reentered.FailedAt)
	}
	if _, found, err := s.NextNewCandidate(ctx, job.ID); err != nil || found {
		t.Errorf("re-entered job must have its candidate cache wiped, found=%v (%v)", found, err)
	}
	if trs, err := s.TransfersForCandidate(ctx, cand.ID); err != nil || len(trs) != 0 {
		t.Errorf("re-entered job must have its transfers wiped, got %d (%v)", len(trs), err)
	}
}

// TestUpsertWantedJobDoesNotDisturbActiveJob: the re-enter path is gated on
// state='CANCELLED', so an in-flight (e.g. DOWNLOADING) job whose album is
// still wanted is left completely untouched.
func TestUpsertWantedJobDoesNotDisturbActiveJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 701, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.SetJobBackoff(ctx, job.ID, 2, now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetJobBackoff: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	again, err := s.UpsertWantedJob(ctx, 701, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("UpsertWantedJob (again): %v", err)
	}
	if again.State != core.StateDownloading {
		t.Errorf("active job state = %v, want DOWNLOADING (untouched)", again.State)
	}
	if again.Retries != 2 {
		t.Errorf("active job retries = %d, want 2 (untouched)", again.Retries)
	}
}
