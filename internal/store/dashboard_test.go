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

	job, err := s.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
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
		t.Errorf("expected nil Transfer for a job with no candidate, got %+v", v.Transfer)
	}
	if v.Peer != "" {
		t.Errorf("expected empty Peer, got %q", v.Peer)
	}
}

func TestListJobsWithTransferJoinsLatestTransfer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 2, now)
	_ = s.UpdateJobMetadata(ctx, job.ID, "Dummy", "Portishead", "", 0, now)

	// First (older) candidate/transfer.
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "peer_one", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates a1: %v", err)
	}
	a1, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate a1: found=%v (%v)", found, err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, a1.ID, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent a1: %v", err)
	}

	// Second (newer) candidate/transfer — this is the one ListJobsWithTransfer must surface.
	later := now.Add(time.Minute)
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "peer_two", Score: 2.0}}, later); err != nil {
		t.Fatalf("InsertCandidates a2: %v", err)
	}
	a2, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate a2: found=%v (%v)", found, err)
	}
	tid2, err := s.RecordEnqueueIntent(ctx, a2.ID, "peer_two", "f2.flac", later.Add(time.Hour), later)
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
		t.Errorf("Peer = %q, want peer_two (the newer candidate)", v.Peer)
	}
	if v.Transfer.State != core.TransferInProgress || v.Transfer.BytesDone != 512 {
		t.Errorf("Transfer = %+v, want state IN_PROGRESS bytesDone 512", v.Transfer)
	}
}

func TestListJobsWithTransferExcludesCancelled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 3, now)
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
// multi-track-album scenario: a single candidate gets one transfer row per
// file (see pipeline.Selecting, one RecordEnqueueIntent call per
// cand.Files entry). ListJobsWithTransfer must still return exactly one
// JobView per job, picking the most recently updated transfer.
func TestListJobsWithTransferDedupesMultiTransferAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 5, now)
	_ = s.UpdateJobMetadata(ctx, job.ID, "Multi Track Album", "Boards of Canada", "", 0, now)

	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "album_peer", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	attempt, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}

	// First file of the album.
	if _, err := s.RecordEnqueueIntent(ctx, attempt.ID, "album_peer", "track1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent track1: %v", err)
	}

	// Second file of the same album, same candidate, enqueued slightly later.
	later := now.Add(time.Minute)
	tid2, err := s.RecordEnqueueIntent(ctx, attempt.ID, "album_peer", "track2.flac", later.Add(time.Hour), later)
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

	job, _ := s.UpsertWantedJob(ctx, 6, now)
	_ = s.UpdateJobMetadata(ctx, job.ID, "Failed Album", "Some Artist", "", 0, now)

	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "flaky_peer", Score: 1.5}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	candidate, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	if err := s.FailCandidate(ctx, candidate.ID, "transfer failed", now); err != nil {
		t.Fatalf("FailCandidate: %v", err)
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
	if v.Attempt.State != core.CandidateFailed {
		t.Errorf("Attempt.State = %q, want FAILED", v.Attempt.State)
	}
}

func TestListJobsWithTransferNilAttemptWhenNoAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 7, now)
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

	job, _ := s.UpsertWantedJob(ctx, 4, now)
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "solo_peer", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	a1, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, a1.ID, "solo_peer", "solo.flac", now.Add(time.Hour), now); err != nil {
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
// dashboard's per-job detail panel: every candidate made for the job (not
// just the latest, unlike JobWithTransfer/ListJobsWithTransfer), newest
// first, each with its own per-file transfers.
func TestJobDetailIncludesAllAttemptsAndTransfersNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 10, now)
	_ = s.UpdateJobMetadata(ctx, job.ID, "Album", "Artist", "", 0, now)

	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "peer_one", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates a1: %v", err)
	}
	a1, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate a1: found=%v (%v)", found, err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, a1.ID, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent a1/f1: %v", err)
	}
	if err := s.FailCandidate(ctx, a1.ID, "transfer failed", now); err != nil {
		t.Fatalf("FailCandidate a1: %v", err)
	}

	later := now.Add(time.Minute)
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "peer_two", Score: 2.0}}, later); err != nil {
		t.Fatalf("InsertCandidates a2: %v", err)
	}
	a2, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate a2: found=%v (%v)", found, err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, a2.ID, "peer_two", "f2.flac", later.Add(time.Hour), later); err != nil {
		t.Fatalf("RecordEnqueueIntent a2/f2: %v", err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, a2.ID, "peer_two", "f3.flac", later.Add(time.Hour), later); err != nil {
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
		t.Errorf("expected newest candidate (peer_two) first, got %q", d.Attempts[0].Attempt.Username)
	}
	if len(d.Attempts[0].Transfers) != 2 {
		t.Errorf("expected 2 transfers for peer_two's candidate, got %d", len(d.Attempts[0].Transfers))
	}
	if d.Attempts[1].Attempt.Username != "peer_one" {
		t.Errorf("expected oldest candidate (peer_one) last, got %q", d.Attempts[1].Attempt.Username)
	}
	if d.Attempts[1].Attempt.FailReason != "transfer failed" {
		t.Errorf("FailReason = %q, want %q", d.Attempts[1].Attempt.FailReason, "transfer failed")
	}
	if len(d.Attempts[1].Transfers) != 1 {
		t.Errorf("expected 1 transfer for peer_one's candidate, got %d", len(d.Attempts[1].Transfers))
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

// TestRetryFailedJobRevivesFailedJob reproduces the dashboard's manual retry
// button: a FAILED job with retries/failed_at set and leftover
// candidates/transfers must come back to WANTED with a clean slate.
func TestRetryFailedJobRevivesFailedJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 20, now)
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "peer_one", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	if _, err := s.RecordEnqueueIntent(ctx, cand.ID, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := s.SetJobBackoff(ctx, job.ID, 3, now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetJobBackoff: %v", err)
	}
	if err := s.MarkJobFailed(ctx, job.ID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	later := now.Add(time.Minute)
	ok, err := s.RetryFailedJob(ctx, job.ID, later)
	if err != nil {
		t.Fatalf("RetryFailedJob: %v", err)
	}
	if !ok {
		t.Fatal("expected RetryFailedJob to return true for a FAILED job")
	}

	jobs, err := s.RunnableJobsInState(ctx, core.StateWanted, later.Add(time.Hour), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("RunnableJobsInState: %v %+v", err, jobs)
	}
	got := jobs[0]
	if got.State != core.StateWanted {
		t.Errorf("State = %q, want WANTED", got.State)
	}
	if got.Retries != 0 {
		t.Errorf("Retries = %d, want 0", got.Retries)
	}
	if got.NotBefore != nil {
		t.Errorf("NotBefore = %v, want nil", got.NotBefore)
	}
	if got.FailedAt != nil {
		t.Errorf("FailedAt = %v, want nil", got.FailedAt)
	}

	if _, found, err := s.NextNewCandidate(ctx, job.ID); err != nil || found {
		t.Fatalf("expected zero candidates after RetryFailedJob, found=%v (%v)", found, err)
	}
	if trs, err := s.TransfersForCandidate(ctx, cand.ID); err != nil || len(trs) != 0 {
		t.Fatalf("expected zero transfers after RetryFailedJob, got %d (%v)", len(trs), err)
	}
}

// TestRetryFailedJobNoopWhenNotFailed guards the race the dashboard button
// can lose: if a module moved the job out of FAILED between the dashboard
// fetching its state and the retry click landing, RetryFailedJob must not
// touch it.
func TestRetryFailedJobNoopWhenNotFailed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 21, now)
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "peer_one", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}

	ok, err := s.RetryFailedJob(ctx, job.ID, now)
	if err != nil {
		t.Fatalf("RetryFailedJob: %v", err)
	}
	if ok {
		t.Fatal("expected RetryFailedJob to return false for a non-FAILED job")
	}

	if _, found, err := s.NextNewCandidate(ctx, job.ID); err != nil || !found {
		t.Fatalf("expected candidate to survive the no-op retry, found=%v (%v)", found, err)
	}
}

func TestRetryFailedJobUnknownID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ok, err := s.RetryFailedJob(ctx, 99999, time.Now())
	if err != nil {
		t.Fatalf("RetryFailedJob: %v", err)
	}
	if ok {
		t.Fatal("expected RetryFailedJob to return false for an unknown job id")
	}
}
