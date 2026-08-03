package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

func TestSyncWantedJobsLargeSnapshotAndDuplicateLastWins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	releases := make([]core.WantedRelease, 0, 1003)
	for id := int64(1); id <= 1001; id++ {
		releases = append(releases, core.WantedRelease{
			ID: id, Title: fmt.Sprintf("album-%d", id), ArtistName: "artist",
			ReleaseDate: "2026-01-01", ArtistID: id + 10_000,
		})
	}
	releases = append(releases,
		core.WantedRelease{ID: 500, Title: "superseded", ArtistName: "old"},
		core.WantedRelease{ID: 500, Title: "last title", ArtistName: "last artist", ReleaseDate: "2026-07-31", ArtistID: 77},
	)

	cancelled, revived, err := s.SyncWantedJobs(ctx, releases, now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatalf("SyncWantedJobs: %v", err)
	}
	if cancelled != 0 || revived != 0 {
		t.Fatalf("counts = cancelled %d, revived %d; want 0, 0", cancelled, revived)
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM album_jobs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1001 {
		t.Fatalf("job count = %d, want 1001 unique albums", count)
	}
	var title, artist, releaseDate string
	var artistID int64
	var updatedAt time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT title, artist_name, release_date, artist_id, updated_at FROM album_jobs WHERE lidarr_album_id = 500`).
		Scan(&title, &artist, &releaseDate, &artistID, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if title != "last title" || artist != "last artist" || releaseDate != "2026-07-31" || artistID != 77 {
		t.Errorf("duplicate metadata = %q/%q/%q/%d; last occurrence did not win", title, artist, releaseDate, artistID)
	}
	if !updatedAt.Equal(now) {
		t.Errorf("updated_at = %v, want %v", updatedAt, now)
	}
}

func TestSyncWantedJobsMetadataCancellationRevivalAndCleanup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	created := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	now := created.Add(2 * time.Hour)
	cutoff := now.Add(-30 * 24 * time.Hour)

	wanted, _ := s.UpsertWantedJob(ctx, 1, created)
	if err := s.UpdateJobMetadata(ctx, wanted.ID, "old title", "old artist", "2020", 1, created); err != nil {
		t.Fatal(err)
	}

	active, _ := s.UpsertWantedJob(ctx, 2, created)
	if _, err := s.AdvanceJobStateFrom(ctx, active.ID, core.StateWanted, core.StateDownloading, created.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	activeUpdated := created.Add(time.Minute)
	if _, err := s.db.ExecContext(ctx, `UPDATE album_jobs SET title='partial old', artist_name='', release_date='old date', artist_id=9, updated_at=$1 WHERE id=$2`, activeUpdated, active.ID); err != nil {
		t.Fatal(err)
	}

	absent, _ := s.UpsertWantedJob(ctx, 3, created)
	if _, err := s.AdvanceJobStateFrom(ctx, absent.ID, core.StateWanted, core.StateDownloading, created); err != nil {
		t.Fatal(err)
	}
	done, _ := s.UpsertWantedJob(ctx, 4, created)
	if err := s.AdvanceJobState(ctx, done.ID, core.StateDone, created); err != nil {
		t.Fatal(err)
	}
	absentCancelled, _ := s.UpsertWantedJob(ctx, 8, created)
	if err := s.AdvanceJobState(ctx, absentCancelled.ID, core.StateCancelled, created); err != nil {
		t.Fatal(err)
	}
	absentFailed, _ := s.UpsertWantedJob(ctx, 9, created)
	if err := s.MarkJobFailed(ctx, absentFailed.ID, cutoff.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	reentered, _ := s.UpsertWantedJob(ctx, 5, created)
	reenteredCandidate := seedWantedSyncChild(t, s, reentered.ID, created, "cancelled-child")
	if err := s.AdvanceJobState(ctx, reentered.ID, core.StateCancelled, created); err != nil {
		t.Fatal(err)
	}
	// Seed an empty-search streak (issue #334): the CANCELLED-reentry branch
	// below is a clean-slate reset and must wipe it too.
	if err := s.SetJobEmptySearchBackoff(ctx, reentered.ID, 4, created, created); err != nil {
		t.Fatal(err)
	}

	oldFailed, _ := s.UpsertWantedJob(ctx, 6, created)
	failedCandidate := seedWantedSyncChild(t, s, oldFailed.ID, created, "failed-child")
	if err := s.MarkJobFailed(ctx, oldFailed.ID, cutoff.Add(-time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	// Same for the FAILED-revive branch - this is SyncWantedJobs' own
	// inlined revive, the production path RetryFailedJob and
	// ReviveFailedJobs are not (ReviveFailedJobs has no caller).
	if err := s.SetJobEmptySearchBackoff(ctx, oldFailed.ID, 6, created, created); err != nil {
		t.Fatal(err)
	}
	boundaryFailed, _ := s.UpsertWantedJob(ctx, 7, created)
	if err := s.MarkJobFailed(ctx, boundaryFailed.ID, cutoff); err != nil {
		t.Fatal(err)
	}

	releases := []core.WantedRelease{
		{ID: 1, Title: "new title", ArtistName: "new artist", ReleaseDate: "2026", ArtistID: 101},
		{ID: 2, Title: "backfilled title", ArtistName: "backfilled artist", ReleaseDate: "2025", ArtistID: 202},
		{ID: 5, Title: "returned", ArtistName: "artist"},
		{ID: 6, Title: "revived", ArtistName: "artist"},
		{ID: 7, Title: "boundary", ArtistName: "artist"},
	}
	cancelled, revived, err := s.SyncWantedJobs(ctx, releases, cutoff, now)
	if err != nil {
		t.Fatalf("SyncWantedJobs: %v", err)
	}
	if cancelled != 1 || revived != 1 {
		t.Fatalf("counts = cancelled %d, revived %d; want 1, 1", cancelled, revived)
	}

	assertWantedSyncJob(t, s, wanted.ID, core.StateWanted, "new title", "new artist", "2026", 101, now)
	assertWantedSyncJob(t, s, active.ID, core.StateDownloading, "backfilled title", "backfilled artist", "2025", 202, activeUpdated)
	assertWantedSyncState(t, s, absent.ID, core.StateCancelled)
	assertWantedSyncState(t, s, done.ID, core.StateDone)
	assertWantedSyncState(t, s, absentCancelled.ID, core.StateCancelled)
	assertWantedSyncState(t, s, absentFailed.ID, core.StateFailed)
	assertWantedSyncState(t, s, reentered.ID, core.StateWanted)
	assertWantedSyncState(t, s, oldFailed.ID, core.StateWanted)
	assertWantedSyncState(t, s, boundaryFailed.ID, core.StateFailed)
	assertWantedSyncEmptySearches(t, s, reentered.ID, 0)
	assertWantedSyncEmptySearches(t, s, oldFailed.ID, 0)

	for _, child := range []struct{ jobID, candidateID int64 }{{reentered.ID, reenteredCandidate}, {oldFailed.ID, failedCandidate}} {
		var candidates, transfers int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM candidates WHERE album_job_id=$1`, child.jobID).Scan(&candidates); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM transfers WHERE candidate_id=$1`, child.candidateID).Scan(&transfers); err != nil {
			t.Fatal(err)
		}
		if candidates != 0 || transfers != 0 {
			t.Errorf("job %d children remain: %d candidates, %d transfers", child.jobID, candidates, transfers)
		}
	}
}

func TestSyncWantedJobsCancelsBothParkedSpellings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	parked, _ := s.UpsertWantedJob(ctx, 20, now)
	legacy, _ := s.UpsertWantedJob(ctx, 21, now)
	if err := s.AdvanceJobState(ctx, parked.ID, core.StateParked, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceJobState(ctx, legacy.ID, core.StateOrphaned, now); err != nil {
		t.Fatal(err)
	}

	cancelled, revived, err := s.SyncWantedJobs(ctx,
		[]core.WantedRelease{{ID: 22, Title: "still wanted", ArtistName: "artist"}},
		now.Add(-30*24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SyncWantedJobs: %v", err)
	}
	if cancelled != 2 || revived != 0 {
		t.Fatalf("counts = cancelled %d, revived %d; want 2, 0", cancelled, revived)
	}
	assertWantedSyncState(t, s, parked.ID, core.StateCancelled)
	assertWantedSyncState(t, s, legacy.ID, core.StateCancelled)
}

func TestSyncWantedJobsEmptyInputChangesNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	active, _ := s.UpsertWantedJob(ctx, 1, now)
	if _, err := s.AdvanceJobStateFrom(ctx, active.ID, core.StateWanted, core.StateDownloading, now); err != nil {
		t.Fatal(err)
	}
	failed, _ := s.UpsertWantedJob(ctx, 2, now)
	if err := s.MarkJobFailed(ctx, failed.ID, now.Add(-60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	cancelled, revived, err := s.SyncWantedJobs(ctx, nil, now.Add(-30*24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if cancelled != 0 || revived != 0 {
		t.Fatalf("empty counts = %d, %d; want 0, 0", cancelled, revived)
	}
	assertWantedSyncState(t, s, active.ID, core.StateDownloading)
	assertWantedSyncState(t, s, failed.ID, core.StateFailed)
}

// A manual job (NULL lidarr_album_id, source='manual') must be invisible to
// SyncWantedJobs: it is never cancelled by a snapshot that omits it, never
// revived (it can't be FAILED-and-in-wantedIDs since it was never wanted),
// and never metadata-refreshed. This is the key correctness guarantee behind
// bypassing WantedSync for manually created jobs (issue #155). Under the
// normal invariant (a manual job's lidarr_album_id stays NULL), the
// `= ANY`/`<> ALL` predicates already exclude the row structurally, since
// both are NULL (never TRUE) against a NULL lidarr_album_id - but that
// invariant was briefly broken for real in #59, which is why every predicate
// also carries an explicit `source = 'lidarr'` guard (#369, pinned by the
// four invariant-violating tests below) rather than relying on NULL alone.
func TestSyncWantedJobsIgnoresManualJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	manual, err := s.CreateManualJob(ctx, "Manual Album", "Manual Artist", "peer1", "",
		[]ManualJobFile{{Filename: "manual.flac", Size: 1}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}

	// A snapshot that mentions a completely different Lidarr album: if the
	// manual job were caught by the cancel predicate, it would be CANCELLED
	// here since its (nonexistent) lidarr_album_id is absent from wantedIDs.
	cancelled, revived, err := s.SyncWantedJobs(ctx,
		[]core.WantedRelease{{ID: 999, Title: "unrelated", ArtistName: "artist", ReleaseDate: "2026-01-01", ArtistID: 1}},
		now.Add(-30*24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SyncWantedJobs: %v", err)
	}
	if cancelled != 0 || revived != 0 {
		t.Fatalf("counts = cancelled %d, revived %d; want 0, 0 (manual job must not be touched)", cancelled, revived)
	}

	assertWantedSyncState(t, s, manual.ID, core.StateDownloading)
	var gotTitle, gotArtist string
	var gotUpdated time.Time
	if err := s.db.QueryRow(`SELECT title, artist_name, updated_at FROM album_jobs WHERE id=$1`, manual.ID).
		Scan(&gotTitle, &gotArtist, &gotUpdated); err != nil {
		t.Fatal(err)
	}
	if gotTitle != "Manual Album" || gotArtist != "Manual Artist" {
		t.Errorf("manual job metadata changed: title=%q artist=%q", gotTitle, gotArtist)
	}
	if !gotUpdated.Equal(now) {
		t.Errorf("manual job updated_at = %v, want unchanged %v", gotUpdated, now)
	}
}

// An empty wanted snapshot is non-authoritative (see SyncWantedJobs' doc
// comment) and returns before running any SQL, so a manual job is left
// completely untouched — this pins that guarantee alongside the
// cancel-predicate defense-in-depth added for issue #155.
func TestSyncWantedJobsEmptySnapshotIgnoresManualJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	manual, err := s.CreateManualJob(ctx, "Manual Album", "Manual Artist", "peer1", "",
		[]ManualJobFile{{Filename: "manual.flac", Size: 1}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}

	cancelled, revived, err := s.SyncWantedJobs(ctx, nil, now.Add(-30*24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SyncWantedJobs: %v", err)
	}
	if cancelled != 0 || revived != 0 {
		t.Fatalf("counts = cancelled %d, revived %d; want 0, 0 (manual job must not be touched)", cancelled, revived)
	}

	assertWantedSyncState(t, s, manual.ID, core.StateDownloading)
}

func TestSyncWantedJobsRollsBackWholeReconciliation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	created := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	existing, _ := s.UpsertWantedJob(ctx, 1, created)
	if _, err := s.db.ExecContext(ctx, `CREATE FUNCTION reject_wanted_refresh() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF NEW.lidarr_album_id = 1 AND NEW.title = 'explode' THEN
			RAISE EXCEPTION 'forced wanted refresh failure';
		END IF;
		RETURN NEW;
	END $$;
	CREATE TRIGGER reject_wanted_refresh BEFORE UPDATE ON album_jobs FOR EACH ROW EXECUTE FUNCTION reject_wanted_refresh()`); err != nil {
		t.Fatal(err)
	}

	_, _, err := s.SyncWantedJobs(ctx, []core.WantedRelease{{ID: 1, Title: "explode"}, {ID: 2, Title: "must roll back"}}, created.Add(-time.Hour), created.Add(time.Hour))
	if err == nil {
		t.Fatal("SyncWantedJobs unexpectedly succeeded")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM album_jobs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("job count after rollback = %d, want 1", count)
	}
	var title string
	if err := s.db.QueryRowContext(ctx, `SELECT title FROM album_jobs WHERE id=$1`, existing.ID).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "" {
		t.Errorf("existing metadata changed despite rollback: %q", title)
	}
}

// The four tests below deliberately violate the source/lidarr_album_id
// invariant that CreateManualJob normally upholds (a manual job's
// lidarr_album_id is always NULL) by writing a non-NULL value onto a manual
// row via raw SQL. That invariant was briefly broken for real during issue
// #59, and when it was, WantedSync predicates that matched on bare
// lidarr_album_id (with no source filter) treated the manual row as a Lidarr
// job — reviving/re-entering it and deleting its candidates and transfers
// (issue #369). These tests pin the predicates themselves, not the
// invariant, so they must keep passing even if the invariant is ever broken
// again.

// TestSyncWantedJobsRevivePredicateExcludesInvariantViolatingManualJob covers
// the "revived" CTE in SyncWantedJobs: a manual job stuck in FAILED with an
// old failed_at must not be revived just because some other code path wrote
// a wanted lidarr_album_id onto it.
func TestSyncWantedJobsRevivePredicateExcludesInvariantViolatingManualJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)

	manual, err := s.CreateManualJob(ctx, "Manual Album", "Manual Artist", "peer1", "",
		[]ManualJobFile{{Filename: "manual.flac", Size: 1}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	if err := s.MarkJobFailed(ctx, manual.ID, cutoff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Invariant violation: give the manual job a lidarr_album_id that is
	// about to appear in a wanted snapshot. CreateManualJob never does this;
	// only a bug (as in #59) can produce it.
	const collidingID int64 = 4242
	breakManualJobInvariant(t, s, manual.ID, collidingID)

	candidatesBefore, transfersBefore := countWantedSyncChildren(t, s, manual.ID)
	if candidatesBefore == 0 || transfersBefore == 0 {
		t.Fatalf("test setup: manual job has no children (candidates=%d transfers=%d)", candidatesBefore, transfersBefore)
	}

	_, revived, err := s.SyncWantedJobs(ctx,
		[]core.WantedRelease{{ID: collidingID, Title: "wanted", ArtistName: "artist", ReleaseDate: "2026-01-01", ArtistID: 1}},
		cutoff, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SyncWantedJobs: %v", err)
	}
	if revived != 0 {
		t.Fatalf("revived = %d, want 0 (manual job must not be revived)", revived)
	}
	assertWantedSyncState(t, s, manual.ID, core.StateFailed)

	candidatesAfter, transfersAfter := countWantedSyncChildren(t, s, manual.ID)
	if candidatesAfter != candidatesBefore || transfersAfter != transfersBefore {
		t.Errorf("manual job children were deleted: candidates %d->%d, transfers %d->%d", candidatesBefore, candidatesAfter, transfersBefore, transfersAfter)
	}
}

// TestSyncWantedJobsReenterPredicateExcludesInvariantViolatingManualJob
// covers the "reentered" CTE: a manual job stuck in CANCELLED must not be
// reset to WANTED (with its children wiped) by an invariant-violating
// lidarr_album_id.
func TestSyncWantedJobsReenterPredicateExcludesInvariantViolatingManualJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	manual, err := s.CreateManualJob(ctx, "Manual Album", "Manual Artist", "peer1", "",
		[]ManualJobFile{{Filename: "manual.flac", Size: 1}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	if err := s.AdvanceJobState(ctx, manual.ID, core.StateCancelled, now); err != nil {
		t.Fatal(err)
	}

	// Invariant violation, same as above.
	const collidingID int64 = 4243
	breakManualJobInvariant(t, s, manual.ID, collidingID)

	candidatesBefore, _ := countWantedSyncChildren(t, s, manual.ID)
	if candidatesBefore == 0 {
		t.Fatal("test setup: manual job has no candidate")
	}

	_, _, err = s.SyncWantedJobs(ctx,
		[]core.WantedRelease{{ID: collidingID, Title: "wanted", ArtistName: "artist", ReleaseDate: "2026-01-01", ArtistID: 1}},
		now.Add(-30*24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SyncWantedJobs: %v", err)
	}
	assertWantedSyncState(t, s, manual.ID, core.StateCancelled)

	candidatesAfter, _ := countWantedSyncChildren(t, s, manual.ID)
	if candidatesAfter != candidatesBefore {
		t.Errorf("manual job candidates were deleted: %d -> %d", candidatesBefore, candidatesAfter)
	}
}

// TestSyncWantedJobsMetadataPredicatesExcludeInvariantViolatingManualJobs
// covers both metadata statements: the WANTED-state refresh and the
// past-WANTED backfill must not touch a manual job's title/artist_name even
// when it carries an invariant-violating, wanted lidarr_album_id.
func TestSyncWantedJobsMetadataPredicatesExcludeInvariantViolatingManualJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// Backfill target: manual job with empty metadata, left in its natural
	// post-creation state (past WANTED - CreateManualJob starts DOWNLOADING).
	backfillTarget, err := s.CreateManualJob(ctx, "", "", "peer1", "",
		[]ManualJobFile{{Filename: "manual.flac", Size: 1}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	const backfillID int64 = 4244
	breakManualJobInvariant(t, s, backfillTarget.ID, backfillID)

	// Refresh target: manual job forced into WANTED. There is no manual-job
	// API that produces this state; it only exists here to exercise the
	// refresh predicate, alongside the same lidarr_album_id violation.
	refreshTarget, err := s.CreateManualJob(ctx, "old title", "old artist", "peer1", "",
		[]ManualJobFile{{Filename: "manual2.flac", Size: 1}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	const refreshID int64 = 4245
	refreshUpdatedAt := now
	if _, err := s.db.ExecContext(ctx, `UPDATE album_jobs SET lidarr_album_id = $1, state = $2, updated_at = $3 WHERE id = $4`,
		refreshID, string(core.StateWanted), refreshUpdatedAt, refreshTarget.ID); err != nil {
		t.Fatal(err)
	}

	releases := []core.WantedRelease{
		{ID: backfillID, Title: "snapshot title", ArtistName: "snapshot artist", ReleaseDate: "2026-01-01", ArtistID: 1},
		{ID: refreshID, Title: "new title", ArtistName: "new artist", ReleaseDate: "2026-02-01", ArtistID: 2},
	}
	if _, _, err := s.SyncWantedJobs(ctx, releases, now.Add(-30*24*time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatalf("SyncWantedJobs: %v", err)
	}

	var backfillTitle, backfillArtist string
	if err := s.db.QueryRowContext(ctx, `SELECT title, artist_name FROM album_jobs WHERE id = $1`, backfillTarget.ID).Scan(&backfillTitle, &backfillArtist); err != nil {
		t.Fatal(err)
	}
	if backfillTitle != "" || backfillArtist != "" {
		t.Errorf("backfill target metadata = %q/%q, want empty (untouched)", backfillTitle, backfillArtist)
	}

	var refreshTitle, refreshArtist string
	var refreshGotUpdated time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT title, artist_name, updated_at FROM album_jobs WHERE id = $1`, refreshTarget.ID).
		Scan(&refreshTitle, &refreshArtist, &refreshGotUpdated); err != nil {
		t.Fatal(err)
	}
	if refreshTitle != "old title" || refreshArtist != "old artist" {
		t.Errorf("refresh target metadata = %q/%q, want unchanged old title/artist", refreshTitle, refreshArtist)
	}
	if !refreshGotUpdated.Equal(refreshUpdatedAt) {
		t.Errorf("refresh target updated_at = %v, want unchanged %v", refreshGotUpdated, refreshUpdatedAt)
	}
}

// TestUpsertWantedJobIgnoresInvariantViolatingManualRow covers
// UpsertWantedJob: when a manual row carries an invariant-violating
// lidarr_album_id equal to the id being upserted, the function must still
// return the (newly inserted) lidarr-sourced row and must not touch the
// manual row's children.
func TestUpsertWantedJobIgnoresInvariantViolatingManualRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	manual, err := s.CreateManualJob(ctx, "Manual Album", "Manual Artist", "peer1", "",
		[]ManualJobFile{{Filename: "manual.flac", Size: 1}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	const collidingID int64 = 4246
	breakManualJobInvariant(t, s, manual.ID, collidingID)

	candidatesBefore, transfersBefore := countWantedSyncChildren(t, s, manual.ID)
	if candidatesBefore == 0 || transfersBefore == 0 {
		t.Fatalf("test setup: manual job has no children (candidates=%d transfers=%d)", candidatesBefore, transfersBefore)
	}

	got, err := s.UpsertWantedJob(ctx, collidingID, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if got.Source != core.SourceLidarr {
		t.Errorf("returned job source = %q, want %q", got.Source, core.SourceLidarr)
	}
	if got.ID == manual.ID {
		t.Errorf("returned job id = %d, same as colliding manual job", got.ID)
	}

	candidatesAfter, transfersAfter := countWantedSyncChildren(t, s, manual.ID)
	if candidatesAfter != candidatesBefore || transfersAfter != transfersBefore {
		t.Errorf("manual job children changed: candidates %d->%d, transfers %d->%d", candidatesBefore, candidatesAfter, transfersBefore, transfersAfter)
	}
}

// TestUpsertWantedJobReenterDoesNotWipeInvariantViolatingManualChildren covers
// the `reentered > 0` branch of UpsertWantedJob (the transfers/candidates
// DELETE at ~665/~670 in pipeline.go), which
// TestUpsertWantedJobIgnoresInvariantViolatingManualRow cannot reach: that
// test's manual job sits in DOWNLOADING, so the re-enter UPDATE (gated on
// state = CANCELLED) affects zero rows and the DELETEs never run. Reaching
// them needs a second, real source='lidarr' row sharing the same
// lidarr_album_id and sitting in CANCELLED, so UpsertWantedJob's re-enter
// path actually fires - only then do the DELETEs' `source = 'lidarr'` guard
// (#369) matter: without it, the subquery would also pick up the
// invariant-violating manual row by lidarr_album_id alone and wipe its
// children too.
func TestUpsertWantedJobReenterDoesNotWipeInvariantViolatingManualChildren(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	manual, err := s.CreateManualJob(ctx, "Manual Album", "Manual Artist", "peer1", "",
		[]ManualJobFile{{Filename: "manual.flac", Size: 1}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	const collidingID int64 = 4247
	breakManualJobInvariant(t, s, manual.ID, collidingID)

	// A real Lidarr job for the same album, put in CANCELLED so the
	// re-enter path in the next UpsertWantedJob call actually fires.
	lidarrJob, err := s.UpsertWantedJob(ctx, collidingID, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob (create lidarr job): %v", err)
	}
	if err := s.AdvanceJobState(ctx, lidarrJob.ID, core.StateCancelled, now); err != nil {
		t.Fatal(err)
	}

	candidatesBefore, transfersBefore := countWantedSyncChildren(t, s, manual.ID)
	if candidatesBefore == 0 || transfersBefore == 0 {
		t.Fatalf("test setup: manual job has no children (candidates=%d transfers=%d)", candidatesBefore, transfersBefore)
	}

	if _, err := s.UpsertWantedJob(ctx, collidingID, now.Add(time.Minute)); err != nil {
		t.Fatalf("UpsertWantedJob (re-enter): %v", err)
	}

	// The re-enter path must have fired on the lidarr job, not the manual one.
	assertWantedSyncState(t, s, lidarrJob.ID, core.StateWanted)
	assertWantedSyncState(t, s, manual.ID, core.StateDownloading)

	candidatesAfter, transfersAfter := countWantedSyncChildren(t, s, manual.ID)
	if candidatesAfter != candidatesBefore || transfersAfter != transfersBefore {
		t.Errorf("manual job children were deleted: candidates %d->%d, transfers %d->%d", candidatesBefore, candidatesAfter, transfersBefore, transfersAfter)
	}
}

// breakManualJobInvariant writes a state CreateManualJob can never produce: a
// non-NULL lidarr_album_id on a manual (source='manual') row. It exists only
// so the invariant-violating tests above and below can reproduce the #59
// scenario that motivated the #369 source = 'lidarr' guards.
func breakManualJobInvariant(t *testing.T, s *Store, jobID, albumID int64) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `UPDATE album_jobs SET lidarr_album_id = $1 WHERE id = $2`, albumID, jobID); err != nil {
		t.Fatal(err)
	}
}

func countWantedSyncChildren(t *testing.T, s *Store, jobID int64) (candidates, transfers int) {
	t.Helper()
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM candidates WHERE album_job_id = $1`, jobID).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM transfers WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id = $1)`, jobID).Scan(&transfers); err != nil {
		t.Fatal(err)
	}
	return candidates, transfers
}

func seedWantedSyncChild(t *testing.T, s *Store, jobID int64, now time.Time, filename string) int64 {
	t.Helper()
	var candidateID int64
	if err := s.db.QueryRow(`INSERT INTO candidates (album_job_id, username, score, files, state, created_at, updated_at)
		VALUES ($1, 'peer', 1, '[]', 'NEW', $2, $2) RETURNING id`, jobID, now).Scan(&candidateID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPendingTransfer(context.Background(), candidateID, "peer", filename, 1, now); err != nil {
		t.Fatal(err)
	}
	return candidateID
}

func assertWantedSyncState(t *testing.T, s *Store, jobID int64, want core.AlbumJobState) {
	t.Helper()
	var state string
	if err := s.db.QueryRow(`SELECT state FROM album_jobs WHERE id=$1`, jobID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if core.AlbumJobState(state) != want {
		t.Errorf("job %d state = %s, want %s", jobID, state, want)
	}
}

func assertWantedSyncEmptySearches(t *testing.T, s *Store, jobID int64, want int) {
	t.Helper()
	var got int
	if err := s.db.QueryRow(`SELECT empty_searches FROM album_jobs WHERE id=$1`, jobID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("job %d empty_searches = %d, want %d", jobID, got, want)
	}
}

func assertWantedSyncJob(t *testing.T, s *Store, jobID int64, state core.AlbumJobState, title, artist, releaseDate string, artistID int64, updatedAt time.Time) {
	t.Helper()
	var gotState, gotTitle, gotArtist, gotDate string
	var gotArtistID int64
	var gotUpdated time.Time
	if err := s.db.QueryRow(`SELECT state, title, artist_name, release_date, artist_id, updated_at FROM album_jobs WHERE id=$1`, jobID).
		Scan(&gotState, &gotTitle, &gotArtist, &gotDate, &gotArtistID, &gotUpdated); err != nil {
		t.Fatal(err)
	}
	if core.AlbumJobState(gotState) != state || gotTitle != title || gotArtist != artist || gotDate != releaseDate || gotArtistID != artistID || !gotUpdated.Equal(updatedAt) {
		t.Errorf("job %d = state %s metadata %q/%q/%q/%d updated %v; want %s %q/%q/%q/%d %v",
			jobID, gotState, gotTitle, gotArtist, gotDate, gotArtistID, gotUpdated, state, title, artist, releaseDate, artistID, updatedAt)
	}
}
