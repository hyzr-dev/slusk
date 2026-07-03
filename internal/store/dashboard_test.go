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
	if err := s.UpdateJobMetadata(ctx, job.ID, "Rounds", "Four Tet", "", 0, now); err != nil {
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
	_ = s.UpdateJobMetadata(ctx, job.ID, "Dummy", "Portishead", "", 0, now)

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
	_ = s.UpdateJobMetadata(ctx, job.ID, "Multi Track Album", "Boards of Canada", "", 0, now)

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

func TestListJobsWithTransferPopulatesAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 6, now)
	_ = s.UpdateJobMetadata(ctx, job.ID, "Failed Album", "Some Artist", "", 0, now)

	attemptID, err := s.CreateAttempt(ctx, job.ID, "flaky_peer", 1.5, now)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	backoff := now.Add(time.Hour)
	if err := s.FailAttempt(ctx, attemptID, "transfer failed", backoff, now); err != nil {
		t.Fatalf("FailAttempt: %v", err)
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	v := views[0]
	if v.Attempt == nil {
		t.Fatalf("expected non-nil Attempt")
	}
	if v.Attempt.Username != "flaky_peer" {
		t.Errorf("Attempt.Username = %q, want flaky_peer", v.Attempt.Username)
	}
	if v.Attempt.FailReason != "transfer failed" {
		t.Errorf("Attempt.FailReason = %q, want %q", v.Attempt.FailReason, "transfer failed")
	}
	if v.Attempt.State != "FAILED" {
		t.Errorf("Attempt.State = %q, want FAILED", v.Attempt.State)
	}
	if v.Attempt.BackoffUntil == nil || !v.Attempt.BackoffUntil.Equal(backoff) {
		t.Errorf("Attempt.BackoffUntil = %v, want %v", v.Attempt.BackoffUntil, backoff)
	}
}

func TestListJobsWithTransferNilAttemptWhenNoAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 7, now)
	_ = s.UpdateJobMetadata(ctx, job.ID, "No Attempt Album", "Nobody", "", 0, now)

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	if views[0].Attempt != nil {
		t.Errorf("expected nil Attempt, got %+v", views[0].Attempt)
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

func TestJobDetailNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, found, err := s.JobDetail(ctx, 99999)
	if err != nil {
		t.Fatalf("JobDetail: %v", err)
	}
	if found {
		t.Error("expected found=false for nonexistent job id")
	}
}

// TestJobDetailIncludesAllAttemptsAndTransfersNewestFirst reproduces the
// dashboard's per-job detail panel: every candidate attempt made for the job
// (not just the latest, unlike JobWithTransfer/ListJobsWithTransfer), newest
// first, each with its own per-file transfers.
func TestJobDetailIncludesAllAttemptsAndTransfersNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 10, now)
	_ = s.UpdateJobMetadata(ctx, job.ID, "Album", "Artist", "", 0, now)

	a1, err := s.CreateAttempt(ctx, job.ID, "peer_one", 1.0, now)
	if err != nil {
		t.Fatalf("CreateAttempt a1: %v", err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, a1, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent a1/f1: %v", err)
	}
	if err := s.FailAttempt(ctx, a1, "transfer failed", now.Add(time.Hour), now); err != nil {
		t.Fatalf("FailAttempt a1: %v", err)
	}

	later := now.Add(time.Minute)
	a2, err := s.CreateAttempt(ctx, job.ID, "peer_two", 2.0, later)
	if err != nil {
		t.Fatalf("CreateAttempt a2: %v", err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, a2, "peer_two", "f2.flac", later.Add(time.Hour), later); err != nil {
		t.Fatalf("RecordEnqueueIntent a2/f2: %v", err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, a2, "peer_two", "f3.flac", later.Add(time.Hour), later); err != nil {
		t.Fatalf("RecordEnqueueIntent a2/f3: %v", err)
	}

	d, found, err := s.JobDetail(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobDetail: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if d.Job.Title != "Album" || d.Job.ArtistName != "Artist" {
		t.Errorf("Job = %+v, want title/artist Album/Artist", d.Job)
	}
	if len(d.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(d.Attempts))
	}
	if d.Attempts[0].Attempt.Username != "peer_two" {
		t.Errorf("expected newest attempt (peer_two) first, got %q", d.Attempts[0].Attempt.Username)
	}
	if len(d.Attempts[0].Transfers) != 2 {
		t.Errorf("expected 2 transfers for peer_two's attempt, got %d", len(d.Attempts[0].Transfers))
	}
	if d.Attempts[1].Attempt.Username != "peer_one" {
		t.Errorf("expected oldest attempt (peer_one) last, got %q", d.Attempts[1].Attempt.Username)
	}
	if d.Attempts[1].Attempt.FailReason != "transfer failed" {
		t.Errorf("FailReason = %q, want %q", d.Attempts[1].Attempt.FailReason, "transfer failed")
	}
	if len(d.Attempts[1].Transfers) != 1 {
		t.Errorf("expected 1 transfer for peer_one's attempt, got %d", len(d.Attempts[1].Transfers))
	}
}

func TestPeersReturnsGlobalAndArtistBreakdown(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if err := s.RecordAttemptOutcome(ctx, 1, "reliable_peer", true, now); err != nil {
		t.Fatalf("RecordAttemptOutcome artist 1: %v", err)
	}
	if err := s.RecordAttemptOutcome(ctx, 2, "reliable_peer", false, now.Add(time.Hour)); err != nil {
		t.Fatalf("RecordAttemptOutcome artist 2: %v", err)
	}
	if err := s.RecordAttemptOutcome(ctx, 0, "no_artist_peer", true, now); err != nil {
		t.Fatalf("RecordAttemptOutcome no artist: %v", err)
	}

	rows, err := s.Peers(ctx)
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	byUsername := map[string]core.PeerRow{}
	for _, r := range rows {
		byUsername[r.Username] = r
	}

	rp, ok := byUsername["reliable_peer"]
	if !ok {
		t.Fatalf("expected reliable_peer in result, got %+v", rows)
	}
	if rp.Global.SuccessCount != 1 || rp.Global.FailCount != 1 {
		t.Errorf("Global = %+v, want success=1 fail=1", rp.Global)
	}
	if len(rp.Artists) != 2 {
		t.Fatalf("expected 2 artist-specific rows, got %d", len(rp.Artists))
	}
	if rp.Artists[1].SuccessCount != 1 || rp.Artists[1].FailCount != 0 {
		t.Errorf("Artists[1] = %+v, want success=1 fail=0", rp.Artists[1])
	}
	if rp.Artists[2].SuccessCount != 0 || rp.Artists[2].FailCount != 1 {
		t.Errorf("Artists[2] = %+v, want success=0 fail=1", rp.Artists[2])
	}

	np, ok := byUsername["no_artist_peer"]
	if !ok {
		t.Fatalf("expected no_artist_peer in result, got %+v", rows)
	}
	if np.Global.SuccessCount != 1 {
		t.Errorf("no_artist_peer Global.SuccessCount = %d, want 1", np.Global.SuccessCount)
	}
	if len(np.Artists) != 0 {
		t.Errorf("expected no artist-specific rows for artistID<=0, got %+v", np.Artists)
	}
}
