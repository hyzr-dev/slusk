package store

import (
	"os"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/store/storetest"
)

// TestMain starts one embedded Postgres instance for the whole package; each
// test gets its own database via newTestStore.
func TestMain(m *testing.M) {
	os.Exit(storetest.Run(m))
}

// newTestStore opens a store against a fresh per-test database, closed automatically.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(storetest.DSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenAppliesSchema(t *testing.T) {
	s := newTestStore(t)

	// Check that all three core tables exist
	tables := []string{"album_jobs", "candidate_attempts", "transfers"}
	for _, table := range tables {
		var count int
		err := s.db.QueryRow(
			`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1`,
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
	// The foreign key constraint must reject it.
	_, err := s.db.Exec(
		`INSERT INTO candidate_attempts (album_job_id, username, score, state, created_at, updated_at)
		 VALUES (999, 'testuser', 1.0, 'PENDING', now(), now())`,
	)
	if err == nil {
		t.Fatal("expected foreign key violation, got nil error")
	}
}

func TestSchemaHasTitleAndArtistColumns(t *testing.T) {
	s := newTestStore(t)

	cols := map[string]bool{}
	rows, err := s.db.Query(
		`SELECT column_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'album_jobs'`)
	if err != nil {
		t.Fatalf("query columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column row: %v", err)
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

// TestJobViewIndexesExist guards against losing the covering indexes for the
// dashboard's ListJobsWithTransfer correlated subqueries. Without them those
// subqueries full-scanned candidate_attempts/transfers once per album_jobs row,
// which at a few thousand jobs made a single /api/jobs request take tens of
// seconds of CPU — and the dashboard polls it every 3 seconds, so requests
// piled up and pinned multiple cores (the 300%-CPU incident).
//
// This asserts index existence rather than inspecting the query plan (the old
// SQLite version used EXPLAIN QUERY PLAN): Postgres deliberately seq-scans
// small tables even when an index exists, so a plan assertion on an empty test
// database would be flaky and meaningless.
func TestJobViewIndexesExist(t *testing.T) {
	s := newTestStore(t)

	for _, idx := range []string{"idx_attempts_job", "idx_transfers_attempt"} {
		var count int
		if err := s.db.QueryRow(
			`SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, idx,
		).Scan(&count); err != nil {
			t.Fatalf("query pg_indexes for %s: %v", idx, err)
		}
		if count != 1 {
			t.Errorf("index %s missing (dashboard job view would full-scan per job row)", idx)
		}
	}
}
