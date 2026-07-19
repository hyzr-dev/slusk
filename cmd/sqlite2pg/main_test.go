package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/samuelenocsson/slskdarr/internal/store/storetest"
)

func TestMain(m *testing.M) {
	os.Exit(storetest.Run(m))
}

func TestRunMigratesFinalSQLiteSchemaToCurrentPostgres(t *testing.T) {
	source := filepath.Join(t.TempDir(), "legacy.db")
	src := createLegacyFixture(t, source)
	seedLegacyFixture(t, src)
	if err := src.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	dsn := storetest.DSN(t)
	if err := run(source, dsn); err != nil {
		t.Fatalf("run: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	defer db.Close()

	stateCases := map[int64]struct {
		state     string
		notBefore bool
		failedAt  bool
	}{
		1:  {"WANTED", false, false}, // DISCOVERED
		2:  {"WANTED", false, false}, // SEARCHING
		3:  {"WANTED", false, false}, // SELECTING (legacy transient state)
		4:  {"DOWNLOADING", false, false},
		5:  {"IMPORTING", false, false}, // VERIFYING is current Importing's verify phase
		6:  {"IMPORTING", false, false},
		7:  {"DONE", false, false},
		8:  {"WANTED", true, false}, // COOLDOWN becomes WANTED with not_before
		9:  {"FAILED", false, true},
		10: {"CANCELLED", false, false},
	}
	for id, want := range stateCases {
		var state string
		var candidatesTried int
		var nextAttempt, notBefore, failedAt sql.NullTime
		if err := db.QueryRow(`SELECT state, candidates_tried, next_attempt_at, not_before, failed_at FROM album_jobs WHERE id=$1`, id).
			Scan(&state, &candidatesTried, &nextAttempt, &notBefore, &failedAt); err != nil {
			t.Fatalf("read album job %d: %v", id, err)
		}
		if state != want.state || notBefore.Valid != want.notBefore || failedAt.Valid != want.failedAt {
			t.Errorf("job %d = state %s not_before.Valid=%v failed_at.Valid=%v, want %+v", id, state, notBefore.Valid, failedAt.Valid, want)
		}
		if candidatesTried != int(id) {
			t.Errorf("job %d candidates_tried=%d, want %d", id, candidatesTried, id)
		}
		if id == 8 && (!nextAttempt.Valid || !notBefore.Valid || !nextAttempt.Time.Equal(notBefore.Time)) {
			t.Errorf("cooldown timestamps not preserved: next=%v not_before=%v", nextAttempt, notBefore)
		}
	}

	candidateCases := map[int64]struct {
		state           string
		jobID           int64
		importSubmitted bool
	}{
		104: {"ACTIVE", 4, false},
		105: {"ACTIVE", 5, false},
		106: {"ACTIVE", 6, true},
		107: {"SUCCEEDED", 7, false},
		108: {"FAILED", 8, false},
	}
	for id, want := range candidateCases {
		var jobID int64
		var username, state, failReason string
		var score float64
		var filesJSON []byte
		var importSubmitted sql.NullTime
		var createdAt, updatedAt time.Time
		if err := db.QueryRow(`SELECT album_job_id, username, score, files, state, fail_reason, import_submitted_at, created_at, updated_at FROM candidates WHERE id=$1`, id).
			Scan(&jobID, &username, &score, &filesJSON, &state, &failReason, &importSubmitted, &createdAt, &updatedAt); err != nil {
			t.Fatalf("read candidate %d: %v", id, err)
		}
		if jobID != want.jobID || state != want.state || importSubmitted.Valid != want.importSubmitted {
			t.Errorf("candidate %d = job=%d state=%s import_submitted.Valid=%v, want %+v", id, jobID, state, importSubmitted.Valid, want)
		}
		if username != "peer"+stateForID(id) || score != float64(id)/10 || createdAt.IsZero() || updatedAt.IsZero() {
			t.Errorf("candidate %d scalar fields changed: user=%q score=%v created=%v updated=%v", id, username, score, createdAt, updatedAt)
		}
		if id == 108 && failReason != "transfer failed" {
			t.Errorf("candidate fail_reason=%q, want transfer failed", failReason)
		}
		var files []candidateFile
		if err := json.Unmarshal(filesJSON, &files); err != nil {
			t.Fatalf("decode candidate %d files: %v", id, err)
		}
		if len(files) != 1 || files[0].Filename != "album/"+stateForID(id)+".flac" || files[0].Size != id*100 {
			t.Errorf("candidate %d files=%+v; transfer-derived file list was not preserved", id, files)
		}
	}

	for candidateID := int64(104); candidateID <= 108; candidateID++ {
		var gotCandidateID, bytesTotal int64
		if err := db.QueryRow(`SELECT candidate_id, bytes_total FROM transfers WHERE id=$1`, 1000+candidateID).
			Scan(&gotCandidateID, &bytesTotal); err != nil {
			t.Fatalf("read transfer for candidate %d: %v", candidateID, err)
		}
		if gotCandidateID != candidateID || bytesTotal != candidateID*100 {
			t.Errorf("transfer mapping for candidate %d = candidate_id %d bytes_total %d", candidateID, gotCandidateID, bytesTotal)
		}
	}

	for table, want := range map[string]int{
		"album_jobs": 10, "candidates": 5, "transfers": 5,
		"known_users": 1, "artist_user_reliability": 1, "job_events": 1,
	} {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != want {
			t.Errorf("%s count=%d, want %d", table, got, want)
		}
	}

	var orphanTransfers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM transfers t LEFT JOIN candidates c ON c.id=t.candidate_id WHERE c.id IS NULL`).Scan(&orphanTransfers); err != nil {
		t.Fatalf("check transfer foreign keys: %v", err)
	}
	if orphanTransfers != 0 {
		t.Errorf("found %d orphaned transfers", orphanTransfers)
	}
	var constraintValidated bool
	if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname='transfers_candidate_id_fkey'`).Scan(&constraintValidated); err != nil {
		t.Fatalf("read transfers FK: %v", err)
	}
	if !constraintValidated {
		t.Error("fresh target transfers foreign key should be validated")
	}

	var nextID int64
	if err := db.QueryRow(`INSERT INTO candidates (album_job_id, username, score, files, state, created_at, updated_at) VALUES (1,'sequence-check',1,'[]','NEW',now(),now()) RETURNING id`).Scan(&nextID); err != nil {
		t.Fatalf("insert after sequence reset: %v", err)
	}
	if nextID <= 108 {
		t.Errorf("candidate identity was not reset past copied IDs: got %d", nextID)
	}
}

func TestRunRejectsUnsupportedSchemaBeforeOpeningTarget(t *testing.T) {
	source := filepath.Join(t.TempDir(), "unsupported.db")
	db := createLegacyFixture(t, source)
	if _, err := db.Exec(`ALTER TABLE candidate_attempts DROP COLUMN updated_at`); err != nil {
		t.Fatalf("make unsupported fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	err := run(source, "postgres://invalid:invalid@127.0.0.1:1/never-touched?sslmode=disable")
	if err == nil {
		t.Fatal("expected unsupported schema error")
	}
	if !strings.Contains(err.Error(), "unsupported SQLite schema") || !strings.Contains(err.Error(), "candidate_attempts") || !strings.Contains(err.Error(), "updated_at") {
		t.Fatalf("error is not actionable: %v", err)
	}
	if strings.Contains(err.Error(), "open target") {
		t.Fatalf("target was opened before source rejection: %v", err)
	}
}

func TestValidateSourceRejectsAmbiguousState(t *testing.T) {
	source := filepath.Join(t.TempDir(), "ambiguous.db")
	db := createLegacyFixture(t, source)
	stamp := "2026-07-01T12:00:00Z"
	if _, err := db.Exec(`INSERT INTO album_jobs (id,lidarr_album_id,state,candidates_tried,created_at,updated_at) VALUES (1,1,'MYSTERY',0,?,?)`, stamp, stamp); err != nil {
		t.Fatalf("seed ambiguous state: %v", err)
	}
	defer db.Close()

	err := validateSource(db)
	if err == nil || !strings.Contains(err.Error(), `unsupported state value "MYSTERY"`) {
		t.Fatalf("validateSource error=%v, want unsupported state", err)
	}
}

func createLegacyFixture(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	statements := []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE album_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT, lidarr_album_id INTEGER NOT NULL, state TEXT NOT NULL,
			candidates_tried INTEGER NOT NULL DEFAULT 0, next_attempt_at DATETIME, created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL, title TEXT NOT NULL DEFAULT '', artist_name TEXT NOT NULL DEFAULT '',
			release_date TEXT NOT NULL DEFAULT '', artist_id INTEGER NOT NULL DEFAULT 0, UNIQUE(lidarr_album_id))`,
		`CREATE TABLE candidate_attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT, album_job_id INTEGER NOT NULL REFERENCES album_jobs(id), username TEXT NOT NULL,
			score REAL NOT NULL, state TEXT NOT NULL, fail_reason TEXT NOT NULL DEFAULT '', backoff_until DATETIME,
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`,
		`CREATE TABLE transfers (
			id INTEGER PRIMARY KEY AUTOINCREMENT, attempt_id INTEGER NOT NULL REFERENCES candidate_attempts(id), slskd_id TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL, filename TEXT NOT NULL, state TEXT NOT NULL, bytes_done INTEGER NOT NULL DEFAULT 0,
			bytes_total INTEGER NOT NULL DEFAULT 0, deadline DATETIME NOT NULL, last_progress_at DATETIME,
			updated_at DATETIME NOT NULL, retries INTEGER NOT NULL DEFAULT 0, UNIQUE(username, filename))`,
		`CREATE TABLE known_users (
			id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, success_count INTEGER NOT NULL DEFAULT 0,
			fail_count INTEGER NOT NULL DEFAULT 0, last_success_at DATETIME, last_fail_at DATETIME, updated_at DATETIME NOT NULL)`,
		`CREATE TABLE artist_user_reliability (
			id INTEGER PRIMARY KEY AUTOINCREMENT, artist_id INTEGER NOT NULL, user_id INTEGER NOT NULL REFERENCES known_users(id),
			success_count INTEGER NOT NULL DEFAULT 0, fail_count INTEGER NOT NULL DEFAULT 0, last_success_at DATETIME,
			last_fail_at DATETIME, updated_at DATETIME NOT NULL, UNIQUE(artist_id,user_id))`,
		`CREATE TABLE job_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT, album_job_id INTEGER NOT NULL, event TEXT NOT NULL, detail TEXT NOT NULL, created_at TIMESTAMP NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatalf("create fixture: %v\n%s", err, statement)
		}
	}
	return db
}

func seedLegacyFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	stamp := "2026-07-01T12:00:00Z"
	later := "2026-07-01T13:00:00Z"
	states := []string{"DISCOVERED", "SEARCHING", "SELECTING", "DOWNLOADING", "VERIFYING", "IMPORTING", "COMPLETED", "COOLDOWN", "FAILED", "CANCELLED"}
	for i, state := range states {
		id := int64(i + 1)
		var next any
		if state == "COOLDOWN" {
			next = later
		}
		if _, err := db.Exec(`INSERT INTO album_jobs (id,lidarr_album_id,state,candidates_tried,next_attempt_at,created_at,updated_at,title,artist_name,release_date,artist_id) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			id, 1000+id, state, id, next, stamp, later, "album", "artist", "2026-01-01", 77); err != nil {
			t.Fatalf("insert job %s: %v", state, err)
		}
	}

	attempts := []struct {
		id, jobID int64
		state     string
		reason    string
	}{
		{104, 4, "PENDING", ""},
		{105, 5, "ACTIVE", ""},
		{106, 6, "PENDING", ""},
		{107, 7, "SUCCEEDED", ""},
		{108, 8, "FAILED", "transfer failed"},
	}
	for _, attempt := range attempts {
		var backoff any
		if attempt.state == "FAILED" {
			backoff = later
		}
		username := "peer" + stateForID(attempt.id)
		if _, err := db.Exec(`INSERT INTO candidate_attempts (id,album_job_id,username,score,state,fail_reason,backoff_until,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			attempt.id, attempt.jobID, username, float64(attempt.id)/10, attempt.state, attempt.reason, backoff, stamp, later); err != nil {
			t.Fatalf("insert attempt %d: %v", attempt.id, err)
		}
		if _, err := db.Exec(`INSERT INTO transfers (id,attempt_id,slskd_id,username,filename,state,bytes_done,bytes_total,deadline,last_progress_at,updated_at,retries) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			1000+attempt.id, attempt.id, "slskd-"+stateForID(attempt.id), username, "album/"+stateForID(attempt.id)+".flac", "COMPLETED", attempt.id*100, attempt.id*100, later, later, later, 2); err != nil {
			t.Fatalf("insert transfer %d: %v", attempt.id, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO known_users (id,username,success_count,fail_count,last_success_at,last_fail_at,updated_at) VALUES (200,'reliable',4,2,?,?,?)`, stamp, later, later); err != nil {
		t.Fatalf("insert known user: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO artist_user_reliability (id,artist_id,user_id,success_count,fail_count,last_success_at,last_fail_at,updated_at) VALUES (300,77,200,3,1,?,?,?)`, stamp, later, later); err != nil {
		t.Fatalf("insert reliability: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO job_events (id,album_job_id,event,detail,created_at) VALUES (400,4,'candidate_selected','preserved event',?)`, stamp); err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func stateForID(id int64) string {
	return string(rune('a' + (id - 104)))
}
