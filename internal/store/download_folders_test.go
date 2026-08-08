package store

import (
	"context"
	"errors"
	"strings"
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
		if err := s.RegisterDownloadFolder(ctx, jobID, "Album", now); err != nil {
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

	if err := s.RegisterDownloadFolder(ctx, jobID, "Album", now); err != nil {
		t.Fatalf("RegisterDownloadFolder: %v", err)
	}
	if err := s.MarkDownloadFolderCleaned(ctx, jobID, "Album", now); err != nil {
		t.Fatalf("MarkDownloadFolderCleaned: %v", err)
	}
	if leaves, err := s.DownloadFoldersForJob(ctx, jobID); err != nil || len(leaves) != 0 {
		t.Fatalf("after cleaning, leaves = %v (%v), want none", leaves, err)
	}

	if err := s.RegisterDownloadFolder(ctx, jobID, "Album", now.Add(time.Hour)); err != nil {
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
		err := s.RegisterDownloadFolder(ctx, jobID, leaf, now)
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
	if err := s.RegisterDownloadFolder(ctx, jobID, "First Album", now); err != nil {
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
	if err := s.RegisterDownloadFolder(ctx, jobID, "Album", now); err != nil {
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
