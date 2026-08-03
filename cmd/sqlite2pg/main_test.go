package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/samuelenocsson/slusk/internal/store/storetest"
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

	stamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	later := time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)
	stateCases := map[int64]struct {
		state                  string
		nextAttempt, notBefore *time.Time
		failedAt               *time.Time
	}{
		1:  {state: "WANTED"}, // DISCOVERED
		2:  {state: "WANTED"}, // SEARCHING
		3:  {state: "WANTED"}, // SELECTING (legacy transient state)
		4:  {state: "DOWNLOADING"},
		5:  {state: "IMPORTING"}, // VERIFYING is current Importing's verify phase
		6:  {state: "IMPORTING"},
		7:  {state: "DONE"},
		8:  {state: "WANTED", nextAttempt: &later, notBefore: &later}, // COOLDOWN
		9:  {state: "FAILED", failedAt: &later},
		10: {state: "CANCELLED"},
	}
	for id, want := range stateCases {
		var albumID, candidatesTried, artistID int64
		var state, title, artist, releaseDate string
		var nextAttempt, notBefore, failedAt sql.NullTime
		var createdAt, updatedAt time.Time
		if err := db.QueryRow(`SELECT lidarr_album_id, state, candidates_tried, next_attempt_at, created_at, updated_at, title, artist_name, release_date, artist_id, not_before, failed_at FROM album_jobs WHERE id=$1`, id).
			Scan(&albumID, &state, &candidatesTried, &nextAttempt, &createdAt, &updatedAt, &title, &artist, &releaseDate, &artistID, &notBefore, &failedAt); err != nil {
			t.Fatalf("read album job %d: %v", id, err)
		}
		if albumID != 1000+id || state != want.state || candidatesTried != id || title != "album" || artist != "artist" || releaseDate != "2026-01-01" || artistID != 77 {
			t.Errorf("job %d scalar fields changed: album=%d state=%q tried=%d title=%q artist=%q release=%q artistID=%d", id, albumID, state, candidatesTried, title, artist, releaseDate, artistID)
		}
		assertTimeEqual(t, fmt.Sprintf("job %d created_at", id), createdAt, stamp)
		assertTimeEqual(t, fmt.Sprintf("job %d updated_at", id), updatedAt, later)
		assertNullTimeEqual(t, fmt.Sprintf("job %d next_attempt_at", id), nextAttempt, want.nextAttempt)
		assertNullTimeEqual(t, fmt.Sprintf("job %d not_before", id), notBefore, want.notBefore)
		assertNullTimeEqual(t, fmt.Sprintf("job %d failed_at", id), failedAt, want.failedAt)
	}

	candidateCases := map[int64]struct {
		state           string
		jobID           int64
		failReason      string
		importSubmitted *time.Time
	}{
		104: {state: "ACTIVE", jobID: 4},
		105: {state: "ACTIVE", jobID: 5},
		106: {state: "ACTIVE", jobID: 6, importSubmitted: &later},
		107: {state: "SUCCEEDED", jobID: 7},
		108: {state: "FAILED", jobID: 8, failReason: "transfer failed"},
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
		if jobID != want.jobID || username != "peer"+stateForID(id) || score != float64(id)/10 || state != want.state || failReason != want.failReason {
			t.Errorf("candidate %d scalar fields changed: job=%d user=%q score=%v state=%q reason=%q, want %+v", id, jobID, username, score, state, failReason, want)
		}
		assertNullTimeEqual(t, fmt.Sprintf("candidate %d import_submitted_at", id), importSubmitted, want.importSubmitted)
		assertTimeEqual(t, fmt.Sprintf("candidate %d created_at", id), createdAt, stamp)
		assertTimeEqual(t, fmt.Sprintf("candidate %d updated_at", id), updatedAt, later)
		var files []candidateFile
		if err := json.Unmarshal(filesJSON, &files); err != nil {
			t.Fatalf("decode candidate %d files: %v", id, err)
		}
		if len(files) != 1 || files[0].Filename != "album/"+stateForID(id)+".flac" || files[0].Size != id*100 {
			t.Errorf("candidate %d files=%+v; transfer-derived file list was not preserved", id, files)
		}
	}

	for candidateID := int64(104); candidateID <= 108; candidateID++ {
		var gotCandidateID, bytesDone, bytesTotal, retries int64
		var slskdID, username, filename, state string
		var deadline, updatedAt time.Time
		var lastProgressAt sql.NullTime
		if err := db.QueryRow(`SELECT candidate_id, slskd_id, username, filename, state, bytes_done, bytes_total, deadline, last_progress_at, updated_at, retries FROM transfers WHERE id=$1`, 1000+candidateID).
			Scan(&gotCandidateID, &slskdID, &username, &filename, &state, &bytesDone, &bytesTotal, &deadline, &lastProgressAt, &updatedAt, &retries); err != nil {
			t.Fatalf("read transfer for candidate %d: %v", candidateID, err)
		}
		wantSuffix := stateForID(candidateID)
		if gotCandidateID != candidateID || slskdID != "slskd-"+wantSuffix || username != "peer"+wantSuffix || filename != "album/"+wantSuffix+".flac" || state != "COMPLETED" || bytesDone != candidateID*100 || bytesTotal != candidateID*100 || retries != 2 {
			t.Errorf("transfer %d scalar fields changed: candidate=%d slskd=%q user=%q filename=%q state=%q done=%d total=%d retries=%d", 1000+candidateID, gotCandidateID, slskdID, username, filename, state, bytesDone, bytesTotal, retries)
		}
		assertTimeEqual(t, fmt.Sprintf("transfer %d deadline", 1000+candidateID), deadline, later)
		wantLastProgress := &later
		if candidateID == 104 {
			wantLastProgress = nil
		}
		assertNullTimeEqual(t, fmt.Sprintf("transfer %d last_progress_at", 1000+candidateID), lastProgressAt, wantLastProgress)
		assertTimeEqual(t, fmt.Sprintf("transfer %d updated_at", 1000+candidateID), updatedAt, later)
	}

	var knownUsername string
	var knownSuccess, knownFail int64
	var knownLastSuccess, knownUpdated time.Time
	var knownLastFail sql.NullTime
	if err := db.QueryRow(`SELECT username, success_count, fail_count, last_success_at, last_fail_at, updated_at FROM known_users WHERE id=200`).
		Scan(&knownUsername, &knownSuccess, &knownFail, &knownLastSuccess, &knownLastFail, &knownUpdated); err != nil {
		t.Fatalf("read known user: %v", err)
	}
	if knownUsername != "reliable" || knownSuccess != 4 || knownFail != 2 {
		t.Errorf("known user scalar fields changed: username=%q success=%d fail=%d", knownUsername, knownSuccess, knownFail)
	}
	assertTimeEqual(t, "known user last_success_at", knownLastSuccess, stamp)
	assertNullTimeEqual(t, "known user last_fail_at", knownLastFail, nil)
	assertTimeEqual(t, "known user updated_at", knownUpdated, later)

	var reliabilityArtistID, reliabilityUserID, reliabilitySuccess, reliabilityFail int64
	var reliabilityLastFail, reliabilityUpdated time.Time
	var reliabilityLastSuccess sql.NullTime
	if err := db.QueryRow(`SELECT artist_id, user_id, success_count, fail_count, last_success_at, last_fail_at, updated_at FROM artist_user_reliability WHERE id=300`).
		Scan(&reliabilityArtistID, &reliabilityUserID, &reliabilitySuccess, &reliabilityFail, &reliabilityLastSuccess, &reliabilityLastFail, &reliabilityUpdated); err != nil {
		t.Fatalf("read artist reliability: %v", err)
	}
	if reliabilityArtistID != 77 || reliabilityUserID != 200 || reliabilitySuccess != 3 || reliabilityFail != 1 {
		t.Errorf("artist reliability scalar fields changed: artist=%d user=%d success=%d fail=%d", reliabilityArtistID, reliabilityUserID, reliabilitySuccess, reliabilityFail)
	}
	assertNullTimeEqual(t, "artist reliability last_success_at", reliabilityLastSuccess, nil)
	assertTimeEqual(t, "artist reliability last_fail_at", reliabilityLastFail, later)
	assertTimeEqual(t, "artist reliability updated_at", reliabilityUpdated, later)

	var eventJobID int64
	var event, detail string
	var eventCreated time.Time
	if err := db.QueryRow(`SELECT album_job_id, event, detail, created_at FROM job_events WHERE id=400`).Scan(&eventJobID, &event, &detail, &eventCreated); err != nil {
		t.Fatalf("read job event: %v", err)
	}
	if eventJobID != 4 || event != "candidate_selected" || detail != "preserved event" {
		t.Errorf("job event scalar fields changed: job=%d event=%q detail=%q", eventJobID, event, detail)
	}
	assertTimeEqual(t, "job event created_at", eventCreated, stamp)

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

	identityCases := []struct {
		name  string
		query string
		want  int64
	}{
		{"album_jobs", `INSERT INTO album_jobs (lidarr_album_id,state,created_at,updated_at) VALUES (9999,'WANTED',now(),now()) RETURNING id`, 11},
		{"candidates", `INSERT INTO candidates (album_job_id,username,score,files,state,created_at,updated_at) VALUES (1,'sequence-check',1,'[]','NEW',now(),now()) RETURNING id`, 109},
		{"transfers", `INSERT INTO transfers (candidate_id,username,filename,state,deadline,updated_at) VALUES (109,'sequence-check','sequence.flac','PENDING',now(),now()) RETURNING id`, 1109},
		{"known_users", `INSERT INTO known_users (username,updated_at) VALUES ('sequence-check',now()) RETURNING id`, 201},
		{"artist_user_reliability", `INSERT INTO artist_user_reliability (artist_id,user_id,updated_at) VALUES (9999,201,now()) RETURNING id`, 301},
		{"job_events", `INSERT INTO job_events (album_job_id,event,detail,created_at) VALUES (11,'sequence_check','identity',now()) RETURNING id`, 401},
	}
	for _, tc := range identityCases {
		var nextID int64
		if err := db.QueryRow(tc.query).Scan(&nextID); err != nil {
			t.Fatalf("insert %s after sequence reset: %v", tc.name, err)
		}
		if nextID != tc.want {
			t.Errorf("%s identity after reset=%d, want %d", tc.name, nextID, tc.want)
		}
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

func TestRunRejectsNonNullCandidateBackoffBeforeOpeningTarget(t *testing.T) {
	source := filepath.Join(t.TempDir(), "candidate-backoff.db")
	db := createLegacyFixture(t, source)
	seedLegacyFixture(t, db)
	if _, err := db.Exec(`UPDATE candidate_attempts SET backoff_until=? WHERE id=108`, "2026-07-01T14:00:00Z"); err != nil {
		t.Fatalf("seed candidate backoff: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	err := run(source, "postgres://invalid:invalid@127.0.0.1:1/never-touched?sslmode=disable")
	if err == nil {
		t.Fatal("expected unsupported candidate backoff error")
	}
	if !strings.Contains(err.Error(), "unsupported SQLite schema") || !strings.Contains(err.Error(), "candidate_attempts.backoff_until") || !strings.Contains(err.Error(), "no safe current equivalent") {
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
		username := "peer" + stateForID(attempt.id)
		if _, err := db.Exec(`INSERT INTO candidate_attempts (id,album_job_id,username,score,state,fail_reason,backoff_until,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			attempt.id, attempt.jobID, username, float64(attempt.id)/10, attempt.state, attempt.reason, nil, stamp, later); err != nil {
			t.Fatalf("insert attempt %d: %v", attempt.id, err)
		}
		var lastProgress any = later
		if attempt.id == 104 {
			lastProgress = nil
		}
		if _, err := db.Exec(`INSERT INTO transfers (id,attempt_id,slskd_id,username,filename,state,bytes_done,bytes_total,deadline,last_progress_at,updated_at,retries) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			1000+attempt.id, attempt.id, "slskd-"+stateForID(attempt.id), username, "album/"+stateForID(attempt.id)+".flac", "COMPLETED", attempt.id*100, attempt.id*100, later, lastProgress, later, 2); err != nil {
			t.Fatalf("insert transfer %d: %v", attempt.id, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO known_users (id,username,success_count,fail_count,last_success_at,last_fail_at,updated_at) VALUES (200,'reliable',4,2,?,?,?)`, stamp, nil, later); err != nil {
		t.Fatalf("insert known user: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO artist_user_reliability (id,artist_id,user_id,success_count,fail_count,last_success_at,last_fail_at,updated_at) VALUES (300,77,200,3,1,?,?,?)`, nil, later, later); err != nil {
		t.Fatalf("insert reliability: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO job_events (id,album_job_id,event,detail,created_at) VALUES (400,4,'candidate_selected','preserved event',?)`, stamp); err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func stateForID(id int64) string {
	return string(rune('a' + (id - 104)))
}

func assertTimeEqual(t *testing.T, name string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s=%s, want %s", name, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func assertNullTimeEqual(t *testing.T, name string, got sql.NullTime, want *time.Time) {
	t.Helper()
	if want == nil {
		if got.Valid {
			t.Errorf("%s=%s, want NULL", name, got.Time.Format(time.RFC3339Nano))
		}
		return
	}
	if !got.Valid {
		t.Errorf("%s=NULL, want %s", name, want.Format(time.RFC3339Nano))
		return
	}
	assertTimeEqual(t, name, got.Time, *want)
}
