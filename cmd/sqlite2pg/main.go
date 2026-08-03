// Command sqlite2pg is a one-off migration tool that transforms the final
// released slusk SQLite schema into the current PostgreSQL schema. It
// validates the source shape before touching the target, applies the current
// store schema, refuses to run if any target table already has rows, and copies
// all supported data preserving primary keys and relationships.
//
// Usage:
//
//	sqlite2pg -sqlite /data/slusk.db -pg 'postgres://slusk:password@localhost:5432/slusk?sslmode=disable'
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/samuelenocsson/slusk/internal/store"
)

type tableSpec struct {
	name    string
	columns []string
}

// sourceTables is the final SQLite schema shipped immediately before the
// PostgreSQL port. Exact validation is intentional: guessing at an older or
// newer shape could silently omit columns that this one-off tool does not know
// how to represent.
var sourceTables = []tableSpec{
	{"album_jobs", []string{"id", "lidarr_album_id", "state", "candidates_tried", "next_attempt_at", "created_at", "updated_at", "title", "artist_name", "release_date", "artist_id"}},
	{"candidate_attempts", []string{"id", "album_job_id", "username", "score", "state", "fail_reason", "backoff_until", "created_at", "updated_at"}},
	{"transfers", []string{"id", "attempt_id", "slskd_id", "username", "filename", "state", "bytes_done", "bytes_total", "deadline", "last_progress_at", "updated_at", "retries"}},
	{"known_users", []string{"id", "username", "success_count", "fail_count", "last_success_at", "last_fail_at", "updated_at"}},
	{"artist_user_reliability", []string{"id", "artist_id", "user_id", "success_count", "fail_count", "last_success_at", "last_fail_at", "updated_at"}},
	{"job_events", []string{"id", "album_job_id", "event", "detail", "created_at"}},
}

var targetTables = []string{
	"album_jobs", "candidates", "transfers", "known_users", "artist_user_reliability", "job_events",
}

var directTables = []tableSpec{
	{"known_users", []string{"id", "username", "success_count", "fail_count", "last_success_at", "last_fail_at", "updated_at"}},
	{"artist_user_reliability", []string{"id", "artist_id", "user_id", "success_count", "fail_count", "last_success_at", "last_fail_at", "updated_at"}},
	{"job_events", []string{"id", "album_job_id", "event", "detail", "created_at"}},
}

// timestampColumns are scanned through sql.NullTime so SQLite's text
// timestamps convert cleanly to timestamptz on insert.
var timestampColumns = map[string]bool{
	"next_attempt_at":     true,
	"not_before":          true,
	"failed_at":           true,
	"created_at":          true,
	"updated_at":          true,
	"backoff_until":       true,
	"deadline":            true,
	"last_progress_at":    true,
	"last_success_at":     true,
	"last_fail_at":        true,
	"import_submitted_at": true,
}

func main() {
	sqlitePath := flag.String("sqlite", "", "path to the existing slusk SQLite database")
	pgDSN := flag.String("pg", "", "PostgreSQL DSN of the migration target")
	flag.Parse()
	if *sqlitePath == "" || *pgDSN == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*sqlitePath, *pgDSN); err != nil {
		log.Fatalf("migration failed: %v", err)
	}
}

func run(sqlitePath, pgDSN string) error {
	if _, err := os.Stat(sqlitePath); err != nil {
		return fmt.Errorf("sqlite database: %w", err)
	}

	src, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer src.Close()
	if err := src.Ping(); err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	if err := validateSource(src); err != nil {
		return fmt.Errorf("unsupported SQLite schema: %w", err)
	}

	// Source validation deliberately precedes store.Open: an unsupported source
	// must fail without applying any schema changes to the target.
	st, err := store.Open(pgDSN)
	if err != nil {
		return fmt.Errorf("open target and apply schema: %w", err)
	}
	if err := st.Close(); err != nil {
		return fmt.Errorf("close target store: %w", err)
	}

	dst, err := sql.Open("pgx", pgDSN)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer dst.Close()

	for _, name := range targetTables {
		var exists bool
		if err := dst.QueryRow(fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s)`, name)).Scan(&exists); err != nil {
			return fmt.Errorf("check target table %s: %w", name, err)
		}
		if exists {
			return fmt.Errorf("target table %s already has rows; refusing to migrate into a non-empty database", name)
		}
	}

	tx, err := dst.Begin()
	if err != nil {
		return fmt.Errorf("begin target transaction: %w", err)
	}
	defer tx.Rollback()

	counts := make(map[string]int)
	if counts["album_jobs"], err = copyAlbumJobs(src, tx); err != nil {
		return fmt.Errorf("copy album_jobs: %w", err)
	}
	if counts["candidates"], err = copyCandidates(src, tx); err != nil {
		return fmt.Errorf("transform candidate_attempts to candidates: %w", err)
	}
	if counts["transfers"], err = copyTable(src, tx, "transfers", "transfers",
		[]string{"id", "attempt_id", "slskd_id", "username", "filename", "state", "bytes_done", "bytes_total", "deadline", "last_progress_at", "updated_at", "retries"},
		[]string{"id", "candidate_id", "slskd_id", "username", "filename", "state", "bytes_done", "bytes_total", "deadline", "last_progress_at", "updated_at", "retries"}); err != nil {
		return fmt.Errorf("copy transfers: %w", err)
	}
	for _, table := range directTables {
		counts[table.name], err = copyTable(src, tx, table.name, table.name, table.columns, table.columns)
		if err != nil {
			return fmt.Errorf("copy %s: %w", table.name, err)
		}
	}

	// migrations/0001_baseline_schema.sql creates this upgrade-compatible constraint NOT VALID. The
	// source relationship preflight and transactional copy make it safe to
	// validate now, leaving a fresh migrated target with fully checked FKs.
	if _, err := tx.Exec(`ALTER TABLE transfers VALIDATE CONSTRAINT transfers_candidate_id_fkey`); err != nil {
		return fmt.Errorf("validate transfers candidate foreign key: %w", err)
	}

	for _, name := range targetTables {
		if _, err := tx.Exec(fmt.Sprintf(
			`SELECT setval(pg_get_serial_sequence('%s','id'), COALESCE(MAX(id), 1), MAX(id) IS NOT NULL) FROM %s`,
			name, name)); err != nil {
			return fmt.Errorf("reset %s id sequence: %w", name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	for _, name := range targetTables {
		log.Printf("migrated %s: %d rows", name, counts[name])
	}
	return nil
}

func validateSource(src *sql.DB) error {
	rows, err := src.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("inspect tables: %w", err)
	}
	var actualTables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("inspect tables: %w", err)
		}
		actualTables = append(actualTables, name)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("inspect tables: %w", err)
	}

	expectedTables := make([]string, len(sourceTables))
	for i, table := range sourceTables {
		expectedTables[i] = table.name
	}
	sort.Strings(expectedTables)
	if strings.Join(actualTables, ",") != strings.Join(expectedTables, ",") {
		return fmt.Errorf("expected final pre-PostgreSQL tables [%s], found [%s]; use the slusk release matching this database or migrate it to the final SQLite release first",
			strings.Join(expectedTables, ", "), strings.Join(actualTables, ", "))
	}

	for _, table := range sourceTables {
		columnRows, err := src.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table.name))
		if err != nil {
			return fmt.Errorf("inspect %s columns: %w", table.name, err)
		}
		var actual []string
		for columnRows.Next() {
			var cid, notNull, pk int
			var name, dataType string
			var defaultValue any
			if err := columnRows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
				columnRows.Close()
				return fmt.Errorf("inspect %s columns: %w", table.name, err)
			}
			actual = append(actual, name)
		}
		if err := columnRows.Close(); err != nil {
			return fmt.Errorf("inspect %s columns: %w", table.name, err)
		}
		if strings.Join(actual, ",") != strings.Join(table.columns, ",") {
			return fmt.Errorf("table %s: expected columns [%s], found [%s]", table.name, strings.Join(table.columns, ", "), strings.Join(actual, ", "))
		}
	}

	if err := validateStates(src, "album_jobs", "state", []string{"DISCOVERED", "SEARCHING", "SELECTING", "DOWNLOADING", "VERIFYING", "IMPORTING", "COMPLETED", "COOLDOWN", "FAILED", "CANCELLED"}); err != nil {
		return err
	}
	if err := validateStates(src, "candidate_attempts", "state", []string{"PENDING", "ACTIVE", "SUCCEEDED", "FAILED"}); err != nil {
		return err
	}
	if err := validateStates(src, "transfers", "state", []string{"PENDING", "QUEUED", "IN_PROGRESS", "COMPLETED", "ERRORED", "CANCELLED", "STALLED"}); err != nil {
		return err
	}

	checks := []struct {
		name  string
		query string
	}{
		// The current candidates model never retries a failed candidate and has
		// no per-candidate scheduling column. Mapping this value to album-level
		// not_before would change its scope, so only NULL can be represented
		// without silently changing or discarding legacy data.
		{"candidate_attempts.backoff_until (no safe current equivalent; it must be NULL)", `SELECT COUNT(*) FROM candidate_attempts WHERE backoff_until IS NOT NULL`},
		{"candidate_attempts.album_job_id", `SELECT COUNT(*) FROM candidate_attempts a LEFT JOIN album_jobs j ON j.id = a.album_job_id WHERE j.id IS NULL`},
		{"transfers.attempt_id", `SELECT COUNT(*) FROM transfers t LEFT JOIN candidate_attempts a ON a.id = t.attempt_id WHERE a.id IS NULL`},
		{"artist_user_reliability.user_id", `SELECT COUNT(*) FROM artist_user_reliability r LEFT JOIN known_users u ON u.id = r.user_id WHERE u.id IS NULL`},
		{"job_events.album_job_id", `SELECT COUNT(*) FROM job_events e LEFT JOIN album_jobs j ON j.id = e.album_job_id WHERE j.id IS NULL`},
		{"candidate/transfer username", `SELECT COUNT(*) FROM transfers t JOIN candidate_attempts a ON a.id = t.attempt_id WHERE t.username <> a.username`},
		{"non-terminal candidate uniqueness", `SELECT COUNT(*) FROM (SELECT album_job_id FROM candidate_attempts WHERE state IN ('PENDING','ACTIVE') GROUP BY album_job_id HAVING COUNT(*) > 1)`},
		{"non-terminal candidate job state", `SELECT COUNT(*) FROM candidate_attempts a JOIN album_jobs j ON j.id = a.album_job_id WHERE a.state IN ('PENDING','ACTIVE') AND j.state NOT IN ('DOWNLOADING','VERIFYING','IMPORTING')`},
		{"active job candidate", `SELECT COUNT(*) FROM album_jobs j WHERE j.state IN ('DOWNLOADING','VERIFYING','IMPORTING') AND (SELECT COUNT(*) FROM candidate_attempts a WHERE a.album_job_id=j.id AND a.state IN ('PENDING','ACTIVE')) <> 1`},
	}
	for _, check := range checks {
		var count int
		if err := src.QueryRow(check.query).Scan(&count); err != nil {
			return fmt.Errorf("validate %s: %w", check.name, err)
		}
		if count != 0 {
			return fmt.Errorf("%s has %d ambiguous or orphaned row(s)", check.name, count)
		}
	}
	return nil
}

func validateStates(src *sql.DB, table, column string, allowed []string) error {
	allowedSet := make(map[string]bool, len(allowed))
	for _, state := range allowed {
		allowedSet[state] = true
	}
	rows, err := src.Query(fmt.Sprintf(`SELECT DISTINCT %s FROM %s ORDER BY %s`, column, table, column))
	if err != nil {
		return fmt.Errorf("inspect %s.%s states: %w", table, column, err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			return fmt.Errorf("inspect %s.%s states: %w", table, column, err)
		}
		if !allowedSet[state] {
			return fmt.Errorf("table %s has unsupported %s value %q (supported: %s)", table, column, state, strings.Join(allowed, ", "))
		}
	}
	return rows.Err()
}

func copyAlbumJobs(src *sql.DB, tx *sql.Tx) (int, error) {
	const query = `SELECT id, lidarr_album_id, state, candidates_tried, next_attempt_at, created_at, updated_at, title, artist_name, release_date, artist_id FROM album_jobs`
	rows, err := src.Query(query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	stmt, err := tx.Prepare(`INSERT INTO album_jobs (id, lidarr_album_id, state, candidates_tried, next_attempt_at, created_at, updated_at, title, artist_name, release_date, artist_id, retries, not_before, failed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	n := 0
	for rows.Next() {
		var id, albumID, candidatesTried, artistID int64
		var legacyState, title, artist, releaseDate string
		var nextAttempt, createdAt, updatedAt sql.NullTime
		if err := rows.Scan(&id, &albumID, &legacyState, &candidatesTried, &nextAttempt, &createdAt, &updatedAt, &title, &artist, &releaseDate, &artistID); err != nil {
			return 0, err
		}
		state, notBefore, failedAt := mapJobState(legacyState, nextAttempt, updatedAt)
		if _, err := stmt.Exec(id, albumID, state, candidatesTried, nextAttempt, createdAt, updatedAt, title, artist, releaseDate, artistID, 0, notBefore, failedAt); err != nil {
			return 0, err
		}
		n++
	}
	return n, rows.Err()
}

func mapJobState(state string, nextAttempt, updatedAt sql.NullTime) (string, sql.NullTime, sql.NullTime) {
	null := sql.NullTime{}
	switch state {
	case "DISCOVERED", "SEARCHING", "SELECTING":
		return "WANTED", null, null
	case "VERIFYING":
		return "IMPORTING", null, null
	case "COMPLETED":
		return "DONE", null, null
	case "COOLDOWN":
		return "WANTED", nextAttempt, null
	case "FAILED":
		return "FAILED", null, updatedAt
	default: // DOWNLOADING, IMPORTING, CANCELLED were retained verbatim.
		return state, null, null
	}
}

type candidateFile struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

func copyCandidates(src *sql.DB, tx *sql.Tx) (int, error) {
	rows, err := src.Query(`SELECT a.id, a.album_job_id, a.username, a.score, a.state, a.fail_reason, a.backoff_until, a.created_at, a.updated_at, j.state, j.updated_at FROM candidate_attempts a JOIN album_jobs j ON j.id = a.album_job_id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	stmt, err := tx.Prepare(`INSERT INTO candidates (id, album_job_id, username, score, files, state, fail_reason, import_submitted_at, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	n := 0
	for rows.Next() {
		var id, jobID int64
		var username, state, failReason, jobState string
		var score float64
		var backoffUntil, createdAt, updatedAt, jobUpdatedAt sql.NullTime
		if err := rows.Scan(&id, &jobID, &username, &score, &state, &failReason, &backoffUntil, &createdAt, &updatedAt, &jobState, &jobUpdatedAt); err != nil {
			return 0, err
		}
		if backoffUntil.Valid {
			return 0, fmt.Errorf("candidate_attempts id %d has non-null backoff_until with no safe current equivalent", id)
		}
		files, err := filesForAttempt(src, id)
		if err != nil {
			return 0, err
		}
		encodedFiles, err := json.Marshal(files)
		if err != nil {
			return 0, err
		}
		if state == "PENDING" {
			// CreateAttempt wrote PENDING and no legacy code promoted it; once
			// selected it was therefore the operational equivalent of ACTIVE.
			state = "ACTIVE"
		}
		var importSubmittedAt sql.NullTime
		if state == "ACTIVE" && jobState == "IMPORTING" {
			// The legacy transition to IMPORTING happened immediately after the
			// Lidarr import request. Current Importing uses this timestamp as that
			// exact phase marker.
			importSubmittedAt = jobUpdatedAt
		}
		if _, err := stmt.Exec(id, jobID, username, score, encodedFiles, state, failReason, importSubmittedAt, createdAt, updatedAt); err != nil {
			return 0, err
		}
		n++
	}
	return n, rows.Err()
}

func filesForAttempt(src *sql.DB, attemptID int64) ([]candidateFile, error) {
	rows, err := src.Query(`SELECT filename, bytes_total FROM transfers WHERE attempt_id = ? ORDER BY id`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := make([]candidateFile, 0)
	for rows.Next() {
		var file candidateFile
		if err := rows.Scan(&file.Filename, &file.Size); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// copyTable streams a table from SQLite into the target transaction. Source
// and target column names may differ (attempt_id -> candidate_id).
func copyTable(src *sql.DB, tx *sql.Tx, sourceName, targetName string, sourceColumns, targetColumns []string) (int, error) {
	if len(sourceColumns) != len(targetColumns) {
		return 0, fmt.Errorf("source/target column count mismatch")
	}
	rows, err := src.Query(fmt.Sprintf(`SELECT %s FROM %s`, strings.Join(sourceColumns, ", "), sourceName))
	if err != nil {
		return 0, fmt.Errorf("read: %w", err)
	}
	defer rows.Close()

	placeholders := make([]string, len(targetColumns))
	for i := range targetColumns {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	insert := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, targetName, strings.Join(targetColumns, ", "), strings.Join(placeholders, ", "))
	stmt, err := tx.Prepare(insert)
	if err != nil {
		return 0, fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	n := 0
	for rows.Next() {
		dests := make([]any, len(sourceColumns))
		for i, column := range sourceColumns {
			if timestampColumns[column] {
				dests[i] = new(sql.NullTime)
			} else {
				dests[i] = new(any)
			}
		}
		if err := rows.Scan(dests...); err != nil {
			return 0, fmt.Errorf("scan row: %w", err)
		}
		args := make([]any, len(sourceColumns))
		for i, column := range sourceColumns {
			if timestampColumns[column] {
				args[i] = *dests[i].(*sql.NullTime)
			} else {
				args[i] = *dests[i].(*any)
			}
		}
		if _, err := stmt.Exec(args...); err != nil {
			return 0, fmt.Errorf("insert row: %w", err)
		}
		n++
	}
	return n, rows.Err()
}
