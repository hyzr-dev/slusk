package store

import (
	"path/filepath"
	"testing"
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
