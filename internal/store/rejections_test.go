package store

import (
	"context"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// helperActivateFiles is helperActivate with a caller-chosen peer and file list,
// so a test can control the (username, release directory) a rejection is keyed
// on.
func helperActivateFiles(t *testing.T, s *Store, albumID int64, username string, files []core.CandidateFile, now time.Time) (jobID, candID int64) {
	t.Helper()
	ctx := context.Background()
	job, err := s.UpsertWantedJob(ctx, albumID, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: username, Score: 1.0, Files: files}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}
	ok, _, err := s.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, 100, now.Add(time.Hour), now)
	if err != nil || !ok {
		t.Fatalf("ActivateCandidateWithTransfers: %v ok=%v", err, ok)
	}
	return job.ID, cand.ID
}

// TestRejectCandidateAndAdvanceRecordsRejection: a content failure records the
// candidate in the job's rejection history, keyed on (username, release
// directory) derived from the candidate's cached files.
func TestRejectCandidateAndAdvanceRecordsRejection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 900, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	if _, err := s.RejectCandidateAndAdvance(ctx, candID, jobID, "import rejected", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}

	got, err := s.RejectedCandidates(ctx, jobID)
	if err != nil {
		t.Fatalf("RejectedCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 rejection, got %d (%+v)", len(got), got)
	}
	if got[0].Username != "alice" {
		t.Errorf("username = %q, want alice", got[0].Username)
	}
	if got[0].ReleaseDir != "music/Artist - Album" {
		t.Errorf("release dir = %q, want %q", got[0].ReleaseDir, "music/Artist - Album")
	}
	if got[0].Reason != "import rejected" {
		t.Errorf("reason = %q, want %q", got[0].Reason, "import rejected")
	}
}

// TestSucceedCandidateAndAdvanceRecordsNoRejection: success never blacklists a
// peer. A rejection written for a candidate that actually worked would lock a
// good peer out for the rest of the job's life.
func TestSucceedCandidateAndAdvanceRecordsNoRejection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 901, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	if _, err := s.SucceedCandidateAndAdvance(ctx, candID, jobID, core.StateDownloading, core.StateImporting, now); err != nil {
		t.Fatalf("SucceedCandidateAndAdvance: %v", err)
	}

	got, err := s.RejectedCandidates(ctx, jobID)
	if err != nil {
		t.Fatalf("RejectedCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no rejections after a success, got %+v", got)
	}
}

// TestRejectCandidateAndAdvanceRollbackRecordsNoRejection: the rejection shares
// the transaction with the candidate/job writes, so a bounced transition leaves
// no history behind either.
func TestRejectCandidateAndAdvanceRollbackRecordsNoRejection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 902, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	// The job leaves DOWNLOADING underneath us, so the whole tx rolls back.
	if err := s.AdvanceJobState(ctx, jobID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	transitioned, err := s.RejectCandidateAndAdvance(ctx, candID, jobID, "transfer failed", core.StateDownloading, core.StateSelecting, now)
	if err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}
	if transitioned {
		t.Fatal("expected transitioned=false when the job already left its from-state")
	}
	got, err := s.RejectedCandidates(ctx, jobID)
	if err != nil {
		t.Fatalf("RejectedCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no rejection when the transaction rolled back, got %+v", got)
	}
}

// TestResetJobToWantedKeepsRejections is the point of the whole table: the
// automatic retry cycle deletes the job's candidates, and the rejection history
// must outlive that deletion or the next search re-caches what just failed.
func TestResetJobToWantedKeepsRejections(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 903, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	if _, err := s.RejectCandidateAndAdvance(ctx, candID, jobID, "import rejected", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}
	if err := s.ResetJobToWanted(ctx, jobID, core.StateSelecting, 1, nil, now); err != nil {
		t.Fatalf("ResetJobToWanted: %v", err)
	}

	if cands, err := s.CandidatesForJob(ctx, jobID); err != nil || len(cands) != 0 {
		t.Fatalf("expected candidates deleted, got %d (%v)", len(cands), err)
	}
	got, err := s.RejectedCandidates(ctx, jobID)
	if err != nil {
		t.Fatalf("RejectedCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rejection history must survive ResetJobToWanted, got %+v", got)
	}
}

// TestRetryFailedJobClearsRejections: an explicit user retry is the escape hatch
// for a peer that has since fixed its share. It resets retries to 0, so the job
// starts over rather than continuing the same attempt.
func TestRetryFailedJobClearsRejections(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 904, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	if _, err := s.RejectCandidateAndAdvance(ctx, candID, jobID, "import rejected", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}
	if err := s.MarkJobFailed(ctx, jobID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}
	ok, err := s.RetryFailedJob(ctx, jobID, now)
	if err != nil || !ok {
		t.Fatalf("RetryFailedJob: %v ok=%v", err, ok)
	}

	got, err := s.RejectedCandidates(ctx, jobID)
	if err != nil {
		t.Fatalf("RejectedCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected rejection history cleared by an explicit retry, got %+v", got)
	}
}

// TestDeleteJobCascadesRejections: the history dies with the job, which is what
// keeps it from becoming a permanent global blacklist.
func TestDeleteJobCascadesRejections(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 905, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	if _, err := s.RejectCandidateAndAdvance(ctx, candID, jobID, "import rejected", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}
	ok, err := s.DeleteJob(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("DeleteJob: %v ok=%v", err, ok)
	}

	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM candidate_rejections WHERE album_job_id = $1`, jobID).Scan(&n); err != nil {
		t.Fatalf("count rejections: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected rejections to cascade with the job, %d left", n)
	}
}

// TestFailCandidateAndAdvanceRecordsNoRejection is the other half of the
// Fail/Reject split: an environmental failure - a peer dropping mid-transfer, an
// import stuck past its timeout - says nothing about the candidate's files, so
// it must not blacklist the peer for the rest of the job. Without this, one
// timeout can make an album with a single seeder permanently unfetchable.
func TestFailCandidateAndAdvanceRecordsNoRejection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 906, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	if _, err := s.FailCandidateAndAdvance(ctx, candID, jobID, "transfer failed", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("FailCandidateAndAdvance: %v", err)
	}

	got, err := s.RejectedCandidates(ctx, jobID)
	if err != nil {
		t.Fatalf("RejectedCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a transfer failure must not blacklist the peer, got %+v", got)
	}
}

// TestForceSearchJobKeepsRejections: force search is the nudge a user reaches
// for when a job looks stuck - which is exactly what issue #317 looks like from
// the outside. Clearing the history here would send the next search straight
// back to re-downloading the files that have been failing import.
func TestForceSearchJobKeepsRejections(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 907, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	if _, err := s.RejectCandidateAndAdvance(ctx, candID, jobID, "import rejected", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}
	ok, err := s.ForceSearchJob(ctx, jobID, now)
	if err != nil || !ok {
		t.Fatalf("ForceSearchJob: %v ok=%v", err, ok)
	}

	got, err := s.RejectedCandidates(ctx, jobID)
	if err != nil {
		t.Fatalf("RejectedCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("force search must keep the rejection history, got %+v", got)
	}
}

// TestUpsertWantedJobReEnterClearsRejections: re-monitoring a cancelled album is
// a start-over, and this is the singular twin of SyncWantedJobs' reentered CTE.
// The two paths must agree, or the same user action behaves differently
// depending on which one the album happened to arrive through.
func TestUpsertWantedJobReEnterClearsRejections(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 908, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	if _, err := s.RejectCandidateAndAdvance(ctx, candID, jobID, "import rejected", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}
	if err := s.AdvanceJobState(ctx, jobID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	if _, err := s.UpsertWantedJob(ctx, 908, now); err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	got, err := s.RejectedCandidates(ctx, jobID)
	if err != nil {
		t.Fatalf("RejectedCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("re-entering a cancelled job must clear the history, got %+v", got)
	}
}

// TestSyncWantedJobsReEnterClearsRejections covers the set-at-a-time re-enter
// CTE. Its deleted_rejections block is copy-pasted across three queries, so each
// gets its own test: mis-scoping one leaves revived jobs permanently
// blacklisting old peers with the whole suite green.
func TestSyncWantedJobsReEnterClearsRejections(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 909, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	if _, err := s.RejectCandidateAndAdvance(ctx, candID, jobID, "import rejected", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}
	if err := s.AdvanceJobState(ctx, jobID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	if _, _, err := s.SyncWantedJobs(ctx, []core.WantedRelease{{ID: 909, Title: "Album", ArtistName: "Artist"}},
		now.Add(-30*24*time.Hour), now); err != nil {
		t.Fatalf("SyncWantedJobs: %v", err)
	}

	got, err := s.RejectedCandidates(ctx, jobID)
	if err != nil {
		t.Fatalf("RejectedCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("re-entering a cancelled job must clear the history, got %+v", got)
	}
}

// TestSyncWantedJobsReviveFailedClearsRejections covers the failed-revive CTE:
// after the failed_revive_after window the job starts over from scratch, and a
// peer that has since fixed its share deserves another look.
func TestSyncWantedJobsReviveFailedClearsRejections(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 910, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	if _, err := s.RejectCandidateAndAdvance(ctx, candID, jobID, "import rejected", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}
	if err := s.MarkJobFailed(ctx, jobID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	later := now.Add(60 * 24 * time.Hour)
	if _, revived, err := s.SyncWantedJobs(ctx, []core.WantedRelease{{ID: 910, Title: "Album", ArtistName: "Artist"}},
		later.Add(-30*24*time.Hour), later); err != nil || revived != 1 {
		t.Fatalf("SyncWantedJobs: %v revived=%d", err, revived)
	}

	got, err := s.RejectedCandidates(ctx, jobID)
	if err != nil {
		t.Fatalf("RejectedCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("reviving a long-failed job must clear the history, got %+v", got)
	}
}

// TestBulkRetryJobsClearsRejections covers the third copy of the CTE: bulk retry
// is RetryFailedJob's set-at-a-time form and must agree with it.
func TestBulkRetryJobsClearsRejections(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	jobID, candID := helperActivateFiles(t, s, 911, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)

	if _, err := s.RejectCandidateAndAdvance(ctx, candID, jobID, "import rejected", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}
	if err := s.MarkJobFailed(ctx, jobID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}
	res, err := s.BulkRetryJobs(ctx, bulkRetryQuery("failed"), now)
	if err != nil || res.Retried != 1 {
		t.Fatalf("BulkRetryJobs: %v result=%+v", err, res)
	}

	got, err := s.RejectedCandidates(ctx, jobID)
	if err != nil {
		t.Fatalf("RejectedCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("bulk retry must clear the history, got %+v", got)
	}
}

// TestCountRejectionsByReason: the cap Importing applies (issue #472) is only
// as good as this count, so it is asserted directly - per reason, distinct in
// the (username, release directory) key, and zero for a job that has none.
func TestCountRejectionsByReason(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	jobID, candID := helperActivateFiles(t, s, 940, "alice",
		[]core.CandidateFile{{Filename: `music\Artist - Album\01.flac`, Size: 1}}, now)
	if n, err := s.CountRejectionsByReason(ctx, jobID, string(core.ReasonIncompleteDownload)); err != nil || n != 0 {
		t.Fatalf("count before any rejection = %d (%v), want 0", n, err)
	}
	if _, err := s.RejectCandidateAndAdvance(ctx, candID, jobID,
		string(core.ReasonIncompleteDownload), core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("RejectCandidateAndAdvance: %v", err)
	}

	// A second peer, same reason: counted.
	if err := s.InsertCandidates(ctx, jobID, []NewCandidate{
		{Username: "bob", Score: 1.0, Files: []core.CandidateFile{{Filename: `music\Other Dir\01.flac`, Size: 1}}},
	}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	bob, found, err := s.NextNewCandidate(ctx, jobID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}
	if ok, _, err := s.ActivateCandidateWithTransfers(ctx, bob.ID, jobID, 100, now.Add(time.Hour), now); err != nil || !ok {
		t.Fatalf("ActivateCandidateWithTransfers: %v ok=%v", err, ok)
	}
	// ActivateCandidateWithTransfers leaves the job DOWNLOADING, which is the
	// state the rejection must advance out of - a bounced job guard rolls the
	// rejection back with it.
	if ok, err := s.RejectCandidateAndAdvance(ctx, bob.ID, jobID,
		string(core.ReasonIncompleteDownload), core.StateDownloading, core.StateSelecting, now); err != nil || !ok {
		t.Fatalf("RejectCandidateAndAdvance (bob): %v ok=%v", err, ok)
	}

	if n, err := s.CountRejectionsByReason(ctx, jobID, string(core.ReasonIncompleteDownload)); err != nil || n != 2 {
		t.Errorf("count = %d (%v), want 2", n, err)
	}
	// A different reason shares neither count.
	if n, err := s.CountRejectionsByReason(ctx, jobID, string(core.ReasonImportRejected)); err != nil || n != 0 {
		t.Errorf("count for the other reason = %d (%v), want 0", n, err)
	}
	// Another job's history is its own.
	// A different peer and filename: one live transfer per (peer, file) is a
	// unique index, so reusing alice's would silently fail to activate.
	otherJob, _ := helperActivateFiles(t, s, 941, "dave",
		[]core.CandidateFile{{Filename: `music\Another Album\01.flac`, Size: 1}}, now)
	if n, err := s.CountRejectionsByReason(ctx, otherJob, string(core.ReasonIncompleteDownload)); err != nil || n != 0 {
		t.Errorf("other job's count = %d (%v), want 0", n, err)
	}
}
