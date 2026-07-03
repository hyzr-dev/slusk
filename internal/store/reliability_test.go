package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestRecordAttemptOutcomeUpsertsGlobalAndArtistScopes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	if err := s.RecordAttemptOutcome(ctx, 100, "reliable_peer", true, t1); err != nil {
		t.Fatalf("RecordAttemptOutcome success: %v", err)
	}
	if err := s.RecordAttemptOutcome(ctx, 100, "reliable_peer", false, t2); err != nil {
		t.Fatalf("RecordAttemptOutcome fail: %v", err)
	}

	rel, err := s.ReliabilityFor(ctx, 100, []string{"reliable_peer"})
	if err != nil {
		t.Fatalf("ReliabilityFor: %v", err)
	}
	pr, ok := rel["reliable_peer"]
	if !ok {
		t.Fatalf("expected reliable_peer in result, got %+v", rel)
	}
	if pr.Artist.SuccessCount != 1 || pr.Artist.FailCount != 1 {
		t.Errorf("artist counters = %+v, want success=1 fail=1", pr.Artist)
	}
	if pr.Artist.LastSuccessAt == nil || !pr.Artist.LastSuccessAt.Equal(t1) {
		t.Errorf("artist LastSuccessAt = %v, want %v", pr.Artist.LastSuccessAt, t1)
	}
	if pr.Artist.LastFailAt == nil || !pr.Artist.LastFailAt.Equal(t2) {
		t.Errorf("artist LastFailAt = %v, want %v", pr.Artist.LastFailAt, t2)
	}
	if pr.Global.SuccessCount != 1 || pr.Global.FailCount != 1 {
		t.Errorf("global counters = %+v, want success=1 fail=1", pr.Global)
	}
}

func TestRecordAttemptOutcomeSeparatesArtistScopes(t *testing.T) {
	// The same peer succeeding for one artist and failing for another must keep
	// each artist's row independent (both feed the shared global row, but a
	// candidate search for artist A must not see artist B's fail history as
	// artist-specific).
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if err := s.RecordAttemptOutcome(ctx, 1, "peer", true, now); err != nil {
		t.Fatalf("record success for artist 1: %v", err)
	}
	if err := s.RecordAttemptOutcome(ctx, 2, "peer", false, now); err != nil {
		t.Fatalf("record fail for artist 2: %v", err)
	}

	rel1, err := s.ReliabilityFor(ctx, 1, []string{"peer"})
	if err != nil {
		t.Fatalf("ReliabilityFor artist 1: %v", err)
	}
	if rel1["peer"].Artist.SuccessCount != 1 || rel1["peer"].Artist.FailCount != 0 {
		t.Errorf("artist 1 counters = %+v, want success=1 fail=0", rel1["peer"].Artist)
	}

	rel2, err := s.ReliabilityFor(ctx, 2, []string{"peer"})
	if err != nil {
		t.Fatalf("ReliabilityFor artist 2: %v", err)
	}
	if rel2["peer"].Artist.SuccessCount != 0 || rel2["peer"].Artist.FailCount != 1 {
		t.Errorf("artist 2 counters = %+v, want success=0 fail=1", rel2["peer"].Artist)
	}

	// Both artists still see the same shared global row.
	if rel1["peer"].Global.SuccessCount != 1 || rel1["peer"].Global.FailCount != 1 {
		t.Errorf("global counters via artist 1 lookup = %+v, want success=1 fail=1", rel1["peer"].Global)
	}
}

func TestRecordAttemptOutcomeSkipsArtistRowWhenArtistIDUnknown(t *testing.T) {
	// artistID <= 0 means the job's artist_id hasn't been backfilled yet (see
	// core.AlbumJob.ArtistID doc comment). The outcome must still be recorded
	// globally, but no artist_user_reliability row should be written for the
	// sentinel "unknown" artist.
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if err := s.RecordAttemptOutcome(ctx, 0, "peer", true, now); err != nil {
		t.Fatalf("RecordAttemptOutcome: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM artist_user_reliability`).Scan(&count); err != nil {
		t.Fatalf("count artist_user_reliability: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no artist_user_reliability rows for unknown artist, got %d", count)
	}

	rel, err := s.ReliabilityFor(ctx, 0, []string{"peer"})
	if err != nil {
		t.Fatalf("ReliabilityFor: %v", err)
	}
	if rel["peer"].Global.SuccessCount != 1 {
		t.Errorf("global success count = %d, want 1", rel["peer"].Global.SuccessCount)
	}
}

func TestReliabilityForOmitsUsernamesWithNoHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if err := s.RecordAttemptOutcome(ctx, 1, "known", true, now); err != nil {
		t.Fatalf("RecordAttemptOutcome: %v", err)
	}

	rel, err := s.ReliabilityFor(ctx, 1, []string{"known", "unknown"})
	if err != nil {
		t.Fatalf("ReliabilityFor: %v", err)
	}
	if _, ok := rel["unknown"]; ok {
		t.Errorf("expected 'unknown' to be absent from the result, got %+v", rel["unknown"])
	}
	if _, ok := rel["known"]; !ok {
		t.Errorf("expected 'known' present in the result")
	}
}

// TestOpenMigratesPreExistingDBMissingReliabilityTables reproduces opening a
// database created before artist_id and the known_users/artist_user_reliability
// tables existed: Open must add the column and create the tables rather than
// failing, and RecordAttemptOutcome/ReliabilityFor must work immediately after.
func TestOpenMigratesPreExistingDBMissingReliabilityTables(t *testing.T) {
	path := t.TempDir() + "/legacy_reliability.db"

	legacyDB, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`CREATE TABLE album_jobs (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		lidarr_album_id  INTEGER NOT NULL,
		state            TEXT NOT NULL,
		candidates_tried INTEGER NOT NULL DEFAULT 0,
		next_attempt_at  DATETIME,
		created_at       DATETIME NOT NULL,
		updated_at       DATETIME NOT NULL,
		title            TEXT NOT NULL DEFAULT '',
		artist_name      TEXT NOT NULL DEFAULT '',
		release_date     TEXT NOT NULL DEFAULT '',
		UNIQUE(lidarr_album_id)
	)`); err != nil {
		t.Fatalf("create legacy album_jobs: %v", err)
	}
	if _, err := legacyDB.Exec(`CREATE TABLE candidate_attempts (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		album_job_id  INTEGER NOT NULL REFERENCES album_jobs(id),
		username      TEXT NOT NULL,
		score         REAL NOT NULL,
		state         TEXT NOT NULL,
		fail_reason   TEXT NOT NULL DEFAULT '',
		backoff_until DATETIME,
		created_at    DATETIME NOT NULL,
		updated_at    DATETIME NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy candidate_attempts: %v", err)
	}
	if _, err := legacyDB.Exec(`CREATE TABLE transfers (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		attempt_id       INTEGER NOT NULL REFERENCES candidate_attempts(id),
		slskd_id         TEXT NOT NULL DEFAULT '',
		username         TEXT NOT NULL,
		filename         TEXT NOT NULL,
		state            TEXT NOT NULL,
		bytes_done       INTEGER NOT NULL DEFAULT 0,
		bytes_total      INTEGER NOT NULL DEFAULT 0,
		retries          INTEGER NOT NULL DEFAULT 0,
		deadline         DATETIME,
		last_progress_at DATETIME,
		updated_at       DATETIME NOT NULL,
		UNIQUE(username, filename)
	)`); err != nil {
		t.Fatalf("create legacy transfers: %v", err)
	}
	if _, err := legacyDB.Exec(
		`INSERT INTO album_jobs (lidarr_album_id, state, created_at, updated_at) VALUES (1, 'DISCOVERED', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seed album_jobs: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on pre-migration db: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := s.RecordAttemptOutcome(ctx, 5, "migrated_peer", true, now); err != nil {
		t.Fatalf("RecordAttemptOutcome after migration: %v", err)
	}
	rel, err := s.ReliabilityFor(ctx, 5, []string{"migrated_peer"})
	if err != nil {
		t.Fatalf("ReliabilityFor after migration: %v", err)
	}
	if rel["migrated_peer"].Artist.SuccessCount != 1 {
		t.Errorf("artist success count = %d, want 1", rel["migrated_peer"].Artist.SuccessCount)
	}

	var artistID int64
	if err := s.db.QueryRow(`SELECT artist_id FROM album_jobs WHERE id = 1`).Scan(&artistID); err != nil {
		t.Fatalf("select migrated artist_id column: %v", err)
	}
	if artistID != 0 {
		t.Errorf("artist_id = %d, want backfilled default 0", artistID)
	}
}
