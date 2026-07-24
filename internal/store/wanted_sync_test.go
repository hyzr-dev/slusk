package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
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

	oldFailed, _ := s.UpsertWantedJob(ctx, 6, created)
	failedCandidate := seedWantedSyncChild(t, s, oldFailed.ID, created, "failed-child")
	if err := s.MarkJobFailed(ctx, oldFailed.ID, cutoff.Add(-time.Microsecond)); err != nil {
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
// bypassing WantedSync for manually created jobs (issue #155): every
// SQL predicate here compares lidarr_album_id with `= ANY`/`<> ALL`, both of
// which are NULL (never TRUE) for a NULL lidarr_album_id, so the row is
// structurally excluded rather than needing an explicit guard.
func TestSyncWantedJobsIgnoresManualJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	manual, err := s.CreateManualJob(ctx, "Manual Album", "Manual Artist", "peer1",
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
