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
