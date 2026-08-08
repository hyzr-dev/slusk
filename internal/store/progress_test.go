package store

import (
	"context"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// stateProgress finds one state's entry in a snapshot, failing the test when
// the state is absent.
func stateProgress(t *testing.T, p JobProgress, state core.AlbumJobState) JobStateProgress {
	t.Helper()
	for _, sp := range p.States {
		if sp.State == state {
			return sp
		}
	}
	t.Fatalf("state %s missing from snapshot %+v", state, p.States)
	return JobStateProgress{}
}

// hasState reports whether a state appears in the snapshot at all.
func hasState(p JobProgress, state core.AlbumJobState) bool {
	for _, sp := range p.States {
		if sp.State == state {
			return true
		}
	}
	return false
}

func TestJobProgressCountsAndOldestUpdatePerState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// Two DOWNLOADING jobs touched at different times: the older one must win.
	oldDownloading, _ := s.UpsertWantedJob(ctx, 700, base)
	if err := s.AdvanceJobState(ctx, oldDownloading.ID, core.StateDownloading, base); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	newDownloading, _ := s.UpsertWantedJob(ctx, 701, base)
	if err := s.AdvanceJobState(ctx, newDownloading.ID, core.StateDownloading, base.Add(3*time.Hour)); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	// One IMPORTING job, newer than either.
	importing, _ := s.UpsertWantedJob(ctx, 702, base)
	if err := s.AdvanceJobState(ctx, importing.ID, core.StateImporting, base.Add(5*time.Hour)); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	got, err := s.JobProgress(ctx)
	if err != nil {
		t.Fatalf("JobProgress: %v", err)
	}

	dl := stateProgress(t, got, core.StateDownloading)
	if dl.Count != 2 {
		t.Errorf("DOWNLOADING count = %d, want 2", dl.Count)
	}
	if !dl.OldestUpdate.Equal(base) {
		t.Errorf("DOWNLOADING oldest update = %s, want %s", dl.OldestUpdate, base)
	}

	imp := stateProgress(t, got, core.StateImporting)
	if imp.Count != 1 {
		t.Errorf("IMPORTING count = %d, want 1", imp.Count)
	}
	if !imp.OldestUpdate.Equal(base.Add(5 * time.Hour)) {
		t.Errorf("IMPORTING oldest update = %s, want %s", imp.OldestUpdate, base.Add(5*time.Hour))
	}
}

func TestJobProgressOmitsStatesWithNoJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 710, now)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	got, err := s.JobProgress(ctx)
	if err != nil {
		t.Fatalf("JobProgress: %v", err)
	}
	if hasState(got, core.StateVerifying) {
		t.Errorf("VERIFYING has no jobs but appears in the snapshot: %+v", got.States)
	}

	// An empty table produces no entries at all rather than a row of zeroes:
	// absent is the honest encoding for "no jobs", zero would read as "fresh".
	if err := s.AdvanceJobState(ctx, job.ID, core.StateDone, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	got, err = s.JobProgress(ctx)
	if err != nil {
		t.Fatalf("JobProgress: %v", err)
	}
	if len(got.States) != 0 {
		t.Errorf("snapshot of an all-terminal table = %+v, want no entries", got.States)
	}
}

func TestJobProgressExcludesTerminalStates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	terminal := []core.AlbumJobState{
		core.StateDone,
		core.StateCompleted,
		core.StateFailed,
		core.StateCancelled,
		core.StateNotImported,
	}
	for i, state := range terminal {
		job, err := s.UpsertWantedJob(ctx, int64(720+i), now)
		if err != nil {
			t.Fatalf("UpsertWantedJob: %v", err)
		}
		if err := s.AdvanceJobState(ctx, job.ID, state, now); err != nil {
			t.Fatalf("AdvanceJobState(%s): %v", state, err)
		}
	}
	// One live job so the snapshot is not trivially empty.
	live, _ := s.UpsertWantedJob(ctx, 730, now)
	if err := s.AdvanceJobState(ctx, live.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	got, err := s.JobProgress(ctx)
	if err != nil {
		t.Fatalf("JobProgress: %v", err)
	}
	for _, state := range terminal {
		if hasState(got, state) {
			t.Errorf("terminal state %s appears in the snapshot", state)
		}
	}
	if !hasState(got, core.StateSelecting) {
		t.Errorf("live SELECTING job missing from snapshot %+v", got.States)
	}
}

// PARKED is neither runnable nor terminal: a parked job needs manual action, so
// it must stay visible rather than being filtered out with the end states.
func TestJobProgressIncludesParked(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 740, now)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateParked, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	got, err := s.JobProgress(ctx)
	if err != nil {
		t.Fatalf("JobProgress: %v", err)
	}
	if p := stateProgress(t, got, core.StateParked); p.Count != 1 {
		t.Errorf("PARKED count = %d, want 1", p.Count)
	}
}

func TestJobProgressCountsJobsWithoutActiveCandidate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertWantedJob(ctx, 750, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{
		{Username: "dana", Score: 1.0, Files: []core.CandidateFile{{Filename: "01 track.flac", Size: 111}}},
	}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}
	// Activation moves the job to DOWNLOADING with the candidate ACTIVE.
	ok, _, err := s.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, 5, now.Add(time.Hour), now)
	if err != nil || !ok {
		t.Fatalf("ActivateCandidateWithTransfers: ok=%v err=%v", ok, err)
	}

	got, err := s.JobProgress(ctx)
	if err != nil {
		t.Fatalf("JobProgress: %v", err)
	}
	if got.JobsWithoutActiveCandidate != 0 {
		t.Errorf("with an ACTIVE candidate: count = %d, want 0", got.JobsWithoutActiveCandidate)
	}

	// Fail the candidate without advancing the job: exactly the wedge shape
	// FailCandidateAndAdvance exists to make impossible.
	if err := s.FailCandidate(ctx, cand.ID, "peer vanished", now); err != nil {
		t.Fatalf("FailCandidate: %v", err)
	}

	got, err = s.JobProgress(ctx)
	if err != nil {
		t.Fatalf("JobProgress: %v", err)
	}
	if got.JobsWithoutActiveCandidate != 1 {
		t.Errorf("DOWNLOADING with no ACTIVE candidate: count = %d, want 1", got.JobsWithoutActiveCandidate)
	}
}

// Only DOWNLOADING and IMPORTING can be wedged this way: those are the two
// modules that skip a job with no ACTIVE candidate while it holds a slot.
func TestJobProgressWedgeCountIgnoresOtherStates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	for i, state := range []core.AlbumJobState{core.StateWanted, core.StateSelecting, core.StateParked} {
		job, err := s.UpsertWantedJob(ctx, int64(760+i), now)
		if err != nil {
			t.Fatalf("UpsertWantedJob: %v", err)
		}
		if err := s.AdvanceJobState(ctx, job.ID, state, now); err != nil {
			t.Fatalf("AdvanceJobState(%s): %v", state, err)
		}
	}

	got, err := s.JobProgress(ctx)
	if err != nil {
		t.Fatalf("JobProgress: %v", err)
	}
	if got.JobsWithoutActiveCandidate != 0 {
		t.Errorf("count = %d, want 0: no job is DOWNLOADING or IMPORTING", got.JobsWithoutActiveCandidate)
	}
}
