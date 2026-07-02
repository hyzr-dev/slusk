package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestWriteAheadEnqueueAndRecover(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertDiscoveredJob(ctx, 42, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	attemptID, err := s.CreateAttempt(ctx, job.ID, "bob", 1.5, now)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	// Step 1 of write-ahead: intent persisted, no slskd id yet.
	deadline := now.Add(30 * time.Minute)
	tid, err := s.RecordEnqueueIntent(ctx, attemptID, "bob", "album/01.flac", deadline, now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}

	// Simulate a crash before AttachTransferID: recover by fallback key.
	tr, found, err := s.FindTransferByFallback(ctx, "bob", "album/01.flac")
	if err != nil || !found {
		t.Fatalf("FindTransferByFallback found=%v err=%v", found, err)
	}
	if tr.SlskdID != "" {
		t.Errorf("expected empty slskd_id before attach, got %q", tr.SlskdID)
	}
	if tr.ID != tid {
		t.Errorf("recovered transfer id mismatch")
	}
	if tr.State != core.TransferQueued {
		t.Errorf("expected QUEUED state, got %v", tr.State)
	}

	// Step 2: attach the id.
	if err := s.AttachTransferID(ctx, tid, "slskd-guid-1", now); err != nil {
		t.Fatalf("AttachTransferID: %v", err)
	}
	tr2, _, _ := s.FindTransferByFallback(ctx, "bob", "album/01.flac")
	if tr2.SlskdID != "slskd-guid-1" {
		t.Errorf("slskd_id = %q, want slskd-guid-1", tr2.SlskdID)
	}
}

func TestTransfersPastDeadline(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertDiscoveredJob(ctx, 1, now)
	attemptID, _ := s.CreateAttempt(ctx, job.ID, "bob", 1.0, now)
	// Deadline already in the past.
	_, _ = s.RecordEnqueueIntent(ctx, attemptID, "bob", "f.flac", now.Add(-time.Minute), now)

	overdue, err := s.TransfersPastDeadline(ctx, now)
	if err != nil {
		t.Fatalf("TransfersPastDeadline: %v", err)
	}
	if len(overdue) != 1 {
		t.Fatalf("expected 1 overdue transfer, got %d", len(overdue))
	}
}

func TestRecordEnqueueIntentIsConflictSafe(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertDiscoveredJob(ctx, 300, now)
	a1, _ := s.CreateAttempt(ctx, job.ID, "bob", 1.0, now)

	id1, err := s.RecordEnqueueIntent(ctx, a1, "bob", "same.flac", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("first intent: %v", err)
	}
	// A later attempt re-enqueues the same (username, filename) — must not error,
	// and must return the existing row rather than creating a duplicate.
	id2, err := s.RecordEnqueueIntent(ctx, a1, "bob", "same.flac", now.Add(2*time.Hour), now)
	if err != nil {
		t.Fatalf("second intent (conflict) errored: %v", err)
	}
	if id1 != id2 {
		t.Errorf("expected same transfer row on conflict, got %d and %d", id1, id2)
	}
}

func TestRetryTransferAccumulatesAndSurvivesResend(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertDiscoveredJob(ctx, 500, now)
	a, _ := s.CreateAttempt(ctx, job.ID, "bob", 1.0, now)

	only := func() core.Transfer {
		t.Helper()
		trs, err := s.TransfersForAttempt(ctx, a)
		if err != nil || len(trs) != 1 {
			t.Fatalf("TransfersForAttempt: %v (n=%d)", err, len(trs))
		}
		return trs[0]
	}

	if err := s.RecordPendingTransfer(ctx, a, "bob", "t.flac", 42, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, a, "bob", "t.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}

	// Peer rejected it: retry sends it back to PENDING and bumps the count.
	if err := s.RetryTransfer(ctx, only().ID, now); err != nil {
		t.Fatalf("RetryTransfer: %v", err)
	}
	if tr := only(); tr.State != core.TransferPending || tr.Retries != 1 {
		t.Fatalf("after retry: state=%v retries=%d, want PENDING/1", tr.State, tr.Retries)
	}

	// Resending must NOT reset the retry count, or the bound is never reached and
	// a rejection loops forever.
	if _, err := s.RecordEnqueueIntent(ctx, a, "bob", "t.flac", now.Add(2*time.Hour), now); err != nil {
		t.Fatalf("resend RecordEnqueueIntent: %v", err)
	}
	if tr := only(); tr.Retries != 1 || tr.State != core.TransferQueued {
		t.Errorf("after resend: state=%v retries=%d, want QUEUED/1", tr.State, tr.Retries)
	}
}

func TestUpsertDiscoveredJobIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a, _ := s.UpsertDiscoveredJob(ctx, 7, now)
	b, err := s.UpsertDiscoveredJob(ctx, 7, now)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if a.ID != b.ID {
		t.Errorf("upsert created a duplicate job: %d != %d", a.ID, b.ID)
	}
}

func TestUpdateJobMetadataSetsTitleAndArtist(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertDiscoveredJob(ctx, 42, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	if job.Title != "" || job.ArtistName != "" {
		t.Fatalf("expected empty title/artist before UpdateJobMetadata, got %q / %q", job.Title, job.ArtistName)
	}

	if err := s.UpdateJobMetadata(ctx, job.ID, "Untrue", "Burial", now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}

	jobs, err := s.JobsInState(ctx, core.StateDiscovered, 10)
	if err != nil {
		t.Fatalf("JobsInState: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Title != "Untrue" || jobs[0].ArtistName != "Burial" {
		t.Errorf("title/artist = %q / %q, want Untrue / Burial", jobs[0].Title, jobs[0].ArtistName)
	}
}

func TestBackfillJobMetadataIfEmptyFillsBlankFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertDiscoveredJob(ctx, 50, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}

	if err := s.BackfillJobMetadataIfEmpty(ctx, job.ID, "Title A", "Artist A"); err != nil {
		t.Fatalf("BackfillJobMetadataIfEmpty: %v", err)
	}

	jobs, err := s.JobsInState(ctx, core.StateDiscovered, 10)
	if err != nil {
		t.Fatalf("JobsInState: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Title != "Title A" || jobs[0].ArtistName != "Artist A" {
		t.Errorf("title/artist = %q / %q, want Title A / Artist A", jobs[0].Title, jobs[0].ArtistName)
	}
}

func TestBackfillJobMetadataIfEmptyDoesNotOverwriteExisting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 51, now)
	if err := s.UpdateJobMetadata(ctx, job.ID, "Original Title", "Original Artist", now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}

	if err := s.BackfillJobMetadataIfEmpty(ctx, job.ID, "New Title", "New Artist"); err != nil {
		t.Fatalf("BackfillJobMetadataIfEmpty: %v", err)
	}

	jobs, err := s.JobsInState(ctx, core.StateDiscovered, 10)
	if err != nil {
		t.Fatalf("JobsInState: %v", err)
	}
	if jobs[0].Title != "Original Title" || jobs[0].ArtistName != "Original Artist" {
		t.Errorf("expected existing metadata preserved, got %q / %q", jobs[0].Title, jobs[0].ArtistName)
	}
}

func TestBackfillJobMetadataIfEmptyDoesNotTouchUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createdAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 52, createdAt)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateFailed, createdAt); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	if err := s.BackfillJobMetadataIfEmpty(ctx, job.ID, "Legacy Title", "Legacy Artist"); err != nil {
		t.Fatalf("BackfillJobMetadataIfEmpty: %v", err)
	}

	failed, err := s.JobsInState(ctx, core.StateFailed, 10)
	if err != nil {
		t.Fatalf("JobsInState: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("expected 1 FAILED job, got %d", len(failed))
	}
	if !failed[0].UpdatedAt.Equal(createdAt) {
		t.Errorf("updated_at = %v, want unchanged %v (must not reset the retry-cooldown clock)", failed[0].UpdatedAt, createdAt)
	}
	if failed[0].Title != "Legacy Title" || failed[0].ArtistName != "Legacy Artist" {
		t.Errorf("title/artist = %q / %q, want Legacy Title / Legacy Artist", failed[0].Title, failed[0].ArtistName)
	}
}
