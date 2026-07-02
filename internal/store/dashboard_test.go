package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestListJobsWithTransferIncludesJobsWithoutAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertDiscoveredJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	if err := s.UpdateJobMetadata(ctx, job.ID, "Rounds", "Four Tet", now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	v := views[0]
	if v.Job.Title != "Rounds" || v.Job.ArtistName != "Four Tet" {
		t.Errorf("title/artist = %q / %q, want Rounds / Four Tet", v.Job.Title, v.Job.ArtistName)
	}
	if v.Transfer != nil {
		t.Errorf("expected nil Transfer for a job with no attempt, got %+v", v.Transfer)
	}
	if v.Peer != "" {
		t.Errorf("expected empty Peer, got %q", v.Peer)
	}
}

func TestListJobsWithTransferJoinsLatestTransfer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 2, now)
	_ = s.UpdateJobMetadata(ctx, job.ID, "Dummy", "Portishead", now)

	// First (older) attempt/transfer.
	a1, err := s.CreateAttempt(ctx, job.ID, "peer_one", 1.0, now)
	if err != nil {
		t.Fatalf("CreateAttempt a1: %v", err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, a1, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent a1: %v", err)
	}

	// Second (newer) attempt/transfer — this is the one ListJobsWithTransfer must surface.
	later := now.Add(time.Minute)
	a2, err := s.CreateAttempt(ctx, job.ID, "peer_two", 2.0, later)
	if err != nil {
		t.Fatalf("CreateAttempt a2: %v", err)
	}
	tid2, err := s.RecordEnqueueIntent(ctx, a2, "peer_two", "f2.flac", later.Add(time.Hour), later)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent a2: %v", err)
	}
	if err := s.UpdateTransferProgress(ctx, tid2, core.TransferInProgress, 512, 1024, later); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	v := views[0]
	if v.Transfer == nil {
		t.Fatalf("expected non-nil Transfer")
	}
	if v.Peer != "peer_two" {
		t.Errorf("Peer = %q, want peer_two (the newer attempt)", v.Peer)
	}
	if v.Transfer.State != core.TransferInProgress || v.Transfer.BytesDone != 512 {
		t.Errorf("Transfer = %+v, want state IN_PROGRESS bytesDone 512", v.Transfer)
	}
}

func TestListJobsWithTransferExcludesCancelled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 3, now)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateCancelled, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("expected 0 views (cancelled job excluded), got %d", len(views))
	}
}

// TestListJobsWithTransferDedupesMultiTransferAttempt reproduces the
// multi-track-album scenario: a single candidate_attempt gets one transfer
// row per file (see engine.Discovery, one RecordEnqueueIntent call per
// cand.Files entry). ListJobsWithTransfer must still return exactly one
// JobView per job, picking the most recently updated transfer.
func TestListJobsWithTransferDedupesMultiTransferAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 5, now)
	_ = s.UpdateJobMetadata(ctx, job.ID, "Multi Track Album", "Boards of Canada", now)

	attempt, err := s.CreateAttempt(ctx, job.ID, "album_peer", 1.0, now)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	// First file of the album.
	if _, err := s.RecordEnqueueIntent(ctx, attempt, "album_peer", "track1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent track1: %v", err)
	}

	// Second file of the same album, same attempt, enqueued slightly later.
	later := now.Add(time.Minute)
	tid2, err := s.RecordEnqueueIntent(ctx, attempt, "album_peer", "track2.flac", later.Add(time.Hour), later)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent track2: %v", err)
	}
	// Bump its updated_at further so it's unambiguously the most recent transfer.
	evenLater := later.Add(time.Minute)
	if err := s.UpdateTransferProgress(ctx, tid2, core.TransferInProgress, 128, 1024, evenLater); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view (one job, not one per transfer), got %d", len(views))
	}
	v := views[0]
	if v.Transfer == nil {
		t.Fatalf("expected non-nil Transfer")
	}
	if v.Transfer.Filename != "track2.flac" {
		t.Errorf("Transfer.Filename = %q, want track2.flac (the more recently updated transfer)", v.Transfer.Filename)
	}
	if v.Transfer.State != core.TransferInProgress || v.Transfer.BytesDone != 128 {
		t.Errorf("Transfer = %+v, want state IN_PROGRESS bytesDone 128", v.Transfer)
	}
}

func TestJobWithTransferNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, found, err := s.JobWithTransfer(ctx, 99999)
	if err != nil {
		t.Fatalf("JobWithTransfer: %v", err)
	}
	if found {
		t.Error("expected found=false for nonexistent job id")
	}
}

func TestJobWithTransferFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 4, now)
	a1, _ := s.CreateAttempt(ctx, job.ID, "solo_peer", 1.0, now)
	if _, err := s.RecordEnqueueIntent(ctx, a1, "solo_peer", "solo.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}

	v, found, err := s.JobWithTransfer(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobWithTransfer: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if v.Peer != "solo_peer" {
		t.Errorf("Peer = %q, want solo_peer", v.Peer)
	}
}
