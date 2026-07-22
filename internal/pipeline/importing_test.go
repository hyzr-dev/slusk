package pipeline

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
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
		manualImportItems: []core.ImportItem{
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
		manualImportItems: []core.ImportItem{
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

// TestImportingVerifyKeepsTrackMatchedFilesDespiteFolderRejection: Lidarr
// stamps "Has unmatched tracks" on every file in a non-bijective folder,
// including files that individually matched a track. Files with TrackIDs must
// be imported anyway; only genuinely unmatched files (no TrackIDs) are dropped.
func TestImportingVerifyKeepsTrackMatchedFilesDespiteFolderRejection(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{
		manualImportItems: []core.ImportItem{
			{ID: 1, Path: "/music/complete/A/01.mp3", Importable: false, TrackIDs: []int64{1}, Rejections: []string{"Has unmatched tracks"}},
			{ID: 2, Path: "/music/complete/A/02.mp3", Importable: false, TrackIDs: []int64{2}, Rejections: []string{"Has unmatched tracks"}},
			{ID: 3, Path: "/music/complete/A/03.mp3", Importable: false, TrackIDs: nil, Rejections: []string{"Has unmatched tracks"}},
		},
	}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{
		{Filename: `A\01.mp3`, Size: 10}, {Filename: `A\02.mp3`, Size: 10}, {Filename: `A\03.mp3`, Size: 10},
	}, now)
	if err := st.SetJobTrackBand(ctx, jobID, 2, 3); err != nil {
		t.Fatalf("SetJobTrackBand: %v", err)
	}

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(music.executedItems) != 2 {
		t.Fatalf("expected exactly the 2 track-matched items submitted, got %+v", music.executedItems)
	}
	for _, it := range music.executedItems {
		if len(it.TrackIDs) == 0 {
			t.Errorf("submitted item without TrackIDs: %+v", it)
		}
	}
	cand, found, err := st.ActiveCandidate(ctx, jobID)
	if err != nil {
		t.Fatalf("ActiveCandidate: %v", err)
	}
	if !found {
		t.Fatalf("expected candidate still ACTIVE")
	}
	if cand.ImportSubmittedAt == nil {
		t.Errorf("expected ImportSubmittedAt set after successful submission")
	}
	if got := jobStateFor(t, st, jobID); got != core.StateImporting {
		t.Errorf("job state = %v, want still IMPORTING", got)
	}
}

// TestImportingVerifyAllUnmatchedStillFailsCandidate: when no file at all has
// a TrackID, the candidate fails to SELECTING exactly as before.
func TestImportingVerifyAllUnmatchedStillFailsCandidate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{
		manualImportItems: []core.ImportItem{
			{ID: 1, Path: "/music/complete/A/01.mp3", Importable: false, TrackIDs: nil, Rejections: []string{"Has unmatched tracks"}},
			{ID: 2, Path: "/music/complete/A/02.mp3", Importable: false, TrackIDs: nil, Rejections: []string{"Has unmatched tracks"}},
		},
	}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{
		{Filename: `A\01.mp3`, Size: 10}, {Filename: `A\02.mp3`, Size: 10},
	}, now)

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateSelecting {
		t.Errorf("all-unmatched import should return job to SELECTING, got %v", got)
	}
	assertCandidateNoLongerActive(t, st, jobID)
	if ev := lastJobEvent(t, st, jobID); ev.Event != core.EventImportRejected {
		t.Errorf("last event = %v, want %v", ev.Event, core.EventImportRejected)
	}
}

// TestImportingCoverageUsesMinTrackCountBand: a candidate covering the
// smallest valid edition (2 tracks) passes even though the canonical
// AlbumStatus total is 3.
func TestImportingCoverageUsesMinTrackCountBand(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{
		manualImportItems: []core.ImportItem{
			{ID: 1, Path: "/music/complete/A/01.mp3", Importable: true, TrackIDs: []int64{1}},
			{ID: 2, Path: "/music/complete/A/02.mp3", Importable: true, TrackIDs: []int64{2}},
		},
		albumTotal: 3,
	}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{
		{Filename: `A\01.mp3`, Size: 10}, {Filename: `A\02.mp3`, Size: 10},
	}, now)
	if err := st.SetJobTrackBand(ctx, jobID, 2, 3); err != nil {
		t.Fatalf("SetJobTrackBand: %v", err)
	}

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(music.executedItems) != 2 {
		t.Fatalf("expected import submitted (not rejected as incomplete), got executed=%+v", music.executedItems)
	}
	if ev := lastJobEvent(t, st, jobID); ev.Event == core.EventImportRejected {
		t.Errorf("expected no rejection event, got %+v", ev)
	}
}

// TestImportingVerifyDedupsFolderBeforeImportScan: verify removes duplicate
// track files from the album folder before asking Lidarr what to import.
func TestImportingVerifyDedupsFolderBeforeImportScan(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	dir := t.TempDir()
	albumDir := filepath.Join(dir, "Album")
	if err := os.MkdirAll(albumDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.flac", "one.mp3"} {
		if err := os.WriteFile(filepath.Join(albumDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := readFileMeta
	t.Cleanup(func() { readFileMeta = orig })
	readFileMeta = func(path string, size int64) dedupFile {
		switch filepath.Base(path) {
		case "one.flac":
			return df(path, 30_000_000, 1, 1, "One", true)
		default:
			return df(path, 8_000_000, 1, 1, "One", false)
		}
	}

	music := &fakeMusic{
		manualImportItems: []core.ImportItem{
			{ID: 1, Path: filepath.Join(albumDir, "one.flac"), Importable: true, TrackIDs: []int64{1}},
		},
	}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	p.CompleteDir = dir
	_, _ = seedImportingJob(t, st, 1, "bob", []core.CandidateFile{
		{Filename: `Album\one.flac`, Size: 10}, {Filename: `Album\one.mp3`, Size: 10},
	}, now)

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if _, err := os.Stat(filepath.Join(albumDir, "one.mp3")); !os.IsNotExist(err) {
		t.Error("one.mp3 should have been removed by dedup before the import scan")
	}
	if _, err := os.Stat(filepath.Join(albumDir, "one.flac")); err != nil {
		t.Errorf("one.flac should still be present: %v", err)
	}
	if len(music.manualImportCalls) != 1 {
		t.Fatalf("expected ManualImportCandidates called once, got %d", len(music.manualImportCalls))
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
		manualImportItems: []core.ImportItem{
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

// TestImportingMissingFolderIdempotentDone reproduces a crash between a prior
// successful ExecuteManualImport and MarkImportSubmitted: verify re-runs,
// finds Lidarr's manual import preview empty and the local folder already gone,
// and must treat this as done rather than erroring.
func TestImportingMissingFolderIdempotentDone(t *testing.T) {
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

func TestImportingEmptyFolderIdempotentDone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{manualImportItems: nil}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	p.CompleteDir = t.TempDir()
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, now)
	if err := os.Mkdir(filepath.Join(p.CompleteDir, "A"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateDone {
		t.Errorf("existing empty folder should be treated as already imported -> DONE, got %v", got)
	}
	assertCandidateNoLongerActive(t, st, jobID)
	if ev := lastJobEvent(t, st, jobID); ev.Event != core.EventAttemptSucceeded {
		t.Errorf("last event = %v, want %v", ev.Event, core.EventAttemptSucceeded)
	}
}

func TestImportingEmptyCandidatesWithFilesRetriesThenEscalates(t *testing.T) {
	ctx := context.Background()
	stuckSince := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{manualImportItems: nil}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	p.CompleteDir = t.TempDir()
	p.StuckAfter = time.Minute
	p.RetryCooldown = 30 * time.Second
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, stuckSince)
	albumFolder := filepath.Join(p.CompleteDir, "A")
	if err := os.Mkdir(albumFolder, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(albumFolder, "01.mp3"), []byte("downloaded"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := NewImporting(p)
	if err := m.Tick(ctx, stuckSince.Add(time.Second)); err != nil {
		t.Fatalf("Tick within StuckAfter: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateImporting {
		t.Errorf("job with local files should remain IMPORTING within StuckAfter, got %v", got)
	}
	if _, found, err := st.ActiveCandidate(ctx, jobID); err != nil || !found {
		t.Errorf("candidate should remain ACTIVE within StuckAfter, found=%v (%v)", found, err)
	}
	if len(music.executedItems) != 0 {
		t.Errorf("ExecuteManualImport must not run for an empty preview, got %+v", music.executedItems)
	}

	if err := m.Tick(ctx, stuckSince.Add(2*time.Second)); err != nil {
		t.Fatalf("Tick within retry cooldown: %v", err)
	}
	if got := len(music.manualImportCalls); got != 1 {
		t.Errorf("empty preview anomaly retried within cooldown: %d attempts", got)
	}

	if err := m.Tick(ctx, stuckSince.Add(p.StuckAfter+time.Second)); err != nil {
		t.Fatalf("Tick past StuckAfter: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateSelecting {
		t.Errorf("anomalous candidate past StuckAfter should return job to SELECTING, got %v", got)
	}
	assertCandidateNoLongerActive(t, st, jobID)
	ev := lastJobEvent(t, st, jobID)
	if ev.Event != core.EventAttemptFailed {
		t.Errorf("last event = %v, want %v", ev.Event, core.EventAttemptFailed)
	}
	if !strings.Contains(ev.Detail, "empty import candidates for non-empty folder") {
		t.Errorf("event detail = %q, want anomaly reason", ev.Detail)
	}
}

func TestImportingEmptyCandidatesFolderReadErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{manualImportItems: nil}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	p.CompleteDir = t.TempDir()
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, now)
	// AlbumFolder resolves to CompleteDir/A. Making that path a regular file
	// produces a stable ReadDir error on every platform without permissions.
	if err := os.WriteFile(filepath.Join(p.CompleteDir, "A"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateImporting {
		t.Errorf("folder read error must not mark job DONE, got %v", got)
	}
	if _, found, err := st.ActiveCandidate(ctx, jobID); err != nil || !found {
		t.Errorf("candidate should remain ACTIVE for retry, found=%v (%v)", found, err)
	}
	if len(music.executedItems) != 0 {
		t.Errorf("ExecuteManualImport must not run for an empty preview, got %+v", music.executedItems)
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

// hasErrorLog reports whether buf (a slog text-handler's output) contains an
// ERROR-level line, so cleanup-on-confirm tests can assert a cleanup failure
// (or a "not empty"/"already gone" case) never escalates to an ERROR log,
// which would be noisy for a routine, expected condition.
func hasErrorLog(buf *strings.Builder) bool {
	return strings.Contains(buf.String(), "level=ERROR")
}

// TestImportingConfirmRemovesEmptyAlbumFolder drives confirm() to DONE via the
// present>=total path and asserts the empty per-album folder slskd left behind
// under CompleteDir is removed once the import is confirmed.
func TestImportingConfirmRemovesEmptyAlbumFolder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{albumTotal: 2, albumPresent: 2}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	completeDir := t.TempDir()
	p.CompleteDir = completeDir
	var logBuf strings.Builder
	p.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))

	jobID, candID := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, now)
	if err := st.MarkImportSubmitted(ctx, candID, now); err != nil {
		t.Fatalf("MarkImportSubmitted: %v", err)
	}
	albumFolder := filepath.Join(completeDir, "A")
	if err := os.Mkdir(albumFolder, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateDone {
		t.Errorf("confirmed import should advance job to DONE, got %v", got)
	}
	if _, err := os.Stat(albumFolder); !os.IsNotExist(err) {
		t.Errorf("expected empty album folder to be removed, stat err = %v", err)
	}
	if hasErrorLog(&logBuf) {
		t.Errorf("expected no ERROR log line, got %q", logBuf.String())
	}
}

// TestImportingConfirmLeavesNonEmptyAlbumFolderButStillCompletesJob covers the
// safety case: even a confirmed import could theoretically leave stray files
// (partial import, junk), so cleanup must never remove anything but an
// already-verified-empty directory, and its failure to remove must never
// block the job's own DONE transition.
func TestImportingConfirmLeavesNonEmptyAlbumFolderButStillCompletesJob(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{albumTotal: 2, albumPresent: 2}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	completeDir := t.TempDir()
	p.CompleteDir = completeDir
	var logBuf strings.Builder
	p.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))

	jobID, candID := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, now)
	if err := st.MarkImportSubmitted(ctx, candID, now); err != nil {
		t.Fatalf("MarkImportSubmitted: %v", err)
	}
	albumFolder := filepath.Join(completeDir, "A")
	if err := os.Mkdir(albumFolder, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(albumFolder, "leftover.txt"), []byte("junk"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateDone {
		t.Errorf("job must still reach DONE even when cleanup can't remove the folder, got %v", got)
	}
	if _, err := os.Stat(albumFolder); err != nil {
		t.Errorf("expected non-empty album folder to remain, stat err = %v", err)
	}
	if hasErrorLog(&logBuf) {
		t.Errorf("expected no ERROR log line for a non-empty folder, got %q", logBuf.String())
	}
}

// TestImportingConfirmToleratesMissingAlbumFolder covers the case where the
// per-album folder is already gone (e.g. removed out-of-band) by the time
// confirm runs: the job must still reach DONE without panicking or logging an
// ERROR.
func TestImportingConfirmToleratesMissingAlbumFolder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{albumTotal: 2, albumPresent: 2}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	p.CompleteDir = t.TempDir() // album subfolder deliberately never created
	var logBuf strings.Builder
	p.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))

	jobID, candID := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, now)
	if err := st.MarkImportSubmitted(ctx, candID, now); err != nil {
		t.Fatalf("MarkImportSubmitted: %v", err)
	}

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateDone {
		t.Errorf("job must still reach DONE when the album folder is already gone, got %v", got)
	}
	if hasErrorLog(&logBuf) {
		t.Errorf("expected no ERROR log line for an already-missing folder, got %q", logBuf.String())
	}
}

// TestImportingVerifyErrorCoolsDownRetries reproduces the "spam Lidarr" bug: a
// ManualImportCandidates call that fails (e.g. Lidarr timing out on a large
// folder scan) used to be retried on the very next tick (~30s), hammering the
// same slow folder for up to StuckAfter. With RetryCooldown set, the job must
// be hidden until the cooldown elapses, then retried.
func TestImportingVerifyErrorCoolsDownRetries(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{manualImportErr: context.DeadlineExceeded}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	p.RetryCooldown = 5 * time.Minute
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, now)

	m := NewImporting(p)
	if err := m.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := len(music.manualImportCalls); got != 1 {
		t.Fatalf("expected 1 scan attempt, got %d", got)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateImporting {
		t.Errorf("job should stay IMPORTING within StuckAfter, got %v", got)
	}

	// Next tick lands within the cooldown: the job must not be scanned again.
	if err := m.Tick(ctx, now.Add(30*time.Second)); err != nil {
		t.Fatalf("Tick within cooldown: %v", err)
	}
	if got := len(music.manualImportCalls); got != 1 {
		t.Errorf("scan retried within cooldown: %d attempts", got)
	}

	// Past the cooldown the job becomes runnable again and retries.
	if err := m.Tick(ctx, now.Add(5*time.Minute+time.Second)); err != nil {
		t.Fatalf("Tick past cooldown: %v", err)
	}
	if got := len(music.manualImportCalls); got != 2 {
		t.Errorf("expected retry after cooldown, got %d attempts", got)
	}
}

// TestImportingStuckEscalationSurvivesCooldown asserts the cooldown does not
// defeat the StuckAfter ceiling: the cooldown write must leave the job's
// updated_at (escalateIfStuck's clock) untouched, so a job whose scans keep
// failing is still escalated to SELECTING once runnable past StuckAfter.
func TestImportingStuckEscalationSurvivesCooldown(t *testing.T) {
	ctx := context.Background()
	stuckSince := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{manualImportErr: context.DeadlineExceeded}
	peers := &fakeSearcher{}
	p, st := newImportingParams(t, music, peers)
	p.StuckAfter = time.Hour
	p.RetryCooldown = 5 * time.Minute
	jobID, _ := seedImportingJob(t, st, 1, "bob", []core.CandidateFile{{Filename: `A\01.mp3`, Size: 10}}, stuckSince)

	m := NewImporting(p)
	// Fail scans every cooldown-interval until past StuckAfter.
	for tick := time.Duration(0); tick <= p.StuckAfter; tick += p.RetryCooldown + time.Second {
		if err := m.Tick(ctx, stuckSince.Add(tick)); err != nil {
			t.Fatalf("Tick at +%v: %v", tick, err)
		}
	}
	if err := m.Tick(ctx, stuckSince.Add(p.StuckAfter+p.RetryCooldown+2*time.Second)); err != nil {
		t.Fatalf("Tick past StuckAfter: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateSelecting {
		t.Errorf("stuck job past StuckAfter should fail to SELECTING, got %v", got)
	}
	assertCandidateNoLongerActive(t, st, jobID)
}
