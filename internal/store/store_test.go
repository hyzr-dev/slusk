package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// newTestStore opens a fresh store in a temp dir, closed automatically.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenAppliesSchema(t *testing.T) {
	s := newTestStore(t)

	// Check that all three required tables exist
	tables := []string{"album_jobs", "candidate_attempts", "transfers"}
	for _, table := range tables {
		var count int
		err := s.db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`,
			table,
		).Scan(&count)
		if err != nil {
			t.Fatalf("query schema for %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("%s table not created", table)
		}
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s := newTestStore(t)

	// Try to insert a candidate_attempts row with a non-existent album_job_id.
	// With foreign key enforcement enabled, this should fail.
	_, err := s.db.Exec(
		`INSERT INTO candidate_attempts (album_job_id, username, score, state, created_at)
		 VALUES (999, 'testuser', 1.0, 'PENDING', datetime('now'))`,
	)
	if err == nil {
		t.Fatal("expected foreign key violation, got nil error")
	}
}

func TestSchemaHasTitleAndArtistColumns(t *testing.T) {
	s := newTestStore(t)

	cols := map[string]bool{}
	rows, err := s.db.Query(`PRAGMA table_info(album_jobs)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma row: %v", err)
		}
		cols[name] = true
	}
	if !cols["title"] {
		t.Error("album_jobs missing title column")
	}
	if !cols["artist_name"] {
		t.Error("album_jobs missing artist_name column")
	}
}

// TestOpenMigratesPreExistingDBMissingUpdatedAt reproduces opening a database
// created before candidate_attempts had an updated_at column: Open must add
// the column and backfill existing rows from created_at, rather than failing.
func TestOpenMigratesPreExistingDBMissingUpdatedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Build a pre-migration candidate_attempts table (no updated_at) and seed
	// a row, simulating a database written before this migration existed.
	legacyDB, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	createdAt := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := legacyDB.Exec(`CREATE TABLE album_jobs (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		lidarr_album_id  INTEGER NOT NULL,
		state            TEXT NOT NULL,
		candidates_tried INTEGER NOT NULL DEFAULT 0,
		next_attempt_at  DATETIME,
		created_at       DATETIME NOT NULL,
		updated_at       DATETIME NOT NULL,
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
		created_at    DATETIME NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy candidate_attempts: %v", err)
	}
	if _, err := legacyDB.Exec(
		`INSERT INTO album_jobs (lidarr_album_id, state, created_at, updated_at) VALUES (1, 'DISCOVERED', ?, ?)`,
		createdAt, createdAt); err != nil {
		t.Fatalf("seed album_jobs: %v", err)
	}
	if _, err := legacyDB.Exec(
		`INSERT INTO candidate_attempts (album_job_id, username, score, state, created_at) VALUES (1, 'legacy_peer', 1.0, 'PENDING', ?)`,
		createdAt); err != nil {
		t.Fatalf("seed candidate_attempts: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	// Opening via the store must migrate the column in rather than erroring.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on pre-migration db: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var updatedAt time.Time
	if err := s.db.QueryRow(`SELECT updated_at FROM candidate_attempts WHERE username = 'legacy_peer'`).Scan(&updatedAt); err != nil {
		t.Fatalf("select updated_at: %v", err)
	}
	if !updatedAt.Equal(createdAt) {
		t.Errorf("updated_at = %v, want backfilled to created_at %v", updatedAt, createdAt)
	}

	// The store must remain fully usable after migration (e.g. FailAttempt's
	// updated_at write works against the migrated column).
	if err := s.FailAttempt(context.Background(), 1, "timeout", createdAt.Add(time.Hour), createdAt.Add(time.Minute)); err != nil {
		t.Fatalf("FailAttempt after migration: %v", err)
	}
}
