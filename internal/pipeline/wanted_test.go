package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
	"github.com/samuelenocsson/slusk/internal/store"
)

// newWantedSyncParams builds WantedSyncParams over a fresh store-backed
// fixture, returning both so tests can assert on store state directly (the
// full *store.Store, not just the narrow WantedSyncStore interface).
func newWantedSyncParams(t *testing.T, music *fakeMusic) (WantedSyncParams, *store.Store) {
	t.Helper()
	st := newBackedStore(t)
	return WantedSyncParams{
		Music:             music,
		Store:             st,
		Interval:          15 * time.Minute,
		FailedReviveAfter: 30 * 24 * time.Hour,
		Logger:            slog.New(slog.NewTextHandler(testDiscard{}, nil)),
	}, st
}

func TestWantedSyncUpsertsAndCancels(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	music := &fakeMusic{wanted: []core.WantedRelease{
		{ID: 1, Title: "A", ArtistName: "X"},
		{ID: 2, Title: "B", ArtistName: "Y"},
	}}
	p, st := newWantedSyncParams(t, music)

	// Pre-existing job for album 3, already DOWNLOADING - no longer wanted.
	job3, err := st.UpsertWantedJob(ctx, 3, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if _, err := st.AdvanceJobStateFrom(ctx, job3.ID, core.StateWanted, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobStateFrom: %v", err)
	}

	w := NewWantedSync(p)
	if err := w.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	wanted, err := st.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if len(wanted) != 2 {
		t.Fatalf("expected 2 WANTED jobs, got %d: %+v", len(wanted), wanted)
	}

	cancelled := jobStateFor(t, st, job3.ID)
	if cancelled != core.StateCancelled {
		t.Errorf("expected job 3 CANCELLED, got %s", cancelled)
	}
}

func TestWantedSyncRevivesOldFailed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	music := &fakeMusic{wanted: []core.WantedRelease{{ID: 1, Title: "A", ArtistName: "X"}}}
	p, st := newWantedSyncParams(t, music)

	// FAILED job for album 1 (still wanted), failed 31 days ago.
	stillWanted, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.MarkJobFailed(ctx, stillWanted.ID, now.Add(-31*24*time.Hour)); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	// FAILED job for album 9 (absent from wanted list), failed 31 days ago too.
	noLongerWanted, err := st.UpsertWantedJob(ctx, 9, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.MarkJobFailed(ctx, noLongerWanted.ID, now.Add(-31*24*time.Hour)); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	w := NewWantedSync(p)
	if err := w.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	revivedJobs, err := st.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	var revived *core.AlbumJob
	for i := range revivedJobs {
		if revivedJobs[i].ID == stillWanted.ID {
			revived = &revivedJobs[i]
		}
	}
	if revived == nil {
		t.Fatalf("expected job %d revived to WANTED, jobs: %+v", stillWanted.ID, revivedJobs)
	}
	if revived.Retries != 0 {
		t.Errorf("expected revived job to have retries reset to 0, got %d", revived.Retries)
	}

	// Album 9 left the wanted list: ReviveFailedJobs only revives jobs whose
	// album is still in wantedIDs, so this job must stay FAILED (FAILED is
	// also one of CancelJobsNotWanted's excluded terminal states, so it isn't
	// touched by that pass either).
	untouchedState := jobStateFor(t, st, noLongerWanted.ID)
	if untouchedState != core.StateFailed {
		t.Errorf("expected job %d (album no longer wanted) to remain FAILED, got %s", noLongerWanted.ID, untouchedState)
	}
}

// TestWantedSyncEmptyListSkipsCancellation: a successful but empty wanted-list
// fetch must NOT cancel in-flight jobs - a transient empty response from Lidarr
// would otherwise cancel every job in the pipeline.
func TestWantedSyncEmptyListSkipsCancellation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	music := &fakeMusic{wanted: nil} // successful fetch, empty list
	p, st := newWantedSyncParams(t, music)

	// A pre-existing DOWNLOADING job that would be cancelled if cancellation ran.
	job, err := st.UpsertWantedJob(ctx, 42, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if _, err := st.AdvanceJobStateFrom(ctx, job.ID, core.StateWanted, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobStateFrom: %v", err)
	}

	w := NewWantedSync(p)
	if err := w.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := jobStateFor(t, st, job.ID); got != core.StateDownloading {
		t.Errorf("empty wanted list must not cancel in-flight jobs, job state = %v, want DOWNLOADING", got)
	}
}

// TestWantedSyncReentersCancelledAlbum: a CANCELLED job whose album reappears on
// the wanted list must re-enter WANTED via the sync (UpsertWantedJob's re-enter
// path).
func TestWantedSyncReentersCancelledAlbum(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	music := &fakeMusic{wanted: []core.WantedRelease{{ID: 7, Title: "A", ArtistName: "X"}}}
	p, st := newWantedSyncParams(t, music)

	job, err := st.UpsertWantedJob(ctx, 7, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	w := NewWantedSync(p)
	if err := w.Tick(ctx, now.Add(time.Minute)); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := jobStateFor(t, st, job.ID); got != core.StateWanted {
		t.Errorf("re-wanted CANCELLED album must re-enter WANTED, got %v", got)
	}
}

type recordingWantedSyncStore struct {
	syncCalls  int
	pruneCalls int
	releases   []core.WantedRelease
	cutoff     time.Time
	now        time.Time
	cancelled  int
	revived    int
	syncErr    error
	pruneErr   error
	// uploadHistoryPrunedAt records the now PruneUploadHistory was called
	// with; the zero value means it was never called.
	uploadHistoryPrunedAt time.Time
}

func (s *recordingWantedSyncStore) SyncWantedJobs(_ context.Context, releases []core.WantedRelease, cutoff, now time.Time) (int, int, error) {
	s.syncCalls++
	s.releases = append([]core.WantedRelease(nil), releases...)
	s.cutoff = cutoff
	s.now = now
	return s.cancelled, s.revived, s.syncErr
}

func (s *recordingWantedSyncStore) PruneJobEvents(context.Context, time.Time) error {
	s.pruneCalls++
	return s.pruneErr
}

func (s *recordingWantedSyncStore) PruneSearchPasses(context.Context, time.Time) error {
	return nil
}

func (s *recordingWantedSyncStore) PruneThroughputMinutes(context.Context, time.Time) error {
	return nil
}

func (s *recordingWantedSyncStore) PruneUploadHistory(_ context.Context, now time.Time) error {
	s.uploadHistoryPrunedAt = now
	return nil
}

// TestWantedSyncPrunesUploadHistory asserts upload_history retention actually
// runs on the sync tick (issue #325). A busy share appends rows continuously,
// so a prune that is defined but never called is unbounded growth with a
// reassuring-looking implementation next to it.
func TestWantedSyncPrunesUploadHistory(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := &recordingWantedSyncStore{}
	w := NewWantedSync(WantedSyncParams{Music: &fakeMusic{}, Store: store})

	if err := w.Tick(context.Background(), now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !store.uploadHistoryPrunedAt.Equal(now) {
		t.Errorf("PruneUploadHistory called with %v, want the tick's now %v", store.uploadHistoryPrunedAt, now)
	}
}

func TestWantedSyncUsesOneBulkCallPublishesLastDuplicateAndLogs(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{wanted: []core.WantedRelease{
		{ID: 1, Title: "first"},
		{ID: 2, Title: "other"},
		{ID: 1, Title: "last"},
	}}
	store := &recordingWantedSyncStore{cancelled: 2, revived: 3}
	var logs bytes.Buffer
	w := NewWantedSync(WantedSyncParams{
		Music: music, Store: store, FailedReviveAfter: 30 * 24 * time.Hour,
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})

	if err := w.Tick(context.Background(), now); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if store.syncCalls != 1 || store.pruneCalls != 1 {
		t.Fatalf("calls = sync %d, prune %d; want 1, 1", store.syncCalls, store.pruneCalls)
	}
	if len(store.releases) != 3 {
		t.Fatalf("bulk call received %d releases, want original 3-entry snapshot", len(store.releases))
	}
	if want := now.Add(-30 * 24 * time.Hour); !store.cutoff.Equal(want) {
		t.Errorf("failed cutoff = %v, want %v", store.cutoff, want)
	}
	if got := w.Wanted(); len(got) != 2 || got[1].Title != "last" {
		t.Errorf("published snapshot = %+v, want 2 unique albums and duplicate last-wins", got)
	}
	logged := logs.String()
	for _, fragment := range []string{"wanted sync complete", "albums=2", "duration="} {
		if !strings.Contains(logged, fragment) {
			t.Errorf("success log %q missing %q", logged, fragment)
		}
	}
}

func TestWantedSyncDoesNotPublishAfterPersistenceOrPruneError(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	music := &fakeMusic{wanted: []core.WantedRelease{{ID: 1, Title: "initial"}}}
	store := &recordingWantedSyncStore{}
	w := NewWantedSync(WantedSyncParams{Music: music, Store: store, FailedReviveAfter: time.Hour})
	if err := w.Tick(context.Background(), now); err != nil {
		t.Fatalf("initial Tick: %v", err)
	}

	music.wanted = []core.WantedRelease{{ID: 2, Title: "replacement"}}
	store.syncErr = errors.New("bulk failed")
	if err := w.Tick(context.Background(), now.Add(time.Minute)); err == nil {
		t.Fatal("Tick succeeded after bulk reconciliation error")
	}
	if got := w.Wanted(); len(got) != 1 || got[1].Title != "initial" {
		t.Errorf("snapshot changed after bulk error: %+v", got)
	}
	if store.pruneCalls != 1 {
		t.Errorf("prune called after bulk error; calls = %d, want initial call only", store.pruneCalls)
	}

	store.syncErr = nil
	store.pruneErr = errors.New("prune failed")
	if err := w.Tick(context.Background(), now.Add(2*time.Minute)); err == nil {
		t.Fatal("Tick succeeded after prune error")
	}
	if got := w.Wanted(); len(got) != 1 || got[1].Title != "initial" {
		t.Errorf("snapshot changed after prune error: %+v", got)
	}
}

func TestWantedSyncKeepsSnapshotOnLidarrError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	music := &fakeMusic{wanted: []core.WantedRelease{{ID: 1, Title: "A", ArtistName: "X"}}}
	p, st := newWantedSyncParams(t, music)

	w := NewWantedSync(p)
	if err := w.Tick(ctx, now); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	if len(w.Wanted()) != 1 {
		t.Fatalf("expected 1 album in snapshot after first tick, got %d", len(w.Wanted()))
	}

	// A pre-existing job for album 2, DOWNLOADING - not present on the second
	// (failing) tick's wanted list, so it would be cancelled if cancellation
	// ran despite the fetch error.
	job2, err := st.UpsertWantedJob(ctx, 2, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if _, err := st.AdvanceJobStateFrom(ctx, job2.ID, core.StateWanted, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobStateFrom: %v", err)
	}

	music.wantedErr = errors.New("lidarr unreachable")
	if err := w.Tick(ctx, now.Add(time.Minute)); err == nil {
		t.Fatalf("expected second Tick to return an error")
	}

	if got := w.Wanted(); len(got) != 1 || got[1].Title != "A" {
		t.Errorf("expected snapshot unchanged after a failing tick, got %+v", got)
	}

	state := jobStateFor(t, st, job2.ID)
	if state != core.StateDownloading {
		t.Errorf("expected job 2 to remain DOWNLOADING (no cancellation on Lidarr error), got %s", state)
	}
}

// testDiscard is an io.Writer that discards everything, used to keep test
// logs quiet without needing *testing.T plumbed into slog.
type testDiscard struct{}

func (testDiscard) Write(p []byte) (int, error) { return len(p), nil }

// TestWantedSyncPrunesExpiredSearchPasses asserts Tick calls
// PruneSearchPasses alongside PruneJobEvents (issue #88): a search pass older
// than the retention window must be gone after a sync, while a recent one
// survives.
func TestWantedSyncPrunesExpiredSearchPasses(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	music := &fakeMusic{wanted: []core.WantedRelease{{ID: 1, Title: "A", ArtistName: "X"}}}
	p, st := newWantedSyncParams(t, music)

	old := now.Add(-31 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)
	if err := st.RecordSearchPass(ctx, core.SearchPass{StartedAt: old, FinishedAt: old, Searched: 1}); err != nil {
		t.Fatalf("RecordSearchPass old: %v", err)
	}
	if err := st.RecordSearchPass(ctx, core.SearchPass{StartedAt: recent, FinishedAt: recent, Searched: 1}); err != nil {
		t.Fatalf("RecordSearchPass recent: %v", err)
	}

	w := NewWantedSync(p)
	if err := w.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	passes, err := st.RecentSearchPasses(ctx, 10)
	if err != nil {
		t.Fatalf("RecentSearchPasses: %v", err)
	}
	if len(passes) != 1 {
		t.Fatalf("expected 1 surviving search pass, got %d: %+v", len(passes), passes)
	}
	if !passes[0].StartedAt.Equal(recent) {
		t.Errorf("surviving pass StartedAt = %v, want %v", passes[0].StartedAt, recent)
	}
}

// jobStateFor scans every pipeline state to find jobID's current state, since
// the store has no direct get-by-ID lookup exposed here. It uses
// RunnableJobsInState with a far-future "now" so a job hidden behind a
// not_before backoff is still found regardless of state.
func jobStateFor(t *testing.T, st *store.Store, jobID int64) core.AlbumJobState {
	t.Helper()
	ctx := context.Background()
	farFuture := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	all := []core.AlbumJobState{
		core.StateWanted, core.StateSearching, core.StateSelecting,
		core.StateDownloading, core.StateVerifying, core.StateImporting,
		core.StateDone, core.StateCooldown, core.StateFailed, core.StateCancelled,
		core.StateDiscovered, core.StateCompleted, core.StateNotImported,
	}
	for _, state := range all {
		jobs, err := st.RunnableJobsInState(ctx, state, farFuture, 100)
		if err != nil {
			t.Fatalf("RunnableJobsInState(%v): %v", state, err)
		}
		for _, j := range jobs {
			if j.ID == jobID {
				return state
			}
		}
	}
	t.Fatalf("job %d not found in any known state", jobID)
	return ""
}
