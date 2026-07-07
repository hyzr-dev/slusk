package pipeline

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// newImportingParams builds ImportingParams over a fresh store-backed
// fixture, with generous defaults each test can override before constructing
// an Importing.
func newImportingParams(t *testing.T, music *fakeMusic, peers *fakeSearcher) (ImportingParams, *store.Store) {
	t.Helper()
	st := newBackedStore(t)
	return ImportingParams{
		Store:                st,
		Music:                music,
		Peers:                peers,
		CompleteDir:          "/music/complete",
		MaxActive:            5,
		StuckAfter:           time.Hour,
		ImportConfirmTimeout: 3 * time.Minute,
		Interval:             30 * time.Second,
		Logger:               slog.New(slog.NewTextHandler(testDiscard{}, nil)),
	}, st
}

// seedImportingJob creates a WANTED job, activates one candidate with the
// given files, marks all its transfers COMPLETED, and advances the job
// straight to IMPORTING - the exact state Importing's verify phase expects
// (mirrors seedActiveCandidate's DOWNLOADING setup, one step further).
func seedImportingJob(t *testing.T, st *store.Store, albumID int64, username string, files []core.CandidateFile, now time.Time) (jobID, candID int64) {
	t.Helper()
	ctx := context.Background()
	jobID, candID = seedActiveCandidate(t, st, albumID, username, files, now)
	for _, f := range files {
		if _, err := st.RecordEnqueueIntent(ctx, candID, username, f.Filename, now.Add(time.Hour), now); err != nil {
			t.Fatalf("RecordEnqueueIntent: %v", err)
		}
	}
	transfers, err := st.TransfersForCandidate(ctx, candID)
	if err != nil {
		t.Fatalf("TransfersForCandidate: %v", err)
	}
	for _, tr := range transfers {
		if err := st.UpdateTransferProgress(ctx, tr.ID, core.TransferCompleted, tr.BytesTotal, tr.BytesTotal, now); err != nil {
			t.Fatalf("UpdateTransferProgress: %v", err)
		}
	}
	if err := st.AdvanceJobState(ctx, jobID, core.StateImporting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	return jobID, candID
}

// assertCandidateNoLongerActive asserts the candidate has left the ACTIVE
// state (store exposes no by-id lookup across every candidate state, so a
// terminal candidate is verified by absence from ActiveCandidate combined with
// the job's own terminal state, asserted separately by the caller).
func assertCandidateNoLongerActive(t *testing.T, st *store.Store, jobID int64) {
	t.Helper()
	if _, found, err := st.ActiveCandidate(context.Background(), jobID); err != nil || found {
		t.Errorf("candidate should no longer be ACTIVE, found=%v (%v)", found, err)
	}
}

// lastJobEvent returns the most recent audit-trail row for a job, so tests
// can assert the event type and detail wording an Importing phase recorded.
func lastJobEvent(t *testing.T, st *store.Store, jobID int64) core.JobEvent {
	t.Helper()
	events, err := st.JobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("expected at least one job event for job %d", jobID)
	}
	return events[0] // JobEvents orders newest first
}

func TestImportingVerifyRejectionFailsCandidateToSelecting(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{
		manualImportItems: []lidarr.ManualImportItem{
			{ID: 1, Path: "/music/complete/A/01.mp3", Rejections: []string{"Quality not in profile"}, Importable: false},
		},
	}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, now)

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateSelecting {
		t.Errorf("rejected import should return job to SELECTING, got %v", got)
	}
	assertCandidateNoLongerActive(t, st, jobID)
	if ev := lastJobEvent(t, st, jobID); ev.Event != core.EventImportRejected {
		t.Errorf("last event = %v, want %v", ev.Event, core.EventImportRejected)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("expected rejected candidate's folder cleaned up, got %+v", peers.deletedFolders)
	}
}

func TestImportingIncompleteCoverageFailsCandidate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{
		manualImportItems: []lidarr.ManualImportItem{
			{ID: 1, Path: "/music/complete/A/01.mp3", Importable: true, TrackIDs: []int64{101}},
		},
		albumTotal: 2, // only 1 track ID covered, out of 2 in the release
	}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, now)

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateSelecting {
		t.Errorf("incomplete coverage should return job to SELECTING, got %v", got)
	}
	assertCandidateNoLongerActive(t, st, jobID)
	ev := lastJobEvent(t, st, jobID)
	if ev.Event != core.EventImportRejected {
		t.Errorf("last event = %v, want %v", ev.Event, core.EventImportRejected)
	}
	if !strings.Contains(ev.Detail, "incomplete download") {
		t.Errorf("event detail = %q, want it to mention 'incomplete download'", ev.Detail)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("expected incomplete candidate's folder cleaned up, got %+v", peers.deletedFolders)
	}
	if len(music.executedItems) != 0 {
		t.Errorf("ExecuteManualImport must not run on an incomplete candidate, got %+v", music.executedItems)
	}
}

// TestImportingHappyPathSubmitsThenConfirmsToDone drives two ticks: the first
// verifies a clean, complete candidate and submits it (ExecuteManualImport
// called, ImportSubmittedAt set, job stays IMPORTING); the second, with
// AlbumStatus now reporting the album complete, confirms it (candidate
// SUCCEEDED, job DONE).
func TestImportingHappyPathSubmitsThenConfirmsToDone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{
		manualImportItems: []lidarr.ManualImportItem{
			{ID: 1, Path: "/music/complete/A/01.mp3", Importable: true, TrackIDs: []int64{101, 102}},
		},
		albumTotal:   2,
		albumPresent: 0, // not yet imported when verify runs
	}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	jobID, candID := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, now)

	m := NewImporting(p)

	// Tick 1: verify phase submits the import.
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if len(music.executedItems) != 1 {
		t.Fatalf("expected ExecuteManualImport called with 1 item, got %d", len(music.executedItems))
	}
	cand, found, err := st.ActiveCandidate(ctx, jobID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	if cand.ID != candID {
		t.Fatalf("ActiveCandidate returned id %d, want %d", cand.ID, candID)
	}
	if cand.ImportSubmittedAt == nil {
		t.Fatalf("expected ImportSubmittedAt set after verify submits the import")
	}
	if got := jobStateFor(t, st, jobID); got != core.StateImporting {
		t.Errorf("job should still be IMPORTING awaiting confirmation, got %v", got)
	}
	if len(peers.deletedFolders) != 0 {
		t.Errorf("a submitted import must not clean up the folder, got %+v", peers.deletedFolders)
	}
	if ev := lastJobEvent(t, st, jobID); ev.Event != core.EventImportOK {
		t.Errorf("last event after tick 1 = %v, want %v", ev.Event, core.EventImportOK)
	}

	// Tick 2: confirm phase sees the album complete.
	music.albumPresent = 2
	tick2 := now.Add(time.Minute)
	if err := m.Tick(ctx, tick2); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateDone {
		t.Errorf("confirmed import should advance job to DONE, got %v", got)
	}
	assertCandidateNoLongerActive(t, st, jobID)
	if ev := lastJobEvent(t, st, jobID); ev.Event != core.EventAttemptSucceeded {
		t.Errorf("last event after tick 2 = %v, want %v", ev.Event, core.EventAttemptSucceeded)
	}
}

func TestImportingConfirmTimeoutFailsCandidate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{albumTotal: 2, albumPresent: 1}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	jobID, candID := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, now)
	if err := st.MarkImportSubmitted(ctx, candID, now); err != nil {
		t.Fatalf("MarkImportSubmitted: %v", err)
	}

	m := NewImporting(p)

	// Within the timeout: left alone.
	withinTimeout := now.Add(p.ImportConfirmTimeout - time.Second)
	if err := m.Tick(ctx, withinTimeout); err != nil {
		t.Fatalf("Tick within timeout: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateImporting {
		t.Errorf("job should still be IMPORTING within the confirm timeout, got %v", got)
	}

	// Past the timeout: failed back to SELECTING, no cleanup (files were
	// imported/moved by Lidarr, not left behind by a failed download).
	pastTimeout := now.Add(p.ImportConfirmTimeout + time.Second)
	if err := m.Tick(ctx, pastTimeout); err != nil {
		t.Fatalf("Tick past timeout: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateSelecting {
		t.Errorf("unconfirmed import past timeout should return job to SELECTING, got %v", got)
	}
	assertCandidateNoLongerActive(t, st, jobID)
	ev := lastJobEvent(t, st, jobID)
	if ev.Event != core.EventAttemptFailed {
		t.Errorf("last event = %v, want %v", ev.Event, core.EventAttemptFailed)
	}
	if !strings.Contains(ev.Detail, "not confirmed") {
		t.Errorf("event detail = %q, want it to mention 'not confirmed'", ev.Detail)
	}
	if len(peers.deletedFolders) != 0 {
		t.Errorf("confirm-timeout must not clean up the folder, got %+v", peers.deletedFolders)
	}
}

// TestImportingEmptyFolderIdempotentDone reproduces a crash between a prior
// successful ExecuteManualImport and MarkImportSubmitted: verify re-runs,
// finds Lidarr's manual import preview empty (the files are already gone from
// the folder, imported), and must treat this as done rather than erroring.
func TestImportingEmptyFolderIdempotentDone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{manualImportItems: nil} // empty folder
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, now)

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateDone {
		t.Errorf("empty folder should be treated as already imported -> DONE, got %v", got)
	}
	assertCandidateNoLongerActive(t, st, jobID)
	if ev := lastJobEvent(t, st, jobID); ev.Event != core.EventAttemptSucceeded {
		t.Errorf("last event = %v, want %v", ev.Event, core.EventAttemptSucceeded)
	}
	if len(peers.deletedFolders) != 0 {
		t.Errorf("the idempotent already-imported path must not clean up anything, got %+v", peers.deletedFolders)
	}
}

// TestImportingStuckEscalation reproduces a Lidarr ManualImportCandidates call
// that keeps erroring every tick (e.g. a broken folder). Within StuckAfter the
// job is left alone to retry; once it has been stuck longer than StuckAfter,
// it must be failed and returned to SELECTING rather than wedged in IMPORTING
// forever.
func TestImportingStuckEscalation(t *testing.T) {
	ctx := context.Background()
	stuckSince := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{manualImportErr: context.DeadlineExceeded}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	p.StuckAfter = time.Minute
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, stuckSince)

	m := NewImporting(p)

	withinTimeout := stuckSince.Add(time.Second)
	if err := m.Tick(ctx, withinTimeout); err != nil {
		t.Fatalf("Tick within timeout: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateImporting {
		t.Errorf("job should still be IMPORTING within StuckAfter, got %v", got)
	}

	pastTimeout := stuckSince.Add(p.StuckAfter + time.Second)
	if err := m.Tick(ctx, pastTimeout); err != nil {
		t.Fatalf("Tick past timeout: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateSelecting {
		t.Errorf("stuck job past StuckAfter should fail to SELECTING, got %v", got)
	}
	assertCandidateNoLongerActive(t, st, jobID)
	if ev := lastJobEvent(t, st, jobID); ev.Event != core.EventAttemptFailed {
		t.Errorf("last event = %v, want %v", ev.Event, core.EventAttemptFailed)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("stuck escalation should clean up the folder, got %+v", peers.deletedFolders)
	}
}
