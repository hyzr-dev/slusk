package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// seedJobWithCandidate creates a job with one candidate and one transfer under
// the given remote filename, which is what the pre-#314 cleanup derived its
// folder from and what the 0015 backfill reads.
func seedJobWithCandidate(t *testing.T, s *Store, albumID int64, filenames []string, now time.Time) (jobID, candID int64) {
	t.Helper()
	ctx := context.Background()
	job, err := s.UpsertWantedJob(ctx, albumID, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	files := make([]core.CandidateFile, len(filenames))
	for i, f := range filenames {
		files[i] = core.CandidateFile{Filename: f, Size: 1}
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "alice", Score: 1, Files: files}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}
	activated, _, err := s.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, 100, now.Add(time.Hour), now)
	if err != nil || !activated {
		t.Fatalf("ActivateCandidateWithTransfers: %v activated=%v", err, activated)
	}
	return job.ID, cand.ID
}

func countDownloadFolders(t *testing.T, s *Store, jobID int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM job_download_folders WHERE album_job_id = $1`, jobID).Scan(&n); err != nil {
		t.Fatalf("count job_download_folders: %v", err)
	}
	return n
}

// TestRegisterDownloadFolderIsIdempotent covers the ordinary case: a candidate
// writes many files into one folder, and every one of them registers.
func TestRegisterDownloadFolderIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, _ := seedJobWithCandidate(t, s, 1000, []string{`music\Artist\Album\01.flac`}, now)

	for i := 0; i < 3; i++ {
		if _, _, err := s.RegisterDownloadFolder(ctx, jobID, "Album", now); err != nil {
			t.Fatalf("RegisterDownloadFolder: %v", err)
		}
	}
	if n := countDownloadFolders(t, s, jobID); n != 1 {
		t.Errorf("rows = %d, want exactly 1 for repeated registration of one folder", n)
	}
	leaves, err := s.DownloadFoldersForJob(ctx, jobID)
	if err != nil {
		t.Fatalf("DownloadFoldersForJob: %v", err)
	}
	if len(leaves) != 1 || leaves[0] != "Album" {
		t.Errorf("leaves = %v, want [Album]", leaves)
	}
}

// TestRegisterDownloadFolderResurrectsCleanedRow is the case a plain
// ON CONFLICT DO NOTHING would get wrong: a later candidate downloads from a
// peer whose remote folder happens to carry a name this job already cleaned up.
// Leaving the row stamped would hide that folder from every later cleanup — a
// new leak in the code meant to close one.
func TestRegisterDownloadFolderResurrectsCleanedRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, _ := seedJobWithCandidate(t, s, 1001, []string{`music\Artist\Album\01.flac`}, now)

	if _, _, err := s.RegisterDownloadFolder(ctx, jobID, "Album", now); err != nil {
		t.Fatalf("RegisterDownloadFolder: %v", err)
	}
	if err := s.MarkDownloadFolderCleaned(ctx, jobID, "Album", now); err != nil {
		t.Fatalf("MarkDownloadFolderCleaned: %v", err)
	}
	if leaves, err := s.DownloadFoldersForJob(ctx, jobID); err != nil || len(leaves) != 0 {
		t.Fatalf("after cleaning, leaves = %v (%v), want none", leaves, err)
	}

	if _, _, err := s.RegisterDownloadFolder(ctx, jobID, "Album", now.Add(time.Hour)); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	leaves, err := s.DownloadFoldersForJob(ctx, jobID)
	if err != nil || len(leaves) != 1 || leaves[0] != "Album" {
		t.Errorf("after re-registration, leaves = %v (%v), want [Album]", leaves, err)
	}
	if n := countDownloadFolders(t, s, jobID); n != 1 {
		t.Errorf("rows = %d, want the same row resurrected rather than a second one", n)
	}
}

// TestRegisterDownloadFolderRejectsNonLeaf checks the Go-side guard, which is
// the first line of defence: the register feeds a recursive delete, and a
// constraint violation surfacing mid-download is a worse signal than an early
// reject.
func TestRegisterDownloadFolderRejectsNonLeaf(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, _ := seedJobWithCandidate(t, s, 1002, []string{`music\Artist\Album\01.flac`}, now)

	for _, leaf := range []string{"", ".", "..", "../x", "a/b", `a\b`, "/abs"} {
		_, _, err := s.RegisterDownloadFolder(ctx, jobID, leaf, now)
		if !errors.Is(err, ErrInvalidDownloadLeaf) {
			t.Errorf("RegisterDownloadFolder(%q) err = %v, want ErrInvalidDownloadLeaf", leaf, err)
		}
	}
	if n := countDownloadFolders(t, s, jobID); n != 0 {
		t.Errorf("rows = %d, want none written", n)
	}
}

// TestDownloadFolderCheckConstraintRejectsNonLeaf is the same rule enforced by
// Postgres rather than by Go, so a future writer that bypasses
// RegisterDownloadFolder still cannot aim cleanup outside the download root.
// Deliberately raw SQL: the point is that the database refuses on its own.
func TestDownloadFolderCheckConstraintRejectsNonLeaf(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, _ := seedJobWithCandidate(t, s, 1003, []string{`music\Artist\Album\01.flac`}, now)

	for _, leaf := range []string{"", ".", "..", "../x", "a/b", `a\b`, "/abs"} {
		_, err := s.db.Exec(
			`INSERT INTO job_download_folders (album_job_id, leaf, created_at) VALUES ($1, $2, $3)`,
			jobID, leaf, now)
		if err == nil {
			t.Errorf("raw INSERT of leaf %q succeeded, want a CHECK violation", leaf)
			continue
		}
		if !strings.Contains(err.Error(), "job_download_folders_leaf_is_one_element") {
			t.Errorf("raw INSERT of leaf %q failed with %v, want the leaf CHECK constraint", leaf, err)
		}
	}
}

// TestResetJobToWantedKeepsDownloadFolders is the whole point of the register
// living in its own table: the reset deletes the candidates and transfers a
// derived folder path was reconstructed from, which is exactly what used to
// orphan earlier search cycles' folders on disk.
func TestResetJobToWantedKeepsDownloadFolders(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := seedJobWithCandidate(t, s, 1004, []string{`music\Artist\First Album\01.flac`}, now)
	if _, _, err := s.RegisterDownloadFolder(ctx, jobID, "First Album", now); err != nil {
		t.Fatalf("RegisterDownloadFolder: %v", err)
	}
	if err := s.AdvanceJobState(ctx, jobID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	if err := s.ResetJobToWanted(ctx, jobID, core.StateSelecting, 1, nil, now); err != nil {
		t.Fatalf("ResetJobToWanted: %v", err)
	}

	// Premise: the derivation source really is gone.
	if transfers, err := s.TransfersForCandidate(ctx, candID); err != nil || len(transfers) != 0 {
		t.Fatalf("transfers after reset = %v (%v), want none (premise of this test)", transfers, err)
	}
	leaves, err := s.DownloadFoldersForJob(ctx, jobID)
	if err != nil || len(leaves) != 1 || leaves[0] != "First Album" {
		t.Errorf("leaves after reset = %v (%v), want [First Album]", leaves, err)
	}
}

// TestDeleteJobRemovesDownloadFolders keeps the table bounded: the register's
// lifetime is the job's, so deleting the job must take its rows with it.
func TestDeleteJobRemovesDownloadFolders(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, _ := seedJobWithCandidate(t, s, 1005, []string{`music\Artist\Album\01.flac`}, now)
	if _, _, err := s.RegisterDownloadFolder(ctx, jobID, "Album", now); err != nil {
		t.Fatalf("RegisterDownloadFolder: %v", err)
	}

	if deleted, err := s.DeleteJob(ctx, jobID); err != nil || !deleted {
		t.Fatalf("DeleteJob: deleted=%v err=%v", deleted, err)
	}
	if n := countDownloadFolders(t, s, jobID); n != 0 {
		t.Errorf("rows after DeleteJob = %d, want 0", n)
	}
}

// TestDownloadFolderBackfillCoversLiveJobsOnly re-runs migration 0015's
// backfill statement (idempotent by construction) against jobs seeded
// afterwards, which is the only way to exercise it: the migration itself runs
// against an empty database at Open. It pins the two decisions that matter —
// terminal jobs are left alone, and an ambiguous candidate (files spread over
// more than one remote directory, the condition commonLeaf refused) is skipped
// rather than guessed at.
func TestDownloadFolderBackfillCoversLiveJobsOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	liveID, _ := seedJobWithCandidate(t, s, 1006, []string{`music\Artist\Live Album\01.flac`, `music\Artist\Live Album\02.flac`}, now)
	doneID, _ := seedJobWithCandidate(t, s, 1007, []string{`music\Artist\Done Album\01.flac`}, now)
	ambiguousID, _ := seedJobWithCandidate(t, s, 1008, []string{`music\Artist\CD1\01.flac`, `music\Artist\CD2\01.flac`}, now)

	if err := s.AdvanceJobState(ctx, doneID, core.StateDone, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	if _, err := s.db.Exec(backfillStatement(t)); err != nil {
		t.Fatalf("run backfill: %v", err)
	}

	if leaves, err := s.DownloadFoldersForJob(ctx, liveID); err != nil || len(leaves) != 1 || leaves[0] != "Live Album" {
		t.Errorf("live job leaves = %v (%v), want [Live Album]", leaves, err)
	}
	if n := countDownloadFolders(t, s, doneID); n != 0 {
		t.Errorf("terminal job rows = %d, want 0", n)
	}
	if n := countDownloadFolders(t, s, ambiguousID); n != 0 {
		t.Errorf("ambiguous-candidate rows = %d, want 0 (the backfill must not guess)", n)
	}
}

// TestDownloadFolderBackfillIgnoresFilesWithNoDirectory pins the one way the
// backfill could register something actively dangerous: Postgres'
// regexp_replace leaves a string with no '/' untouched, so a peer sharing a
// track at its share root would yield leaf = the file's own name. cleanupFolder
// would then hand that to DeleteDownloadFolder, which is os.RemoveAll on the
// native backend — deleting a downloaded track. Go's commonLeaf returns "" for
// the same input, and the backfill must agree with it.
func TestDownloadFolderBackfillIgnoresFilesWithNoDirectory(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, _ := seedJobWithCandidate(t, s, 1009, []string{"bonus.flac"}, now)

	if _, err := s.db.Exec(backfillStatement(t)); err != nil {
		t.Fatalf("run backfill: %v", err)
	}
	if n := countDownloadFolders(t, s, jobID); n != 0 {
		t.Errorf("rows = %d, want 0: a bare filename names no folder", n)
	}
}

// backfillStatement extracts the INSERT half of migration 0015 from the
// embedded migrations, so the test exercises the shipped SQL rather than a
// copy that could drift from it.
func backfillStatement(t *testing.T) string {
	t.Helper()
	raw, err := migrationsFS.ReadFile("migrations/0015_job_download_folders.sql")
	if err != nil {
		t.Fatalf("read migration 0015: %v", err)
	}
	const marker = "WITH dirs AS ("
	i := strings.Index(string(raw), marker)
	if i < 0 {
		t.Fatalf("migration 0015 no longer contains %q; update this test", marker)
	}
	return string(raw)[i:]
}

// TestRegisterDownloadFolderTreatsCaseVariantsAsOneFolder is issue #479's
// runtime half: on a case-insensitive download root two casings are one
// directory, and registering both used to produce two rows — so cleanup stamped
// one and left the other uncleaned forever, naming a folder already gone.
//
// The surviving row keeps the casing it was first seen under. That value is
// handed verbatim to DeleteDownloadFolder, so it must stay a name something has
// actually been written under rather than the latest spelling observed.
func TestRegisterDownloadFolderTreatsCaseVariantsAsOneFolder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	jobID, _ := seedJobWithCandidate(t, s, 1100, []string{`music\Artist\Cd1\01.flac`}, now)

	for _, leaf := range []string{"Cd1", "cd1", "CD1"} {
		if _, _, err := s.RegisterDownloadFolder(ctx, jobID, leaf, now); err != nil {
			t.Fatalf("RegisterDownloadFolder(%q): %v", leaf, err)
		}
	}

	if n := countDownloadFolders(t, s, jobID); n != 1 {
		t.Errorf("rows = %d, want 1: three casings are one folder", n)
	}
	leaves, err := s.DownloadFoldersForJob(ctx, jobID)
	if err != nil || len(leaves) != 1 || leaves[0] != "Cd1" {
		t.Errorf("leaves = %v (%v), want [Cd1]: the first-seen casing is the one on disk", leaves, err)
	}
}

// TestMarkDownloadFolderCleanedMatchesCaseInsensitively pins the other side of
// the same comparison. A caller holding a differently-cased spelling of a
// registered folder must still be able to stamp it: matching exactly here while
// registering case-insensitively would leave the row permanently uncleaned,
// which is the defect #479 exists to remove.
func TestMarkDownloadFolderCleanedMatchesCaseInsensitively(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	jobID, _ := seedJobWithCandidate(t, s, 1101, []string{`music\Artist\Cd1\01.flac`}, now)
	if _, _, err := s.RegisterDownloadFolder(ctx, jobID, "Cd1", now); err != nil {
		t.Fatalf("RegisterDownloadFolder: %v", err)
	}

	if err := s.MarkDownloadFolderCleaned(ctx, jobID, "cd1", now); err != nil {
		t.Fatalf("MarkDownloadFolderCleaned: %v", err)
	}

	leaves, err := s.DownloadFoldersForJob(ctx, jobID)
	if err != nil || len(leaves) != 0 {
		t.Errorf("uncleaned leaves = %v (%v), want none", leaves, err)
	}
}

// TestDownloadFolderCaseDedupe exercises migration 0017's data step against the
// shipped SQL. The index the migration creates makes violating rows
// unreachable through the normal path, so the test drops it, seeds the rows the
// canary actually had, and re-runs the dedupe.
//
// The two decisions it pins are the ones that could lose data rather than
// merely tidy it: the survivor is the OLDEST row (agreeing with
// RegisterDownloadFolder, which never rewrites a row's casing), and it is
// uncleaned if ANY row in the group was uncleaned. A cleaned survivor hiding a
// folder still on disk is exactly the leak #314 closed.
func TestDownloadFolderCaseDedupe(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	older := now.Add(-time.Hour)

	if _, err := s.db.Exec(`DROP INDEX job_download_folders_job_lower_leaf_key`); err != nil {
		t.Fatalf("drop index (premise: migration 0017 created it): %v", err)
	}

	// Distinct remote filenames per job: activation refuses a candidate whose
	// remote file another live candidate already owns.
	mixed, _ := seedJobWithCandidate(t, s, 1102, []string{`music\Artist\Mixed\01.flac`}, now)
	allCleaned, _ := seedJobWithCandidate(t, s, 1103, []string{`music\Artist\Cleaned\01.flac`}, now)
	otherJob, _ := seedJobWithCandidate(t, s, 1104, []string{`music\Artist\Other\01.flac`}, now)

	// mixed: older row cleaned, newer row uncleaned. The survivor must be the
	// older casing AND uncleaned - neither row has both properties.
	rawInsertFolder(t, s, mixed, "Cd1", older, &now)
	rawInsertFolder(t, s, mixed, "cd1", now, nil)
	// allCleaned: nothing uncleaned, so the newest cleaned_at is kept.
	rawInsertFolder(t, s, allCleaned, "Greatest Hits", older, &older)
	rawInsertFolder(t, s, allCleaned, "greatest hits", now, &now)
	// otherJob: same leaf, different job - not a duplicate, must survive whole.
	rawInsertFolder(t, s, otherJob, "cd1", now, nil)

	stmts := dedupeStatements(t)
	if _, err := s.db.Exec(stmts); err != nil {
		t.Fatalf("run 0017 dedupe: %v", err)
	}

	if n := countDownloadFolders(t, s, mixed); n != 1 {
		t.Errorf("mixed rows = %d, want 1", n)
	}
	leaves, err := s.DownloadFoldersForJob(ctx, mixed)
	if err != nil || len(leaves) != 1 || leaves[0] != "Cd1" {
		t.Errorf("mixed uncleaned leaves = %v (%v), want [Cd1]: an uncleaned duplicate must not be hidden", leaves, err)
	}

	if n := countDownloadFolders(t, s, allCleaned); n != 1 {
		t.Errorf("all-cleaned rows = %d, want 1", n)
	}
	if leaves, err := s.DownloadFoldersForJob(ctx, allCleaned); err != nil || len(leaves) != 0 {
		t.Errorf("all-cleaned uncleaned leaves = %v (%v), want none", leaves, err)
	}

	if n := countDownloadFolders(t, s, otherJob); n != 1 {
		t.Errorf("other job rows = %d, want 1: uniqueness is per job, not global", n)
	}

	// The point of the dedupe: the index can now be created. If it cannot, the
	// migration would fail at startup and the container would not come up.
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX job_download_folders_job_lower_leaf_key
		ON job_download_folders (album_job_id, lower(leaf))`); err != nil {
		t.Fatalf("recreate index after dedupe: %v", err)
	}
}

// rawInsertFolder writes a register row directly, bypassing
// RegisterDownloadFolder, which is the only way to produce the case-variant
// rows migration 0017 exists to clean up.
func rawInsertFolder(t *testing.T, s *Store, jobID int64, leaf string, created time.Time, cleaned *time.Time) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO job_download_folders (album_job_id, leaf, created_at, cleaned_at)
		 VALUES ($1, $2, $3, $4)`, jobID, leaf, created, cleaned); err != nil {
		t.Fatalf("raw insert %q: %v", leaf, err)
	}
}

// dedupeStatements extracts migration 0017's data step - everything before the
// index it enables - from the embedded migrations, so the test exercises the
// shipped SQL rather than a copy that could drift from it.
func dedupeStatements(t *testing.T) string {
	t.Helper()
	raw, err := migrationsFS.ReadFile("migrations/0017_download_folder_leaf_case.sql")
	if err != nil {
		t.Fatalf("read migration 0017: %v", err)
	}
	const start = "WITH ranked AS ("
	const end = "CREATE UNIQUE INDEX"
	i := strings.Index(string(raw), start)
	j := strings.Index(string(raw), end)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("migration 0017 no longer contains %q .. %q; update this test", start, end)
	}
	return string(raw)[i:j]
}

// mustOwnFolder registers leaf for jobID and fails the test unless the job got
// it, so the ownership tests below read as a sequence of claims rather than a
// sequence of three-value assignments.
func mustOwnFolder(t *testing.T, s *Store, jobID int64, leaf string, now time.Time) {
	t.Helper()
	owner, ok, err := s.RegisterDownloadFolder(context.Background(), jobID, leaf, now)
	if err != nil {
		t.Fatalf("RegisterDownloadFolder(%d, %q): %v", jobID, leaf, err)
	}
	if !ok {
		t.Fatalf("job %d was refused folder %q, owner = %d", jobID, leaf, owner)
	}
}

// TestRegisterDownloadFolderBlocksASecondLiveJob is issue #471's core claim.
// Neither backend lets slusk choose the local directory - both derive it from
// the peer's share path - so two peers who happen to share a directory name are
// enough for two jobs to write into one directory at the same time. The second
// job must be told no, and must not leave a row behind: a row is what later
// entitles it to run a recursive delete over the first job's files.
func TestRegisterDownloadFolderBlocksASecondLiveJob(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	s := newTestStore(t)

	first, _ := seedJobWithCandidate(t, s, 1, []string{`music\A\cd1\01.flac`}, now)
	second, _ := seedJobWithCandidate(t, s, 2, []string{`music\B\cd1\01.flac`}, now)

	mustOwnFolder(t, s, first, "cd1", now)

	owner, ok, err := s.RegisterDownloadFolder(ctx, second, "cd1", now)
	if err != nil {
		t.Fatalf("RegisterDownloadFolder: %v", err)
	}
	if ok || owner != first {
		t.Fatalf("second job got (owner=%d, ok=%v), want (owner=%d, ok=false)", owner, ok, first)
	}
	if n := countDownloadFolders(t, s, second); n != 0 {
		t.Errorf("refused job left %d rows behind, want 0 - a row is a licence to delete", n)
	}
}

// TestRegisterDownloadFolderOwnershipIsCaseInsensitive: the same comparison
// that makes two casings one row (#479) has to decide ownership, or a peer
// sharing `CD1` walks straight into the folder a peer sharing `cd1` is using on
// a case-insensitive filesystem.
func TestRegisterDownloadFolderOwnershipIsCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	s := newTestStore(t)

	first, _ := seedJobWithCandidate(t, s, 1, []string{`music\A\Cd1\01.flac`}, now)
	second, _ := seedJobWithCandidate(t, s, 2, []string{`music\B\CD1\01.flac`}, now)

	mustOwnFolder(t, s, first, "Cd1", now)

	owner, ok, err := s.RegisterDownloadFolder(ctx, second, "CD1", now)
	if err != nil {
		t.Fatalf("RegisterDownloadFolder: %v", err)
	}
	if ok || owner != first {
		t.Fatalf("second job got (owner=%d, ok=%v), want (owner=%d, ok=false)", owner, ok, first)
	}
}

// TestRegisterDownloadFolderIgnoresIdleOwners is the correction that a canary
// measurement forced. Three live jobs held `cd1` there and only one was
// downloading: the other two carried rows left over from earlier search cycles,
// which ResetJobToWanted deliberately keeps (#314) because deleting them is
// what stranded folders on disk in the first place.
//
// So the row answers two questions and only one of them is state-dependent.
// Cleanup asks "may this hold my bytes" of every uncleaned row; ownership asks
// "is someone writing there now", which only DOWNLOADING and IMPORTING can be
// true of. The idle job's row must survive being ignored - losing it would
// trade this bug for the leak #314 closed.
func TestRegisterDownloadFolderIgnoresIdleOwners(t *testing.T) {
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	s := newTestStore(t)
	ctx := context.Background()

	idle, _ := seedJobWithCandidate(t, s, 1, []string{`music\A\cd1\01.flac`}, now)
	mustOwnFolder(t, s, idle, "cd1", now)
	if err := s.ResetJobToWanted(ctx, idle, core.StateDownloading, 0, nil, now); err != nil {
		t.Fatalf("ResetJobToWanted: %v", err)
	}

	live, _ := seedJobWithCandidate(t, s, 2, []string{`music\B\cd1\01.flac`}, now)
	mustOwnFolder(t, s, live, "cd1", now)

	leaves, err := s.DownloadFoldersForJob(ctx, idle)
	if err != nil {
		t.Fatalf("DownloadFoldersForJob: %v", err)
	}
	if len(leaves) != 1 || leaves[0] != "cd1" {
		t.Errorf("idle job's uncleaned leaves = %v, want [cd1] - cleanup still has to find it", leaves)
	}
}

// TestRegisterDownloadFolderReleasesOnCleanup: cleanup stamping cleaned_at is
// what hands the folder on. Without this the first collision on a name like
// `Digital Media 02` would block it for the lifetime of the deployment.
func TestRegisterDownloadFolderReleasesOnCleanup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	s := newTestStore(t)

	first, _ := seedJobWithCandidate(t, s, 1, []string{`music\A\cd1\01.flac`}, now)
	second, _ := seedJobWithCandidate(t, s, 2, []string{`music\B\cd1\01.flac`}, now)

	mustOwnFolder(t, s, first, "cd1", now)
	if _, ok, err := s.RegisterDownloadFolder(ctx, second, "cd1", now); err != nil || ok {
		t.Fatalf("second job should be blocked before cleanup, got ok=%v err=%v", ok, err)
	}
	if err := s.MarkDownloadFolderCleaned(ctx, first, "cd1", now); err != nil {
		t.Fatalf("MarkDownloadFolderCleaned: %v", err)
	}
	mustOwnFolder(t, s, second, "cd1", now.Add(time.Minute))
}

// TestRegisterDownloadFolderReclaimIsNotAConflict: a job re-registering a
// folder it already holds must stay a success. Every file of a candidate
// registers, and Downloading re-registers on every tick, so treating the job's
// own row as a live owner would deadlock it against itself on file two.
func TestRegisterDownloadFolderReclaimIsNotAConflict(t *testing.T) {
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	s := newTestStore(t)

	jobID, _ := seedJobWithCandidate(t, s, 1, []string{`music\A\cd1\01.flac`}, now)
	mustOwnFolder(t, s, jobID, "cd1", now)
	mustOwnFolder(t, s, jobID, "cd1", now.Add(time.Minute))
	mustOwnFolder(t, s, jobID, "CD1", now.Add(2*time.Minute))

	if n := countDownloadFolders(t, s, jobID); n != 1 {
		t.Errorf("reclaims left %d rows, want 1", n)
	}
}

// TestRegisterDownloadFolderConcurrent is the reason the claim runs inside a
// transaction holding an advisory lock. Every store transaction here is READ
// COMMITTED, so a lookup followed by an INSERT does not serialize: both jobs
// see the folder free and both claim it, and the test that would have caught
// it passes because it never ran the two at once.
//
// Follows ActivateCandidateWithTransfers' concurrency test - all goroutines
// released from one channel, exactly one winner.
func TestRegisterDownloadFolderConcurrent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	s := newTestStore(t)

	const racers = 5
	jobs := make([]int64, racers)
	for i := range jobs {
		jobs[i], _ = seedJobWithCandidate(t, s,
			int64(i+1), []string{fmt.Sprintf(`music\Artist %d\cd1\01.flac`, i)}, now)
	}

	type result struct {
		owner int64
		ok    bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, racers)
	var wg sync.WaitGroup
	for _, jobID := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			owner, ok, err := s.RegisterDownloadFolder(ctx, jobID, "cd1", now)
			results <- result{owner: owner, ok: ok, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	winners, losers := 0, 0
	for r := range results {
		switch {
		case r.err != nil:
			t.Fatalf("concurrent registration: %v", r.err)
		case r.ok:
			winners++
		default:
			losers++
			if r.owner == 0 {
				t.Error("a refused job was told ok=false with no owner to wait for")
			}
		}
	}
	if winners != 1 || losers != racers-1 {
		t.Fatalf("registration results: winners=%d losers=%d, want 1/%d", winners, losers, racers-1)
	}
}

// TestDeferCandidateKeepsTheFirstTimestamp pins the two properties the ceiling
// and the event both hang off: the clock starts once, and `first` is true
// exactly once. A candidate re-deferred every tick that refreshed its own
// timestamp would push its deadline forward forever, and the wait would never
// be broken.
func TestDeferCandidateKeepsTheFirstTimestamp(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	s := newTestStore(t)

	_, candID := seedJobWithCandidate(t, s, 1, []string{`music\A\cd1\01.flac`}, now)

	since, first, err := s.DeferCandidate(ctx, candID, now)
	if err != nil || !first || !since.Equal(now) {
		t.Fatalf("first deferral = (%v, %v, %v), want (%v, true, nil)", since, first, err, now)
	}
	later := now.Add(30 * time.Minute)
	since, first, err = s.DeferCandidate(ctx, candID, later)
	if err != nil || first || !since.Equal(now) {
		t.Fatalf("second deferral = (%v, %v, %v), want (%v, false, nil)", since, first, err, now)
	}

	if err := s.ClearCandidateDeferral(ctx, candID); err != nil {
		t.Fatalf("ClearCandidateDeferral: %v", err)
	}
	// A later wait starts a fresh clock, and reports itself as fresh: inheriting
	// the old timestamp would fail the candidate on its first deferred tick.
	since, first, err = s.DeferCandidate(ctx, candID, later)
	if err != nil || !first || !since.Equal(later) {
		t.Fatalf("deferral after clear = (%v, %v, %v), want (%v, true, nil)", since, first, err, later)
	}
}
