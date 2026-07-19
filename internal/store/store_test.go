package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

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

func TestOpenContextHonorsCancelledStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := OpenContext(ctx, "postgres://127.0.0.1:1/unreachable?sslmode=disable"); err == nil {
		t.Fatal("OpenContext returned nil error for cancelled startup")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled OpenContext took %v", elapsed)
	}
}

func TestOpenAppliesSchema(t *testing.T) {
	s := newTestStore(t)

	// Check that all three core tables exist
	tables := []string{"album_jobs", "candidates", "transfers"}
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

	// Try to insert a candidates row with a non-existent album_job_id.
	// The foreign key constraint must reject it.
	_, err := s.db.Exec(
		`INSERT INTO candidates (album_job_id, username, score, files, state, created_at, updated_at)
		 VALUES (999, 'testuser', 1.0, '[]', 'NEW', now(), now())`,
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
// subqueries full-scanned candidates/transfers once per album_jobs row,
// which at a few thousand jobs made a single /api/jobs request take tens of
// seconds of CPU — and the dashboard polls it every 3 seconds, so requests
// piled up and pinned multiple cores (the 300%-CPU incident).
//
// This asserts index existence rather than inspecting the query plan (the old
// SQLite version used EXPLAIN QUERY PLAN): Postgres deliberately seq-scans
// small tables even when an index exists, so a plan assertion on an empty test
// database would be flaky and meaningless.
// TestOpenRecyclesIdleConnections guards against a pooled connection sitting
// idle long enough for the network path to silently kill it (e.g. Docker
// Swarm's overlay network dropping an idle mapping with no FIN/RST reaching
// either side): with no idle-recycling policy, the next query on that
// connection blocks forever, since neither the OS nor database/sql notices a
// connection that looks fine but no longer delivers bytes. Open must configure
// a connection max idle time so idle connections are proactively replaced
// well before that can happen.
func TestOpenRecyclesIdleConnections(t *testing.T) {
	s, err := openWithLimits(storetest.DSN(t), 10*time.Millisecond, time.Hour)
	if err != nil {
		t.Fatalf("openWithLimits: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.db.Exec("SELECT 1"); err != nil {
		t.Fatalf("first query: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for s.db.Stats().MaxIdleTimeClosed == 0 {
		if time.Now().After(deadline) {
			t.Fatal("expected at least one connection closed due to ConnMaxIdleTime, got 0 - idle connections are never recycled")
		}
		time.Sleep(20 * time.Millisecond) // let the connection sit idle past the limit
		if _, err := s.db.Exec("SELECT 1"); err != nil {
			t.Fatalf("query: %v", err)
		}
	}
}

func TestJobViewIndexesExist(t *testing.T) {
	s := newTestStore(t)

	for _, idx := range []string{"idx_candidates_job", "idx_candidates_job_created", "idx_transfers_candidate"} {
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

// TestSplitStatements exercises the naive semicolon-splitting parser
// applyOneMigration relies on to break a migration file's SQL text into
// individually-executable statements.
func TestSplitStatements(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "plain statements split on semicolon",
			input: "CREATE TABLE a (id INT);\nCREATE TABLE b (id INT);",
			want: []string{
				"CREATE TABLE a (id INT)",
				"\nCREATE TABLE b (id INT)",
			},
		},
		{
			name: "DO block kept as one statement despite internal semicolons",
			input: "DO $$ BEGIN\n" +
				"    IF EXISTS (SELECT 1) THEN\n" +
				"        ALTER TABLE t RENAME COLUMN a TO b;\n" +
				"    END IF;\n" +
				"END $$;",
			want: []string{
				"DO $$ BEGIN\n" +
					"    IF EXISTS (SELECT 1) THEN\n" +
					"        ALTER TABLE t RENAME COLUMN a TO b;\n" +
					"    END IF;\n" +
					"END $$",
			},
		},
		{
			name: "two DO blocks with a plain statement between them",
			input: "DO $$ BEGIN X; END $$;\n" +
				"SELECT 1;\n" +
				"DO $$ BEGIN Y; END $$;",
			want: []string{
				"DO $$ BEGIN X; END $$",
				"\nSELECT 1",
				"\nDO $$ BEGIN Y; END $$",
			},
		},
		{
			name:  "line comment containing a semicolon does not split the statement",
			input: "-- a comment; with a semicolon inside it\nSELECT 1;",
			want: []string{
				"-- a comment; with a semicolon inside it\nSELECT 1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitStatements(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitStatements(%q) = %d statements, want %d\ngot: %#v", tt.input, len(got), len(tt.want), got)
			}
			for i := range got {
				if strings.TrimSpace(got[i]) != strings.TrimSpace(tt.want[i]) {
					t.Errorf("statement %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestSchemaMigratesLegacyShape verifies schema.sql boots cleanly against a
// pre-rewrite database: transfers still named attempt_id with an inline FK to
// candidate_attempts and the old idx_transfers_attempt index. The clean-slate
// script only truncates - it does not drop - so production databases hit
// exactly this shape on first post-rewrite boot. The apply must run the
// attempt_id → candidate_id migration BEFORE any statement that references
// candidate_id by name: IF NOT EXISTS only guards names, Postgres still
// resolves the column list.
func TestSchemaMigratesLegacyShape(t *testing.T) {
	dsn := storetest.DSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE album_jobs (
			id    BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			state TEXT NOT NULL
		)`,
		`CREATE TABLE candidate_attempts (
			id           BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			album_job_id BIGINT NOT NULL REFERENCES album_jobs(id)
		)`,
		`CREATE TABLE transfers (
			id         BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			attempt_id BIGINT NOT NULL REFERENCES candidate_attempts(id),
			slskd_id   TEXT NOT NULL DEFAULT '',
			username   TEXT NOT NULL,
			filename   TEXT NOT NULL,
			state      TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (username, filename)
		)`,
		`CREATE INDEX idx_attempts_job ON candidate_attempts(album_job_id)`,
		`CREATE INDEX idx_transfers_attempt ON transfers(attempt_id, updated_at)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create legacy shape: %v\nstatement: %s", err, stmt)
		}
	}

	s, err := Open(dsn) // applies schema.sql against the legacy shape
	if err != nil {
		t.Fatalf("Open against legacy-shape database: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var count int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'transfers' AND column_name = 'candidate_id'`,
	).Scan(&count); err != nil {
		t.Fatalf("query transfers columns: %v", err)
	}
	if count != 1 {
		t.Error("transfers.attempt_id was not renamed to candidate_id")
	}
	if err := s.db.QueryRow(
		`SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'idx_transfers_candidate'`,
	).Scan(&count); err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	if count != 1 {
		t.Error("idx_transfers_candidate missing after migrating legacy shape")
	}
}

// TestSchemaMigratesGlobalTransferUniquenessToCandidateOwnership verifies
// migration 0001 fixes up a database that predates candidate-scoped transfer
// ownership: transfers already renamed to candidate_id, but still carrying
// the old globally-unique (username, filename) constraint. Unlike
// TestSchemaMigratesLegacyShape (attempt_id era), the drift is installed on a
// fresh database *before* Migrate ever runs, since once migration 0001 is
// recorded in schema_migrations it will never be re-applied to heal drift -
// that is the whole point of switching to versioned migrations.
func TestSchemaMigratesGlobalTransferUniquenessToCandidateOwnership(t *testing.T) {
	dsn := storetest.DSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE album_jobs (
			id    BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			state TEXT NOT NULL
		)`,
		`CREATE TABLE candidates (
			id           BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			album_job_id BIGINT NOT NULL REFERENCES album_jobs(id),
			state        TEXT NOT NULL,
			created_at   TIMESTAMPTZ NOT NULL
		)`,
		`CREATE TABLE transfers (
			id           BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			candidate_id BIGINT NOT NULL,
			slskd_id     TEXT NOT NULL DEFAULT '',
			username     TEXT NOT NULL,
			filename     TEXT NOT NULL,
			state        TEXT NOT NULL,
			updated_at   TIMESTAMPTZ NOT NULL,
			CONSTRAINT transfers_username_filename_key UNIQUE (username, filename)
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create pre-scoped-ownership shape: %v\nstatement: %s", err, stmt)
		}
	}

	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate against pre-scoped-ownership shape: %v", err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	var oldCount, scopedCount, liveIndexCount int
	if err := db.QueryRow(`SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'transfers'::regclass AND conname = 'transfers_username_filename_key'`).Scan(&oldCount); err != nil {
		t.Fatalf("query old transfer constraint: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'transfers'::regclass AND conname = 'transfers_candidate_username_filename_key'`).Scan(&scopedCount); err != nil {
		t.Fatalf("query scoped transfer constraint: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = 'idx_transfers_live_remote_owner'`).Scan(&liveIndexCount); err != nil {
		t.Fatalf("query live-owner transfer index: %v", err)
	}
	if oldCount != 0 || scopedCount != 1 || liveIndexCount != 1 {
		t.Errorf("transfer keys after migration: old=%d scoped=%d live_index=%d, want 0/1/1", oldCount, scopedCount, liveIndexCount)
	}
}

// TestMigrateTwiceIsIdempotent makes explicit that running Migrate against a
// database that has already had it applied (Open already runs Migrate once)
// is safe and applies nothing further.
func TestMigrateTwiceIsIdempotent(t *testing.T) {
	s := newTestStore(t) // Open already ran Migrate once

	if err := Migrate(context.Background(), s.db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	for _, table := range []string{"album_jobs", "candidates", "transfers"} {
		var count int
		if err := s.db.QueryRow(
			`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1`,
			table,
		).Scan(&count); err != nil {
			t.Fatalf("query schema for %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("%s table missing after re-running Migrate", table)
		}
	}
}

// TestMigrateAppliesEachVersionExactlyOnce runs Migrate twice against a fresh
// database and asserts schema_migrations ends up with exactly one row per
// embedded migration version, with no duplicates, after both runs.
func TestMigrateAppliesEachVersionExactlyOnce(t *testing.T) {
	s := newTestStore(t) // Open already ran Migrate once

	routine, _, err := loadMigrations(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}

	if err := Migrate(context.Background(), s.db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	var rowCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&rowCount); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if rowCount != len(routine) {
		t.Errorf("schema_migrations has %d rows, want %d (one per embedded routine migration)", rowCount, len(routine))
	}

	var distinctCount int
	if err := s.db.QueryRow(`SELECT count(DISTINCT version) FROM schema_migrations`).Scan(&distinctCount); err != nil {
		t.Fatalf("query distinct schema_migrations versions: %v", err)
	}
	if distinctCount != rowCount {
		t.Errorf("schema_migrations has %d rows but only %d distinct versions - duplicates present", rowCount, distinctCount)
	}
}

// TestMigrateFailedMigrationRollsBackCleanly injects a broken final migration
// into a test fs.FS and asserts: Migrate (via migrateFromFS) returns an
// error, no schema_migrations row exists for that failed version, and no
// partial DDL from that migration's earlier statements persisted.
func TestMigrateFailedMigrationRollsBackCleanly(t *testing.T) {
	s := newTestStore(t)

	fsys := fstest.MapFS{
		"migrations/0001_ok.sql": &fstest.MapFile{
			Data: []byte(`CREATE TABLE IF NOT EXISTS broken_migration_ok (id BIGINT)`),
		},
		"migrations/0002_broken.sql": &fstest.MapFile{
			Data: []byte(`CREATE TABLE IF NOT EXISTS broken_migration_partial (id BIGINT);
THIS IS NOT VALID SQL;`),
		},
	}

	err := migrateFromFS(context.Background(), s.db, fsys, "migrations", false, nil)
	if err == nil {
		t.Fatal("expected error from migration with invalid SQL, got nil")
	}

	var version2Count int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = 2`).Scan(&version2Count); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if version2Count != 0 {
		t.Errorf("schema_migrations has a row for the failed version 2 migration")
	}

	var partialTableCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'broken_migration_partial'`,
	).Scan(&partialTableCount); err != nil {
		t.Fatalf("query information_schema.tables: %v", err)
	}
	if partialTableCount != 0 {
		t.Error("broken_migration_partial table exists - partial DDL from the failed migration was not rolled back")
	}
}

// TestMigrateSkipsDestructiveByDefault builds a test fs.FS containing a
// "_destructive.sql" migration and asserts the routine (non-destructive)
// apply path never applies or records it.
func TestMigrateSkipsDestructiveByDefault(t *testing.T) {
	s := newTestStore(t)

	fsys := fstest.MapFS{
		"migrations/9999_create_marker_destructive.sql": &fstest.MapFile{
			Data: []byte(`CREATE TABLE IF NOT EXISTS destructive_marker_table (id BIGINT)`),
		},
	}

	if err := migrateFromFS(context.Background(), s.db, fsys, "migrations", false, nil); err != nil {
		t.Fatalf("routine migrateFromFS: %v", err)
	}

	var tableCount, recordedCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'destructive_marker_table'`,
	).Scan(&tableCount); err != nil {
		t.Fatalf("query information_schema.tables: %v", err)
	}
	if tableCount != 0 {
		t.Error("destructive migration was applied by the routine (non-destructive) path")
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = 9999`).Scan(&recordedCount); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if recordedCount != 0 {
		t.Error("destructive migration was recorded in schema_migrations by the routine (non-destructive) path")
	}
}

// TestApplyDestructiveAppliesExplicitly is the counterpart to
// TestMigrateSkipsDestructiveByDefault: calling the destructive-apply path
// directly against the same kind of fixture must apply it and record it.
func TestApplyDestructiveAppliesExplicitly(t *testing.T) {
	s := newTestStore(t)

	fsys := fstest.MapFS{
		"migrations/9999_create_marker_destructive.sql": &fstest.MapFile{
			Data: []byte(`CREATE TABLE IF NOT EXISTS destructive_marker_table (id BIGINT)`),
		},
	}

	if err := migrateFromFS(context.Background(), s.db, fsys, "migrations", true, nil); err != nil {
		t.Fatalf("destructive migrateFromFS: %v", err)
	}

	var tableCount, recordedCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'destructive_marker_table'`,
	).Scan(&tableCount); err != nil {
		t.Fatalf("query information_schema.tables: %v", err)
	}
	if tableCount != 1 {
		t.Error("destructive migration was not applied by the explicit destructive-apply path")
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = 9999`).Scan(&recordedCount); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if recordedCount != 1 {
		t.Error("destructive migration was not recorded in schema_migrations by the explicit destructive-apply path")
	}
}

// TestLoadMigrationsRejectsDuplicateVersions asserts discovery returns an
// error - not a panic - when two migration filenames parse to the same
// version.
func TestLoadMigrationsRejectsDuplicateVersions(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_first.sql":  &fstest.MapFile{Data: []byte(`SELECT 1`)},
		"migrations/0001_second.sql": &fstest.MapFile{Data: []byte(`SELECT 1`)},
	}
	if _, _, err := loadMigrations(fsys, "migrations"); err == nil {
		t.Fatal("expected error for duplicate migration versions, got nil")
	}
}

// TestLoadMigrationsRejectsMalformedFilename asserts discovery returns an
// error - not a panic - for a filename that does not match the
// "%04d_description.sql" convention.
func TestLoadMigrationsRejectsMalformedFilename(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/not_a_valid_name.sql": &fstest.MapFile{Data: []byte(`SELECT 1`)},
	}
	if _, _, err := loadMigrations(fsys, "migrations"); err == nil {
		t.Fatal("expected error for malformed migration filename, got nil")
	}
}
