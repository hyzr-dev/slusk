package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

func insertDashboardTestJob(t *testing.T, s *Store, lidarrID int64, source core.JobSource, state core.AlbumJobState, transferState core.TransferState, title, artist, peer string, retries int, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRowContext(context.Background(),
		`INSERT INTO album_jobs (lidarr_album_id, source, state, retries, created_at, updated_at, title, artist_name)
		 VALUES ($1, $2, $3, $4, $5, $5, $6, $7) RETURNING id`,
		lidarrID, string(source), string(state), retries, at, title, artist).Scan(&id); err != nil {
		t.Fatalf("insert dashboard job: %v", err)
	}
	if peer == "" && transferState == "" {
		return id
	}
	var candidateID int64
	if err := s.db.QueryRowContext(context.Background(),
		`INSERT INTO candidates (album_job_id, username, score, files, state, created_at, updated_at)
		 VALUES ($1, $2, 1, '[]', 'ACTIVE', $3, $3) RETURNING id`,
		id, peer, at).Scan(&candidateID); err != nil {
		t.Fatalf("insert dashboard candidate: %v", err)
	}
	if transferState != "" {
		if _, err := s.db.ExecContext(context.Background(),
			`INSERT INTO transfers (candidate_id, username, filename, state, deadline, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			candidateID, peer, fmt.Sprintf("%d.flac", id), string(transferState), at.Add(time.Hour), at); err != nil {
			t.Fatalf("insert dashboard transfer: %v", err)
		}
	}
	return id
}

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
	if v.Peer != "" {
		t.Errorf("expected empty Peer, got %q", v.Peer)
	}
}

// TestListJobsWithTransferJoinsLatestTransfer covers two candidates from two
// separate search passes (distinct created_at, unlike the same-batch
// scenario TestListJobsWithTransferPrefersActiveOverNewerNeverTried
// reproduces): both stay NEW here, so currentCandidateOrder's tiebreak
// (updated_at DESC, id DESC) picks the more recently touched one — peer_two's
// candidate, whose own transfer aggregate must be what AlbumBytesDone/Total
// and Peer reflect.
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
	if _, _, err := s.RecordEnqueueIntent(ctx, a1.ID, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
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
	tid2, _, err := s.RecordEnqueueIntent(ctx, a2.ID, "peer_two", "f2.flac", later.Add(time.Hour), later)
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
	if v.Peer != "peer_two" {
		t.Errorf("Peer = %q, want peer_two (the newer candidate)", v.Peer)
	}
	if v.AlbumBytesDone != 512 || v.AlbumBytesTotal != 1024 {
		t.Errorf("AlbumBytes = %d/%d, want 512/1024 (peer_two's own transfer)", v.AlbumBytesDone, v.AlbumBytesTotal)
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
// JobView per job — its AlbumBytes* aggregate across every file of the
// candidate (issue #269), not just whichever transfer happens to be most
// recently updated.
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
	if _, _, err := s.RecordEnqueueIntent(ctx, attempt.ID, "album_peer", "track1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent track1: %v", err)
	}

	// Second file of the same album, same candidate, enqueued slightly later.
	later := now.Add(time.Minute)
	tid2, _, err := s.RecordEnqueueIntent(ctx, attempt.ID, "album_peer", "track2.flac", later.Add(time.Hour), later)
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
	// track1 was never given a size (RecordEnqueueIntent alone leaves
	// bytes_total 0), so the album total is track2's alone.
	if v.AlbumBytesDone != 128 || v.AlbumBytesTotal != 1024 {
		t.Errorf("AlbumBytes = %d/%d, want 128/1024 (track2, the only file with progress)", v.AlbumBytesDone, v.AlbumBytesTotal)
	}
	if v.Peer != "album_peer" {
		t.Errorf("Peer = %q, want album_peer", v.Peer)
	}
}

// TestListJobsWithTransferAggregatesAlbumBytes reproduces the multi-track
// album progress-bar bug (issue #174): AlbumBytesDone/Total must be the SUM
// across every transfer of the candidate, not just one arbitrarily-chosen
// transfer's numbers.
func TestListJobsWithTransferAggregatesAlbumBytes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 50, now)
	_ = s.UpdateJobMetadata(ctx, job.ID, "Album Bytes", "Aphex Twin", "", 0, now)

	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "album_peer", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	attempt, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}

	tid1, _, err := s.RecordEnqueueIntent(ctx, attempt.ID, "album_peer", "track1.flac", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent track1: %v", err)
	}
	if err := s.UpdateTransferProgress(ctx, tid1, core.TransferInProgress, 1000, 2000, now); err != nil {
		t.Fatalf("UpdateTransferProgress track1: %v", err)
	}

	later := now.Add(time.Minute)
	tid2, _, err := s.RecordEnqueueIntent(ctx, attempt.ID, "album_peer", "track2.flac", later.Add(time.Hour), later)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent track2: %v", err)
	}
	if err := s.UpdateTransferProgress(ctx, tid2, core.TransferInProgress, 300, 3000, later); err != nil {
		t.Fatalf("UpdateTransferProgress track2: %v", err)
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	v := views[0]

	// Sums across both transfers, not just the latest (track2).
	if v.AlbumBytesDone != 1300 {
		t.Errorf("AlbumBytesDone = %d, want 1300 (1000+300)", v.AlbumBytesDone)
	}
	if v.AlbumBytesTotal != 5000 {
		t.Errorf("AlbumBytesTotal = %d, want 5000 (2000+3000)", v.AlbumBytesTotal)
	}
	if v.AlbumBytesRemaining != 3700 {
		t.Errorf("AlbumBytesRemaining = %d, want 3700 ((2000-1000)+(3000-300))", v.AlbumBytesRemaining)
	}
	if v.Peer != "album_peer" {
		t.Errorf("Peer = %q, want album_peer", v.Peer)
	}
}

// TestListJobsWithTransferAlbumBytesZeroWithoutAttempt covers the a.id IS
// NULL case: the lateral aggregate must yield zeros (via COALESCE), not NULL
// scan errors, for a job with no candidate yet.
func TestListJobsWithTransferAlbumBytesZeroWithoutAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 51, now)

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	v := views[0]
	if v.Job.ID != job.ID {
		t.Fatalf("unexpected job in result: %+v", v.Job)
	}
	if v.Attempt != nil {
		t.Errorf("expected nil Attempt, got %+v", v.Attempt)
	}
	if v.AlbumBytesDone != 0 || v.AlbumBytesTotal != 0 || v.AlbumBytesRemaining != 0 {
		t.Errorf("AlbumBytes* = %d/%d/%d, want all zero for a job with no candidate", v.AlbumBytesDone, v.AlbumBytesTotal, v.AlbumBytesRemaining)
	}
}

// TestListJobsWithTransferAlbumBytesRemainingExcludesTerminal covers the
// FILTER clause: a terminal (ERRORED) transfer with leftover bytes must
// still count toward AlbumBytesDone/Total (it happened) but must be excluded
// from AlbumBytesRemaining (it will never resume).
func TestListJobsWithTransferAlbumBytesRemainingExcludesTerminal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 52, now)
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "flaky_peer", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	attempt, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}

	// A live transfer still in progress.
	tid1, _, err := s.RecordEnqueueIntent(ctx, attempt.ID, "flaky_peer", "track1.flac", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent track1: %v", err)
	}
	if err := s.UpdateTransferProgress(ctx, tid1, core.TransferInProgress, 100, 1000, now); err != nil {
		t.Fatalf("UpdateTransferProgress track1: %v", err)
	}

	// A terminal (errored) transfer with bytes left undone.
	later := now.Add(time.Minute)
	tid2, _, err := s.RecordEnqueueIntent(ctx, attempt.ID, "flaky_peer", "track2.flac", later.Add(time.Hour), later)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent track2: %v", err)
	}
	if err := s.UpdateTransferProgress(ctx, tid2, core.TransferErrored, 200, 2000, later); err != nil {
		t.Fatalf("UpdateTransferProgress track2: %v", err)
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	v := views[0]

	if v.AlbumBytesDone != 300 {
		t.Errorf("AlbumBytesDone = %d, want 300 (100+200, errored transfer's bytes still counted)", v.AlbumBytesDone)
	}
	if v.AlbumBytesTotal != 3000 {
		t.Errorf("AlbumBytesTotal = %d, want 3000 (1000+2000, errored transfer's total still counted)", v.AlbumBytesTotal)
	}
	// Only track1's leftover (1000-100=900) counts; track2 is terminal so its
	// leftover (2000-200=1800) is excluded.
	if v.AlbumBytesRemaining != 900 {
		t.Errorf("AlbumBytesRemaining = %d, want 900 (only the non-terminal track1's leftover)", v.AlbumBytesRemaining)
	}
}

// TestListJobsWithTransferAlbumBytesRemainingNeverNegative covers the
// GREATEST(..., 0) guard: a transfer whose bytes_done overshoots bytes_total
// (a progress report past the announced size) must not push
// AlbumBytesRemaining negative.
func TestListJobsWithTransferAlbumBytesRemainingNeverNegative(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 53, now)
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "overshoot_peer", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	attempt, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}

	tid, _, err := s.RecordEnqueueIntent(ctx, attempt.ID, "overshoot_peer", "track1.flac", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := s.UpdateTransferProgress(ctx, tid, core.TransferInProgress, 1500, 1000, now); err != nil {
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
	if v.AlbumBytesRemaining != 0 {
		t.Errorf("AlbumBytesRemaining = %d, want 0 (bytes_done > bytes_total must not go negative)", v.AlbumBytesRemaining)
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
	if _, _, err := s.RecordEnqueueIntent(ctx, a1.ID, "solo_peer", "solo.flac", now.Add(time.Hour), now); err != nil {
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
	if _, _, err := s.RecordEnqueueIntent(ctx, a1.ID, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
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
	if _, _, err := s.RecordEnqueueIntent(ctx, a2.ID, "peer_two", "f2.flac", later.Add(time.Hour), later); err != nil {
		t.Fatalf("RecordEnqueueIntent a2/f2: %v", err)
	}
	if _, _, err := s.RecordEnqueueIntent(ctx, a2.ID, "peer_two", "f3.flac", later.Add(time.Hour), later); err != nil {
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

// TestTransferBytesByCandidateReturnsPerFileBytes covers the per-file byte
// query backing internal/observ's live-bytes overlay (issue #161): given a
// set of candidate ids, it returns each candidate's own files keyed by
// filename, and does not leak another candidate's files into the result.
func TestTransferBytesByCandidateReturnsPerFileBytes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	jobA, _ := s.UpsertWantedJob(ctx, 61, now)
	if err := s.InsertCandidates(ctx, jobA.ID, []NewCandidate{{Username: "peer_a", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates a: %v", err)
	}
	a, found, err := s.NextNewCandidate(ctx, jobA.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate a: found=%v (%v)", found, err)
	}
	tid1, _, err := s.RecordEnqueueIntent(ctx, a.ID, "peer_a", "01.flac", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent a/01: %v", err)
	}
	if err := s.UpdateTransferProgress(ctx, tid1, core.TransferInProgress, 400, 1000, now); err != nil {
		t.Fatalf("UpdateTransferProgress a/01: %v", err)
	}
	tid2, _, err := s.RecordEnqueueIntent(ctx, a.ID, "peer_a", "02.flac", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent a/02: %v", err)
	}
	if err := s.UpdateTransferProgress(ctx, tid2, core.TransferCompleted, 2000, 2000, now); err != nil {
		t.Fatalf("UpdateTransferProgress a/02: %v", err)
	}

	jobB, _ := s.UpsertWantedJob(ctx, 62, now)
	if err := s.InsertCandidates(ctx, jobB.ID, []NewCandidate{{Username: "peer_b", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates b: %v", err)
	}
	b, found, err := s.NextNewCandidate(ctx, jobB.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate b: found=%v (%v)", found, err)
	}
	// Same filename as candidate a's first file, to prove results are keyed
	// per-candidate rather than colliding on filename alone.
	if _, _, err := s.RecordEnqueueIntent(ctx, b.ID, "peer_b", "01.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent b/01: %v", err)
	}

	got, err := s.TransferBytesByCandidate(ctx, []int64{a.ID, b.ID})
	if err != nil {
		t.Fatalf("TransferBytesByCandidate: %v", err)
	}
	if got[a.ID]["01.flac"] != 400 || got[a.ID]["02.flac"] != 2000 {
		t.Errorf("candidate a bytes = %+v, want 01.flac=400, 02.flac=2000", got[a.ID])
	}
	if got[b.ID]["01.flac"] != 0 {
		t.Errorf("candidate b bytes = %+v, want 01.flac=0 (never enqueued progress)", got[b.ID])
	}
}

// TestTransferBytesByCandidateEmptyAndUnknownIDs covers the two degenerate
// inputs: no ids at all, and ids that don't match any candidate.
func TestTransferBytesByCandidateEmptyAndUnknownIDs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	got, err := s.TransferBytesByCandidate(ctx, nil)
	if err != nil {
		t.Fatalf("TransferBytesByCandidate(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map for nil ids, got %+v", got)
	}

	got, err = s.TransferBytesByCandidate(ctx, []int64{999999})
	if err != nil {
		t.Fatalf("TransferBytesByCandidate(unknown): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map for unknown candidate id, got %+v", got)
	}
}

func TestPeersReturnsGlobalCountersOnly(t *testing.T) {
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

	page, err := s.Peers(ctx, PeersQuery{Sort: "username", Dir: "asc"})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	rows := page.Peers
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

	np, ok := byUsername["no_artist_peer"]
	if !ok {
		t.Fatalf("expected no_artist_peer in result, got %+v", rows)
	}
	if np.Global.SuccessCount != 1 {
		t.Errorf("no_artist_peer Global.SuccessCount = %d, want 1", np.Global.SuccessCount)
	}
}

func TestPeerHistoryReturnsArtistRowsWithNames(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if err := s.RecordAttemptOutcome(ctx, 1, "reliable_peer", true, now); err != nil {
		t.Fatalf("RecordAttemptOutcome artist 1: %v", err)
	}
	if err := s.RecordAttemptOutcome(ctx, 2, "reliable_peer", false, now.Add(time.Hour)); err != nil {
		t.Fatalf("RecordAttemptOutcome artist 2: %v", err)
	}
	if err := s.RecordAttemptOutcome(ctx, 3, "reliable_peer", true, now); err != nil {
		t.Fatalf("RecordAttemptOutcome artist 3: %v", err)
	}
	if err := s.RecordAttemptOutcome(ctx, 0, "no_artist_peer", true, now); err != nil {
		t.Fatalf("RecordAttemptOutcome no artist: %v", err)
	}

	// Artist 1 has a name. Artist 2 has only the empty-string DEFAULT, which
	// means "no name known" and must not be served as a nameless artist.
	// Artist 3 has no album_jobs row at all — the "every job deleted" case.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO album_jobs (lidarr_album_id, state, created_at, updated_at, title, artist_name, artist_id)
		 VALUES (901, 'WANTED', $1, $1, 'Album A', 'Named Artist', 1),
		        (902, 'WANTED', $1, $1, 'Album B', '', 2)`, now); err != nil {
		t.Fatalf("seed album_jobs: %v", err)
	}

	history, found, err := s.PeerHistory(ctx, "reliable_peer")
	if err != nil {
		t.Fatalf("PeerHistory: %v", err)
	}
	if !found {
		t.Fatal("PeerHistory reported reliable_peer unknown")
	}
	if history.Global.SuccessCount != 2 || history.Global.FailCount != 1 {
		t.Errorf("Global = %+v, want success=2 fail=1", history.Global)
	}
	if len(history.Artists) != 3 {
		t.Fatalf("expected 3 artist rows, got %+v", history.Artists)
	}
	if got := history.Artists[0]; got.ArtistID != 1 || got.Name != "Named Artist" || got.Counters.SuccessCount != 1 || got.Counters.FailCount != 0 {
		t.Errorf("Artists[0] = %+v, want artist 1 'Named Artist' success=1 fail=0", got)
	}
	if got := history.Artists[1]; got.ArtistID != 2 || got.Name != "" || got.Counters.FailCount != 1 {
		t.Errorf("Artists[1] = %+v, want artist 2 with no name and fail=1", got)
	}
	if got := history.Artists[2]; got.ArtistID != 3 || got.Name != "" {
		t.Errorf("Artists[2] = %+v, want artist 3 with no name", got)
	}

	// A peer whose only outcome was recorded with artistID <= 0 exists but has
	// no artist-specific rows — a different answer from "no such peer".
	empty, found, err := s.PeerHistory(ctx, "no_artist_peer")
	if err != nil {
		t.Fatalf("PeerHistory(no_artist_peer): %v", err)
	}
	if !found {
		t.Fatal("PeerHistory reported no_artist_peer unknown")
	}
	if len(empty.Artists) != 0 {
		t.Errorf("expected no artist rows for artistID<=0, got %+v", empty.Artists)
	}
}

func TestPeerHistoryReportsUnknownPeer(t *testing.T) {
	s := newTestStore(t)

	if _, found, err := s.PeerHistory(context.Background(), "never_seen"); err != nil {
		t.Fatalf("PeerHistory: %v", err)
	} else if found {
		t.Error("PeerHistory reported an unknown peer as found")
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
	if _, _, err := s.RecordEnqueueIntent(ctx, cand.ID, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := s.SetJobBackoff(ctx, job.ID, 3, now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetJobBackoff: %v", err)
	}
	if err := s.MarkJobFailed(ctx, job.ID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}
	// Seed an empty-search streak on the now-FAILED job (issue #334): a
	// manual retry must wipe it, or a job revived by the dashboard's Retry
	// button would carry a stale streak straight into the token-drop
	// rewrite instead of starting clean (see rewrite.go). SetJobBackoff
	// above already zeroes empty_searches, so this has to happen after it
	// to actually exercise RetryFailedJob's own reset.
	if err := s.SetJobEmptySearchBackoff(ctx, job.ID, 7, now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetJobEmptySearchBackoff: %v", err)
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
	if got.EmptySearches != 0 {
		t.Errorf("EmptySearches = %d, want 0", got.EmptySearches)
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

// TestRetryFailedJobRevivesParkedSpellings verifies that both the canonical
// PARKED state and its legacy ORPHANED spelling remain manually retryable and
// receive the same clean-slate reset.
func TestRetryFailedJobRevivesParkedSpellings(t *testing.T) {
	for i, state := range []core.AlbumJobState{core.StateParked, core.StateOrphaned} {
		t.Run(string(state), func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

			job, _ := s.UpsertWantedJob(ctx, int64(22+i), now)
			if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "peer_one", Score: 1.0}}, now); err != nil {
				t.Fatalf("InsertCandidates: %v", err)
			}
			cand, found, err := s.NextNewCandidate(ctx, job.ID)
			if err != nil || !found {
				t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
			}
			if _, _, err := s.RecordEnqueueIntent(ctx, cand.ID, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
				t.Fatalf("RecordEnqueueIntent: %v", err)
			}
			if _, err := s.db.ExecContext(ctx,
				`UPDATE album_jobs SET retries=3, not_before=$1, failed_at=$2 WHERE id=$3`,
				now.Add(time.Hour), now, job.ID); err != nil {
				t.Fatalf("seed retry metadata: %v", err)
			}
			if err := s.AdvanceJobState(ctx, job.ID, state, now); err != nil {
				t.Fatalf("AdvanceJobState: %v", err)
			}

			later := now.Add(time.Minute)
			ok, err := s.RetryFailedJob(ctx, job.ID, later)
			if err != nil {
				t.Fatalf("RetryFailedJob: %v", err)
			}
			if !ok {
				t.Fatalf("expected RetryFailedJob to return true for a %s job", state)
			}

			view, found, err := s.JobWithTransfer(ctx, job.ID)
			if err != nil || !found {
				t.Fatalf("JobWithTransfer: found=%v err=%v", found, err)
			}
			if got := view.Job; got.State != core.StateWanted || got.Retries != 0 || got.NotBefore != nil || got.FailedAt != nil {
				t.Errorf("job after retry = state %q retries %d not_before %v failed_at %v", got.State, got.Retries, got.NotBefore, got.FailedAt)
			}
			if _, found, err := s.NextNewCandidate(ctx, job.ID); err != nil || found {
				t.Fatalf("expected zero candidates after RetryFailedJob, found=%v (%v)", found, err)
			}
			if trs, err := s.TransfersForCandidate(ctx, cand.ID); err != nil || len(trs) != 0 {
				t.Fatalf("expected zero transfers after RetryFailedJob, got %d (%v)", len(trs), err)
			}
		})
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

// TestRetryManualJobRevivesCandidateToNew covers issue #347: unlike
// RetryFailedJob, a manual job's retry must try the same peer again rather
// than re-search, so its candidate is revived to NEW (not deleted) while its
// stale FAILED/CANCELLED transfers are cleared, and the job lands in
// SELECTING (not WANTED) so Selecting picks it straight back up.
func TestRetryManualJobRevivesCandidateToNew(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.CreateManualJob(ctx, "Album", "Artist", "peer_one", "",
		[]ManualJobFile{{Filename: "f1.flac", Size: 10}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	cand, found, err := s.ActiveCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	if _, err := s.FailCandidateAndAdvance(ctx, cand.ID, job.ID, "transfer failed", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("FailCandidateAndAdvance: %v", err)
	}
	if err := s.MarkJobFailed(ctx, job.ID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}
	// Seed an empty-search streak (issue #334): RetryManualJob's clean-slate
	// reset must wipe it too.
	if err := s.SetJobEmptySearchBackoff(ctx, job.ID, 5, now, now); err != nil {
		t.Fatalf("SetJobEmptySearchBackoff: %v", err)
	}

	later := now.Add(time.Minute)
	ok, err := s.RetryManualJob(ctx, job.ID, later)
	if err != nil {
		t.Fatalf("RetryManualJob: %v", err)
	}
	if !ok {
		t.Fatal("expected RetryManualJob to return true for a FAILED manual job")
	}

	view, found, err := s.JobWithTransfer(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v err=%v", found, err)
	}
	if got := view.Job; got.State != core.StateSelecting || got.Retries != 0 || got.NotBefore != nil || got.FailedAt != nil {
		t.Errorf("job after retry = state %q retries %d not_before %v failed_at %v", got.State, got.Retries, got.NotBefore, got.FailedAt)
	}
	if got := view.Job; got.EmptySearches != 0 {
		t.Errorf("EmptySearches = %d, want 0", got.EmptySearches)
	}

	cands, err := s.CandidatesForJob(ctx, job.ID)
	if err != nil || len(cands) != 1 {
		t.Fatalf("CandidatesForJob = %d (%v), want the original candidate revived, not deleted", len(cands), err)
	}
	if cands[0].ID != cand.ID {
		t.Errorf("candidate id = %d, want the same original candidate %d", cands[0].ID, cand.ID)
	}
	if cands[0].State != core.CandidateNew {
		t.Errorf("candidate state = %q, want NEW", cands[0].State)
	}
	if cands[0].Username != "peer_one" {
		t.Errorf("candidate username = %q, want peer_one (the user's original choice)", cands[0].Username)
	}

	if trs, err := s.TransfersForCandidate(ctx, cand.ID); err != nil || len(trs) != 0 {
		t.Fatalf("expected the stale transfer set cleared, got %d (%v)", len(trs), err)
	}
}

// TestRetryManualJobClearsFailReasonAndImportSubmittedAt covers issue #347:
// RetryFailedJob gets a clean-slate candidate for free by deleting the row,
// but RetryManualJob revives the same row, so it must explicitly clear
// fail_reason (otherwise the dashboard shows the previous attempt's failure
// while the retry is still in flight) and import_submitted_at (otherwise a
// candidate that reaches IMPORTING again skips verify straight to confirm,
// whose timeout is measured from the stale value and has already expired).
func TestRetryManualJobClearsFailReasonAndImportSubmittedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.CreateManualJob(ctx, "Album", "Artist", "peer_one", "",
		[]ManualJobFile{{Filename: "f1.flac", Size: 10}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	cand, found, err := s.ActiveCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	if err := s.MarkImportSubmitted(ctx, cand.ID, now); err != nil {
		t.Fatalf("MarkImportSubmitted: %v", err)
	}
	if _, err := s.FailCandidateAndAdvance(ctx, cand.ID, job.ID, "import not confirmed", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("FailCandidateAndAdvance: %v", err)
	}
	if err := s.MarkJobFailed(ctx, job.ID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	cands, err := s.CandidatesForJob(ctx, job.ID)
	if err != nil || len(cands) != 1 {
		t.Fatalf("CandidatesForJob = %d (%v)", len(cands), err)
	}
	if cands[0].FailReason == "" || cands[0].ImportSubmittedAt == nil {
		t.Fatalf("test setup: expected fail_reason and import_submitted_at both set before retry, got %+v", cands[0])
	}

	later := now.Add(time.Minute)
	ok, err := s.RetryManualJob(ctx, job.ID, later)
	if err != nil {
		t.Fatalf("RetryManualJob: %v", err)
	}
	if !ok {
		t.Fatal("expected RetryManualJob to return true for a FAILED manual job")
	}

	cands, err = s.CandidatesForJob(ctx, job.ID)
	if err != nil || len(cands) != 1 {
		t.Fatalf("CandidatesForJob = %d (%v)", len(cands), err)
	}
	if cands[0].FailReason != "" {
		t.Errorf("fail_reason = %q, want cleared", cands[0].FailReason)
	}
	if cands[0].ImportSubmittedAt != nil {
		t.Errorf("import_submitted_at = %v, want cleared", cands[0].ImportSubmittedAt)
	}
}

// TestRetryManualJobRevivesParkedManualJob covers the other live path in
// RetryManualJob's allowlist: a manual job is created straight into
// DOWNLOADING, so ParkJobForCandidate (not a FAILED transition) is how it
// most commonly reaches a retryable state - unlike RetryFailedJob's PARKED
// case, this is not a legacy spelling, it is the routine one.
func TestRetryManualJobRevivesParkedManualJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.CreateManualJob(ctx, "Album", "Artist", "peer_one", "",
		[]ManualJobFile{{Filename: "f1.flac", Size: 10}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	cand, found, err := s.ActiveCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	transfers, err := s.TransfersForCandidate(ctx, cand.ID)
	if err != nil || len(transfers) != 1 {
		t.Fatalf("TransfersForCandidate = %d (%v)", len(transfers), err)
	}

	parked, err := s.ParkJobForCandidate(ctx, transfers[0].ID, cand.ID, core.TransferErrored, 5, 10, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ParkJobForCandidate: %v", err)
	}
	if !parked {
		t.Fatal("expected ParkJobForCandidate to return true for a DOWNLOADING job")
	}
	view, found, err := s.JobWithTransfer(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
	}
	if view.Job.State != core.StateParked {
		t.Fatalf("test setup: job state = %q, want PARKED", view.Job.State)
	}

	later := now.Add(2 * time.Minute)
	ok, err := s.RetryManualJob(ctx, job.ID, later)
	if err != nil {
		t.Fatalf("RetryManualJob: %v", err)
	}
	if !ok {
		t.Fatal("expected RetryManualJob to return true for a PARKED manual job")
	}

	view, found, err = s.JobWithTransfer(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
	}
	if view.Job.State != core.StateSelecting {
		t.Errorf("job state = %q, want SELECTING", view.Job.State)
	}
	cands, err := s.CandidatesForJob(ctx, job.ID)
	if err != nil || len(cands) != 1 {
		t.Fatalf("CandidatesForJob = %d (%v)", len(cands), err)
	}
	if cands[0].State != core.CandidateNew {
		t.Errorf("candidate state = %q, want NEW", cands[0].State)
	}
}

// TestRetryManualJobNoopWithoutCandidates covers issue #347's zero-candidate
// case, which is a real production population rather than a hypothetical one:
// RetryFailedJob and ForceSearchJob both DELETE candidates, so every manual
// job that went through either before #347 shipped now sits FAILED with none.
// Reviving such a job to SELECTING would only have Selecting fail it again on
// the next tick, so the whole transaction rolls back and the caller is told
// not-retryable instead - the peer the user chose is gone for good.
func TestRetryManualJobNoopWithoutCandidates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.CreateManualJob(ctx, "Album", "Artist", "bob", "",
		[]ManualJobFile{{Filename: "track.flac", Size: 10}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	if err := s.MarkJobFailed(ctx, job.ID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}
	// Reproduce what a pre-#347 retry did to this job: candidates (and their
	// transfers) deleted, job left behind. Deleting through the store's own
	// RetryFailedJob would also move the job to WANTED, so do it directly to
	// isolate the condition under test.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM transfers WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id = $1)`, job.ID); err != nil {
		t.Fatalf("delete transfers: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM candidates WHERE album_job_id = $1`, job.ID); err != nil {
		t.Fatalf("delete candidates: %v", err)
	}

	ok, err := s.RetryManualJob(ctx, job.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("RetryManualJob: %v", err)
	}
	if ok {
		t.Error("RetryManualJob = true, want false for a job with no candidate to revive")
	}

	// The album_jobs UPDATE ran before the candidate check, so this asserts the
	// rollback actually took: a committed transaction would leave SELECTING.
	view, found, err := s.JobWithTransfer(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
	}
	if view.Job.State != core.StateFailed {
		t.Errorf("job state = %q, want still FAILED (transaction rolled back)", view.Job.State)
	}
}

// TestRetryManualJobNoopForLidarrJob guards the routing: RetryManualJob must
// never touch a lidarr-sourced job even if it happens to be FAILED, or it
// would silently skip the re-search a lidarr job actually needs.
func TestRetryManualJobNoopForLidarrJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 50, now)
	if err := s.MarkJobFailed(ctx, job.ID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	ok, err := s.RetryManualJob(ctx, job.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("RetryManualJob: %v", err)
	}
	if ok {
		t.Fatal("expected RetryManualJob to return false for a lidarr-sourced job")
	}
}

// TestRetryManualJobNoopWhenNotRetryable mirrors
// TestRetryFailedJobNoopWhenNotFailed for the manual-job path: a job not in
// FAILED/PARKED/ORPHANED must be left untouched.
func TestRetryManualJobNoopWhenNotRetryable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.CreateManualJob(ctx, "Album", "Artist", "peer_one", "",
		[]ManualJobFile{{Filename: "f1.flac", Size: 10}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}

	ok, err := s.RetryManualJob(ctx, job.ID, now)
	if err != nil {
		t.Fatalf("RetryManualJob: %v", err)
	}
	if ok {
		t.Fatal("expected RetryManualJob to return false for a still-DOWNLOADING job")
	}
}

// TestRetryJobsNoopForNotImportedJob covers issue #59: NOT_IMPORTED is a
// terminal, non-failure outcome (the download succeeded, there was simply no
// Lidarr album to import into), so neither retry path may revive it - the
// download is not a failure to retry. Both RetryFailedJob (the generic path)
// and RetryManualJob (the manual-specific one) must independently refuse it,
// since app.Jobs.Retry's ErrJobNotRetryable relies on the store-side WHERE
// clause, not on any state check of its own.
func TestRetryJobsNoopForNotImportedJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.CreateManualJob(ctx, "Album", "Artist", "peer_one", "",
		[]ManualJobFile{{Filename: "f1.flac", Size: 10}}, now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	if ok, err := s.AdvanceJobStateFrom(ctx, job.ID, core.StateDownloading, core.StateNotImported, now); err != nil || !ok {
		t.Fatalf("AdvanceJobStateFrom: ok=%v err=%v", ok, err)
	}

	if ok, err := s.RetryManualJob(ctx, job.ID, now); err != nil || ok {
		t.Fatalf("RetryManualJob: ok=%v err=%v, want ok=false", ok, err)
	}
	if ok, err := s.RetryFailedJob(ctx, job.ID, now); err != nil || ok {
		t.Fatalf("RetryFailedJob: ok=%v err=%v, want ok=false", ok, err)
	}
	view, found, err := s.JobWithTransfer(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: %v found=%v", err, found)
	}
	if view.Job.State != core.StateNotImported {
		t.Errorf("job state after no-op retries = %v, want still NOT_IMPORTED", view.Job.State)
	}
}

// TestForceSearchJobResetsAndReturnsToWanted covers issue #159's force-search
// button: a FAILED job with backoff, failed_at, and stale candidates/transfers
// is reset to a clean WANTED slate.
func TestForceSearchJobResetsAndReturnsToWanted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 30, now)
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "peer_one", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	if _, _, err := s.RecordEnqueueIntent(ctx, cand.ID, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := s.SetJobBackoff(ctx, job.ID, 3, now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetJobBackoff: %v", err)
	}
	if err := s.MarkJobFailed(ctx, job.ID, now); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}
	// Seed an empty-search streak on the now-FAILED job (issue #334):
	// force-search must wipe it too, same reasoning as
	// TestRetryFailedJobRevivesFailedJob. SetJobBackoff above already
	// zeroes empty_searches, so this has to happen after it.
	if err := s.SetJobEmptySearchBackoff(ctx, job.ID, 9, now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetJobEmptySearchBackoff: %v", err)
	}

	later := now.Add(time.Minute)
	ok, err := s.ForceSearchJob(ctx, job.ID, later)
	if err != nil {
		t.Fatalf("ForceSearchJob: %v", err)
	}
	if !ok {
		t.Fatal("expected ForceSearchJob to return true for a FAILED job")
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
	if got.EmptySearches != 0 {
		t.Errorf("EmptySearches = %d, want 0", got.EmptySearches)
	}
	if got.NotBefore != nil {
		t.Errorf("NotBefore = %v, want nil", got.NotBefore)
	}
	if got.FailedAt != nil {
		t.Errorf("FailedAt = %v, want nil", got.FailedAt)
	}

	if _, found, err := s.NextNewCandidate(ctx, job.ID); err != nil || found {
		t.Fatalf("expected zero candidates after ForceSearchJob, found=%v (%v)", found, err)
	}
	if trs, err := s.TransfersForCandidate(ctx, cand.ID); err != nil || len(trs) != 0 {
		t.Fatalf("expected zero transfers after ForceSearchJob, got %d (%v)", len(trs), err)
	}
}

// TestForceSearchJobRefusedWhileDownloading guards the active-transfer race:
// a DOWNLOADING job must not be force-searched out from under a live transfer.
func TestForceSearchJobRefusedWhileDownloading(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 31, now)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState to SELECTING: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobState to DOWNLOADING: %v", err)
	}

	ok, err := s.ForceSearchJob(ctx, job.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ForceSearchJob: %v", err)
	}
	if ok {
		t.Fatal("expected ForceSearchJob to return false for a DOWNLOADING job")
	}
}

func TestForceSearchJobUnknownID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ok, err := s.ForceSearchJob(ctx, 99999, time.Now())
	if err != nil {
		t.Fatalf("ForceSearchJob: %v", err)
	}
	if ok {
		t.Fatal("expected ForceSearchJob to return false for an unknown job id")
	}
}

func TestParkedSpellingsSupportForceSearchAndDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	for i, state := range []core.AlbumJobState{core.StateParked, core.StateOrphaned} {
		searchJob, _ := s.UpsertWantedJob(ctx, int64(40+i*2), now)
		if err := s.AdvanceJobState(ctx, searchJob.ID, state, now); err != nil {
			t.Fatalf("AdvanceJobState(%s): %v", state, err)
		}
		if ok, err := s.ForceSearchJob(ctx, searchJob.ID, now.Add(time.Minute)); err != nil || !ok {
			t.Fatalf("ForceSearchJob(%s): ok=%v err=%v", state, ok, err)
		}
		if got := jobStateForStore(t, s, searchJob.ID); got != core.StateWanted {
			t.Errorf("ForceSearchJob(%s) state = %s, want WANTED", state, got)
		}

		deleteJob, _ := s.UpsertWantedJob(ctx, int64(41+i*2), now)
		if err := s.AdvanceJobState(ctx, deleteJob.ID, state, now); err != nil {
			t.Fatalf("AdvanceJobState(%s): %v", state, err)
		}
		if ok, err := s.DeleteJob(ctx, deleteJob.ID); err != nil || !ok {
			t.Fatalf("DeleteJob(%s): ok=%v err=%v", state, ok, err)
		}
		if _, found, err := s.JobWithTransfer(ctx, deleteJob.ID); err != nil || found {
			t.Fatalf("deleted %s job still present: found=%v err=%v", state, found, err)
		}
	}
}

// TestDeleteJobRemovesAllChildren covers issue #159's hard-delete button: the
// job's candidates, transfers, and job_events all go with it.
func TestDeleteJobRemovesAllChildren(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 40, now)
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: "peer_one", Score: 1.0}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	if _, _, err := s.RecordEnqueueIntent(ctx, cand.ID, "peer_one", "f1.flac", now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	if err := s.AddJobEvent(ctx, job.ID, core.EventSearch, "", now); err != nil {
		t.Fatalf("AddJobEvent: %v", err)
	}

	ok, err := s.DeleteJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if !ok {
		t.Fatal("expected DeleteJob to return true")
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("expected the job to be gone, got %+v", views)
	}
	if _, found, err := s.NextNewCandidate(ctx, job.ID); err != nil || found {
		t.Fatalf("expected zero candidates after DeleteJob, found=%v (%v)", found, err)
	}
	if trs, err := s.TransfersForCandidate(ctx, cand.ID); err != nil || len(trs) != 0 {
		t.Fatalf("expected zero transfers after DeleteJob, got %d (%v)", len(trs), err)
	}
	if events, err := s.JobEvents(ctx, job.ID); err != nil || len(events) != 0 {
		t.Fatalf("expected zero job_events after DeleteJob, got %d (%v)", len(events), err)
	}
}

func TestDeleteJobUnknownID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ok, err := s.DeleteJob(ctx, 99999)
	if err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if ok {
		t.Fatal("expected DeleteJob to return false for an unknown job id")
	}
}

// TestDeleteJobRefusedWhileImporting guards against deleting a job out from
// under an in-flight Lidarr import.
func TestDeleteJobRefusedWhileImporting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 41, now)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState to SELECTING: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateDownloading, now); err != nil {
		t.Fatalf("AdvanceJobState to DOWNLOADING: %v", err)
	}
	if err := s.AdvanceJobState(ctx, job.ID, core.StateImporting, now); err != nil {
		t.Fatalf("AdvanceJobState to IMPORTING: %v", err)
	}

	ok, err := s.DeleteJob(ctx, job.ID)
	if !errors.Is(err, ErrJobImporting) {
		t.Fatalf("DeleteJob err = %v, want ErrJobImporting", err)
	}
	if ok {
		t.Fatal("expected DeleteJob to return false when refused")
	}

	views, err := s.ListJobsWithTransfer(ctx)
	if err != nil {
		t.Fatalf("ListJobsWithTransfer: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected the IMPORTING job to survive the refused delete, got %+v", views)
	}
}

func TestListDashboardJobsStatusOrderAndIndependentFacets(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	activeID := insertDashboardTestJob(t, s, 2001, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "Active", "Artist", "peer_active", 0, now)
	importingID := insertDashboardTestJob(t, s, 2002, core.SourceLidarr, core.StateImporting, core.TransferStalled, "Importing", "Artist", "peer_importing", 0, now.Add(time.Second))
	// WANTED reports its own 'wanted' status (issue #416), not 'queued' — it
	// still sorts third under sort=st, ahead of stalled/failed/parked/done, so
	// wantIDs below is unchanged.
	wantedID := insertDashboardTestJob(t, s, 2003, core.SourceLidarr, core.StateWanted, "", "Wanted", "Artist", "", 0, now.Add(2*time.Second))
	stalledID := insertDashboardTestJob(t, s, 2004, core.SourceManual, core.StateDownloading, core.TransferStalled, "Stalled", "Artist", "peer_stalled", 0, now.Add(3*time.Second))
	failedLidarrID := insertDashboardTestJob(t, s, 2005, core.SourceLidarr, core.StateFailed, core.TransferInProgress, "Failed Lidarr", "Artist", "peer_failed", 0, now.Add(4*time.Second))
	failedManualID := insertDashboardTestJob(t, s, 2006, core.SourceManual, core.StateDownloading, core.TransferErrored, "Failed Manual", "Artist", "peer_failed_manual", 0, now.Add(5*time.Second))
	parkedID := insertDashboardTestJob(t, s, 2007, core.SourceManual, core.StateParked, core.TransferInProgress, "Parked", "Artist", "peer_parked", 0, now.Add(6*time.Second))
	doneID := insertDashboardTestJob(t, s, 2008, core.SourceLidarr, core.StateDone, core.TransferInProgress, "Done Percent%_Literal", "Artist", "Visible_Peer", 0, now.Add(7*time.Second))

	page, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: "all", Source: "all"})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	gotIDs := make([]int64, len(page.Jobs))
	for i, job := range page.Jobs {
		gotIDs[i] = job.Job.ID
	}
	wantIDs := []int64{activeID, importingID, wantedID, stalledID, failedLidarrID, failedManualID, parkedID, doneID}
	if fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
		t.Errorf("status order ids = %v, want %v", gotIDs, wantIDs)
	}
	if page.Total != 8 || page.Facets.Status.All != 8 || page.Facets.Status.Active != 1 || page.Facets.Status.Importing != 1 || page.Facets.Status.Wanted != 1 || page.Facets.Status.Stalled != 1 || page.Facets.Status.Failed != 2 || page.Facets.Status.Parked != 1 || page.Facets.Status.Done != 1 {
		t.Errorf("unexpected unfiltered counts: total=%d status=%+v", page.Total, page.Facets.Status)
	}
	if page.Facets.Status.Queued != 0 || page.Facets.Status.Selecting != 0 || page.Facets.Status.Waiting != 0 {
		t.Errorf("unexpected non-zero split-queued facets with no fixture in those statuses: %+v", page.Facets.Status)
	}
	if page.Facets.Source.All != 8 || page.Facets.Source.Lidarr != 5 || page.Facets.Source.Manual != 3 {
		t.Errorf("unexpected source facets: %+v", page.Facets.Source)
	}

	importing, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: "importing", Source: "all"})
	if err != nil {
		t.Fatalf("ListDashboardJobs importing: %v", err)
	}
	if importing.Total != 1 || len(importing.Jobs) != 1 || importing.Jobs[0].Job.ID != importingID {
		t.Errorf("importing page = total %d jobs %+v", importing.Total, importing.Jobs)
	}

	filtered, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: "failed", Source: "lidarr"})
	if err != nil {
		t.Fatalf("ListDashboardJobs filtered: %v", err)
	}
	if filtered.Total != 1 || len(filtered.Jobs) != 1 || filtered.Jobs[0].Job.ID != failedLidarrID {
		t.Errorf("filtered page = total %d jobs %+v", filtered.Total, filtered.Jobs)
	}
	// Status ignores the selected status but still respects source=lidarr.
	if filtered.Facets.Status.All != 5 || filtered.Facets.Status.Active != 1 || filtered.Facets.Status.Importing != 1 || filtered.Facets.Status.Failed != 1 || filtered.Facets.Status.Done != 1 {
		t.Errorf("status facets did not ignore only status: %+v", filtered.Facets.Status)
	}
	// Source ignores source=lidarr but still respects status=failed.
	if filtered.Facets.Source.All != 2 || filtered.Facets.Source.Lidarr != 1 || filtered.Facets.Source.Manual != 1 {
		t.Errorf("source facets did not ignore only source: %+v", filtered.Facets.Source)
	}

	literal, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: "all", Source: "all", Query: "%_literal"})
	if err != nil {
		t.Fatalf("ListDashboardJobs literal q: %v", err)
	}
	if literal.Total != 1 || len(literal.Jobs) != 1 || literal.Jobs[0].Job.ID != doneID {
		t.Errorf("literal q page = total %d jobs %+v", literal.Total, literal.Jobs)
	}
	peerSearch, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: "all", Source: "all", Query: "visible_peer"})
	if err != nil {
		t.Fatalf("ListDashboardJobs peer q: %v", err)
	}
	if peerSearch.Total != 1 || peerSearch.Jobs[0].Job.ID != doneID {
		t.Errorf("peer q page = total %d jobs %+v", peerSearch.Total, peerSearch.Jobs)
	}
	idSearch, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: "all", Source: "all", Query: fmt.Sprint(activeID)})
	if err != nil {
		t.Fatalf("ListDashboardJobs id q: %v", err)
	}
	if idSearch.Total != 0 || len(idSearch.Jobs) != 0 {
		t.Errorf("id-only q unexpectedly matched jobs: total %d jobs %+v", idSearch.Total, idSearch.Jobs)
	}
}

func TestListDashboardJobsPaginationAndOutOfRangePage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 13; i++ {
		insertDashboardTestJob(t, s, int64(3000+i), core.SourceLidarr, core.StateWanted, "", fmt.Sprintf("Album %02d", i), "Artist", "", 0, now.Add(time.Duration(i)*time.Second))
	}

	first, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "album", Dir: "asc", Filter: "all", Source: "all"})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	second, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Page: 1, Sort: "album", Dir: "asc", Filter: "all", Source: "all"})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	outside, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Page: 99, Sort: "album", Dir: "asc", Filter: "all", Source: "all"})
	if err != nil {
		t.Fatalf("out-of-range page: %v", err)
	}
	if len(first.Jobs) != int(DashboardJobsPageSize) || first.Total != 13 {
		t.Errorf("first = len %d total %d", len(first.Jobs), first.Total)
	}
	if len(second.Jobs) != 1 || second.Total != 13 || second.Jobs[0].Job.Title != "Album 12" {
		t.Errorf("second = len %d total %d jobs %+v", len(second.Jobs), second.Total, second.Jobs)
	}
	if outside.Jobs == nil || len(outside.Jobs) != 0 || outside.Total != 13 {
		t.Errorf("outside = jobs %#v total %d, want non-nil empty and 13", outside.Jobs, outside.Total)
	}
}

func TestListDashboardJobsPersistedSortsAndIDTieBreak(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	betaID := insertDashboardTestJob(t, s, 4001, core.SourceLidarr, core.StateWanted, "", "beta", "Artist", "", 2, now)
	alphaZID := insertDashboardTestJob(t, s, 4002, core.SourceLidarr, core.StateWanted, "", "Alpha", "Zebra", "", 1, now)
	alphaAID := insertDashboardTestJob(t, s, 4003, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "alpha", "Able", "zoe", 2, now)
	aliceID := insertDashboardTestJob(t, s, 4004, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "gamma", "Artist", "alice", 0, now)

	assertOrder := func(sort, dir string, want []int64) {
		t.Helper()
		page, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: sort, Dir: dir, Filter: "all", Source: "all"})
		if err != nil {
			t.Fatalf("sort %s %s: %v", sort, dir, err)
		}
		got := make([]int64, len(page.Jobs))
		for i, job := range page.Jobs {
			got[i] = job.Job.ID
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("sort %s %s = %v, want %v", sort, dir, got, want)
		}
	}
	assertOrder("album", "asc", []int64{alphaAID, alphaZID, betaID, aliceID})
	assertOrder("peer", "desc", []int64{alphaAID, aliceID, betaID, alphaZID})
	assertOrder("try", "desc", []int64{betaID, alphaAID, alphaZID, aliceID})
}

// TestListDashboardJobsSortTransferGroupsActiveBeforeStalledThenAge is issue
// #268: sort=transfer ranks active above stalled above everything else, then
// created_at ascending within a group — the same rule
// web/src/routes/jobSort.ts's 'transferOrder' used client-side, now moved
// into SQL. Deliberately includes a job outside the active/stalled union (the
// "queued" one) unfiltered, to prove it sorts last rather than being merely
// absent from a pre-filtered set.
func TestListDashboardJobsSortTransferGroupsActiveBeforeStalledThenAge(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	activeNewerID := insertDashboardTestJob(t, s, 6001, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "Active newer", "Artist", "peer_a1", 0, now.Add(4*time.Second))
	activeOlderID := insertDashboardTestJob(t, s, 6002, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "Active older", "Artist", "peer_a2", 0, now)
	stalledNewerID := insertDashboardTestJob(t, s, 6003, core.SourceLidarr, core.StateDownloading, core.TransferStalled, "Stalled newer", "Artist", "peer_s1", 0, now.Add(3*time.Second))
	stalledOlderID := insertDashboardTestJob(t, s, 6004, core.SourceLidarr, core.StateDownloading, core.TransferStalled, "Stalled older", "Artist", "peer_s2", 0, now.Add(time.Second))
	queuedID := insertDashboardTestJob(t, s, 6005, core.SourceLidarr, core.StateWanted, "", "Queued", "Artist", "", 0, now.Add(2*time.Second))

	page, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "transfer", Dir: "asc", Filter: "all", Source: "all"})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	got := make([]int64, len(page.Jobs))
	for i, job := range page.Jobs {
		got[i] = job.Job.ID
	}
	want := []int64{activeOlderID, activeNewerID, stalledOlderID, stalledNewerID, queuedID}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sort=transfer ids = %v, want %v (active-oldest-first, then stalled-oldest-first, then everything else)", got, want)
	}
}

// TestListDashboardJobsSortTransferIDTieBreak covers the id tiebreak
// specifically: two jobs in the same status group with an identical
// created_at must come out in id order, since without a tiebreaker
// Postgres' order for equal values is undefined and the same job could
// appear on two pages while another never shows at all.
func TestListDashboardJobsSortTransferIDTieBreak(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	firstID := insertDashboardTestJob(t, s, 6101, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "First", "Artist", "peer_1", 0, now)
	secondID := insertDashboardTestJob(t, s, 6102, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "Second", "Artist", "peer_2", 0, now)
	if firstID >= secondID {
		t.Fatalf("test setup: expected firstID < secondID, got %d and %d", firstID, secondID)
	}

	page, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "transfer", Dir: "asc", Filter: "all", Source: "all"})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	got := make([]int64, len(page.Jobs))
	for i, job := range page.Jobs {
		got[i] = job.Job.ID
	}
	want := []int64{firstID, secondID}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("tied active jobs = %v, want %v (id ascending)", got, want)
	}
}

// TestDashboardJobsOrderTransferIgnoresDirection is a pure-function
// counterpart to the id-tiebreak test above: sort=transfer's whole purpose
// is a stable ranking, so dashboardJobsOrder must produce byte-identical SQL
// (group ascending, age ascending, id ascending) regardless of Dir — proof
// the ordering holds under both dir values even though
// validateDashboardJobsQuery additionally rejects dir=desc outright as a
// defense-in-depth measure (see TestValidateDashboardJobsQueryRejectsDescForTransferSort).
func TestDashboardJobsOrderTransferIgnoresDirection(t *testing.T) {
	asc := dashboardJobsOrder(DashboardJobsQuery{Sort: "transfer", Dir: "asc"})
	desc := dashboardJobsOrder(DashboardJobsQuery{Sort: "transfer", Dir: "desc"})
	if asc != desc {
		t.Fatalf("dashboardJobsOrder(transfer) differs by Dir:\nasc  = %q\ndesc = %q", asc, desc)
	}
	if strings.Contains(asc, "DESC") {
		t.Errorf("dashboardJobsOrder(transfer) = %q, must never contain DESC", asc)
	}
}

// TestValidateDashboardJobsQueryRejectsDescForTransferSort covers the
// decision issue #268 asked for explicitly: sort=transfer's ranking exists
// to be stable, so dir=desc — which would only reverse that stability for no
// meaningful alternative order — is rejected rather than silently
// reinterpreted.
func TestValidateDashboardJobsQueryRejectsDescForTransferSort(t *testing.T) {
	err := validateDashboardJobsQuery(DashboardJobsQuery{
		PageSize: DashboardJobsPageSize, Sort: "transfer", Dir: "desc", Filter: "all", Source: "all",
	})
	if err == nil {
		t.Fatal("expected an error for sort=transfer, dir=desc")
	}
	if err := validateDashboardJobsQuery(DashboardJobsQuery{
		PageSize: DashboardJobsPageSize, Sort: "transfer", Dir: "asc", Filter: "all", Source: "all",
	}); err != nil {
		t.Errorf("sort=transfer, dir=asc must be valid, got %v", err)
	}
}

// TestValidateDashboardJobsQueryPageSizeBounds covers issue #268's bounded
// pageSize: 1-50 inclusive, with 0 (Go's zero value, meaning "unset" — see
// ListDashboardJobs) accepted only via that defaulting path, never directly
// by validateDashboardJobsQuery itself.
func TestValidateDashboardJobsQueryPageSizeBounds(t *testing.T) {
	base := DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: "all", Source: "all"}
	for _, pageSize := range []int64{0, -1, 51} {
		q := base
		q.PageSize = pageSize
		if err := validateDashboardJobsQuery(q); err == nil {
			t.Errorf("PageSize=%d: expected an error", pageSize)
		}
	}
	for _, pageSize := range []int64{1, 12, 50} {
		q := base
		q.PageSize = pageSize
		if err := validateDashboardJobsQuery(q); err != nil {
			t.Errorf("PageSize=%d: expected no error, got %v", pageSize, err)
		}
	}
}

// TestListDashboardJobsPageSizeDefaultsWhenUnset covers ListDashboardJobs'
// own defaulting: a query that never sets PageSize (every store-level test
// predating issue #268, and any future caller that genuinely doesn't care)
// gets DashboardJobsPageSize rather than failing validation on a bound
// nothing chose.
func TestListDashboardJobsPageSizeDefaultsWhenUnset(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for i := 0; i < int(DashboardJobsPageSize)+1; i++ {
		insertDashboardTestJob(t, s, int64(7000+i), core.SourceLidarr, core.StateWanted, "", fmt.Sprintf("Album %02d", i), "Artist", "", 0, now.Add(time.Duration(i)*time.Second))
	}
	page, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "album", Dir: "asc", Filter: "all", Source: "all"})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	if int64(len(page.Jobs)) != DashboardJobsPageSize {
		t.Errorf("len(page.Jobs) = %d, want the default page size %d", len(page.Jobs), DashboardJobsPageSize)
	}
}

// TestListDashboardJobsExplicitPageSize covers an explicit, smaller PageSize
// (Overview's TRANSFERS panel requests 8, issue #268): the LIMIT actually
// applied must match, and it must still compose with filter=inflight and
// sort=transfer.
func TestListDashboardJobsExplicitPageSize(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		insertDashboardTestJob(t, s, int64(8000+i), core.SourceLidarr, core.StateDownloading, core.TransferInProgress, fmt.Sprintf("Active %02d", i), "Artist", fmt.Sprintf("peer_%d", i), 0, now.Add(time.Duration(i)*time.Second))
	}
	page, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{
		Sort: "transfer", Dir: "asc", Filter: "inflight", Source: "all", PageSize: 8,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	if len(page.Jobs) != 8 || page.Total != 10 {
		t.Errorf("len(page.Jobs) = %d total = %d, want 8 of 10", len(page.Jobs), page.Total)
	}
}

// TestListJobsWithTransferPrefersActiveOverNewerNeverTried is issue #269: a
// job's status/bytes/peer must come from its ACTIVE candidate, never from a
// same-batch NEW candidate that merely has a higher id. InsertCandidates
// writes every candidate of one search pass with an identical created_at, so
// the naive "most recently created" ordering this view used to use falls
// back to id DESC — and since NextNewCandidate tries candidates
// best-score-first, that tiebreak deterministically picks the WORST-ranked,
// never-attempted candidate. On the pre-fix join that candidate has zero
// transfers, so the job read back as queued/0 bytes/no peer despite actively
// downloading.
func TestListJobsWithTransferPrefersActiveOverNewerNeverTried(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 100, now)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	// Both candidates inserted in the same batch, so they share created_at —
	// exactly InsertCandidates' real-world behavior for one search pass.
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{
		{Username: "active_peer", Score: 2.0, Files: []core.CandidateFile{{Filename: "01.flac", Size: 1000}}},
		{Username: "never_tried_peer", Score: 1.0, Files: []core.CandidateFile{{Filename: "01.flac", Size: 500}}},
	}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}

	// NextNewCandidate tries best-score-first, so this is active_peer's
	// candidate — inserted first, so it also has the LOWER id; the untried
	// never_tried_peer candidate has the higher one.
	winner, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	if winner.Username != "active_peer" {
		t.Fatalf("expected active_peer picked first (best score), got %q", winner.Username)
	}

	ok, _, err := s.ActivateCandidateWithTransfers(ctx, winner.ID, job.ID, 5, now)
	if err != nil || !ok {
		t.Fatalf("ActivateCandidateWithTransfers: ok=%v err=%v", ok, err)
	}
	transfers, err := s.TransfersForCandidate(ctx, winner.ID)
	if err != nil || len(transfers) != 1 {
		t.Fatalf("TransfersForCandidate: %v (%d transfers)", err, len(transfers))
	}
	if err := s.UpdateTransferProgress(ctx, transfers[0].ID, core.TransferInProgress, 400, 1000, now.Add(time.Minute)); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}

	view, found, err := s.JobWithTransfer(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
	}
	if view.Status != "active" {
		t.Errorf("Status = %q, want active (the pre-fix join reports queued here)", view.Status)
	}
	if view.AlbumBytesDone != 400 {
		t.Errorf("AlbumBytesDone = %d, want 400 (the pre-fix join reports 0 here)", view.AlbumBytesDone)
	}
	if view.Peer != "active_peer" {
		t.Errorf("Peer = %q, want active_peer (the pre-fix join reports the untried candidate's peer)", view.Peer)
	}
}

// TestListJobsWithTransferPrefersSucceededOverNeverTried covers a finished
// (DONE) job whose SUCCEEDED candidate must still supply the bytes and peer,
// never a higher-id NEW candidate left behind by a later, unrelated search
// pass. The job reaches DONE the way the real pipeline does it:
// DOWNLOADING→IMPORTING via AdvanceJobStateFrom (candidate stays ACTIVE, see
// internal/pipeline/downloading.go), then IMPORTING→DONE via
// SucceedCandidateAndAdvance (see internal/pipeline/importing.go), which is
// the only transition that ever marks a candidate SUCCEEDED.
func TestListJobsWithTransferPrefersSucceededOverNeverTried(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 101, now)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{
		{Username: "winner_peer", Score: 5.0, Files: []core.CandidateFile{{Filename: "01.flac", Size: 2000}}},
	}, now); err != nil {
		t.Fatalf("InsertCandidates winner: %v", err)
	}
	winner, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	ok, _, err := s.ActivateCandidateWithTransfers(ctx, winner.ID, job.ID, 5, now)
	if err != nil || !ok {
		t.Fatalf("ActivateCandidateWithTransfers: ok=%v err=%v", ok, err)
	}
	transfers, err := s.TransfersForCandidate(ctx, winner.ID)
	if err != nil || len(transfers) != 1 {
		t.Fatalf("TransfersForCandidate: %v (%d transfers)", err, len(transfers))
	}
	if err := s.UpdateTransferProgress(ctx, transfers[0].ID, core.TransferCompleted, 2000, 2000, now.Add(time.Minute)); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}
	if ok, err := s.AdvanceJobStateFrom(ctx, job.ID, core.StateDownloading, core.StateImporting, now.Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("AdvanceJobStateFrom: ok=%v err=%v", ok, err)
	}
	if ok, err := s.SucceedCandidateAndAdvance(ctx, winner.ID, job.ID, core.StateImporting, core.StateDone, now.Add(3*time.Minute)); err != nil || !ok {
		t.Fatalf("SucceedCandidateAndAdvance: ok=%v err=%v", ok, err)
	}

	// A later, unrelated search pass leaves several never-tried candidates
	// with higher ids than winner's.
	later := now.Add(4 * time.Minute)
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{
		{Username: "later_peer_1", Score: 3.0, Files: []core.CandidateFile{{Filename: "x.flac", Size: 1}}},
		{Username: "later_peer_2", Score: 1.0, Files: []core.CandidateFile{{Filename: "y.flac", Size: 1}}},
	}, later); err != nil {
		t.Fatalf("InsertCandidates later: %v", err)
	}

	view, found, err := s.JobWithTransfer(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
	}
	if view.Status != "done" {
		t.Errorf("Status = %q, want done", view.Status)
	}
	if view.AlbumBytesDone != 2000 || view.AlbumBytesTotal != 2000 {
		t.Errorf("AlbumBytes = %d/%d, want 2000/2000 (the succeeded candidate)", view.AlbumBytesDone, view.AlbumBytesTotal)
	}
	if view.Peer != "winner_peer" {
		t.Errorf("Peer = %q, want winner_peer", view.Peer)
	}
}

// TestListJobsWithTransferAggregateActiveOutranksLatestUpdatedRow is issue
// #269's aggregate case: a candidate with several transfers where only ONE
// is IN_PROGRESS, and the most recently updated row is actually COMPLETED —
// the job is still 'active', which a single-latest-transfer join could only
// report correctly by accident (whenever the in-progress row happened to
// also be the most recently touched one).
func TestListJobsWithTransferAggregateActiveOutranksLatestUpdatedRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 102, now)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{
		{Username: "multi_peer", Score: 1.0, Files: []core.CandidateFile{
			{Filename: "01.flac", Size: 1000},
			{Filename: "02.flac", Size: 1000},
			{Filename: "03.flac", Size: 1000},
		}},
	}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	ok, _, err := s.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, 5, now)
	if err != nil || !ok {
		t.Fatalf("ActivateCandidateWithTransfers: ok=%v err=%v", ok, err)
	}
	transfers, err := s.TransfersForCandidate(ctx, cand.ID)
	if err != nil || len(transfers) != 3 {
		t.Fatalf("TransfersForCandidate: %v (%d transfers)", err, len(transfers))
	}
	byFilename := map[string]core.Transfer{}
	for _, tr := range transfers {
		byFilename[tr.Filename] = tr
	}

	// Touched first: the file that stays IN_PROGRESS.
	if err := s.UpdateTransferProgress(ctx, byFilename["01.flac"].ID, core.TransferInProgress, 500, 1000, now.Add(time.Minute)); err != nil {
		t.Fatalf("UpdateTransferProgress 01: %v", err)
	}
	// Touched LAST, so it — not the in-progress file — is the row a
	// single-latest-transfer join would have picked.
	if err := s.UpdateTransferProgress(ctx, byFilename["02.flac"].ID, core.TransferCompleted, 1000, 1000, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("UpdateTransferProgress 02: %v", err)
	}
	// 03.flac stays PENDING, untouched since activation.

	view, found, err := s.JobWithTransfer(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
	}
	if view.Status != "active" {
		t.Errorf("Status = %q, want active (one file is still IN_PROGRESS)", view.Status)
	}
}

// TestJobWithTransferCandidatelessDownloadingJobIsQueued covers a
// DOWNLOADING job with zero candidates — a.id is NULL, so jobViewFrom's agg
// LATERAL aggregates over no rows. This is correct by construction (an
// ungrouped aggregate always returns exactly one row; COUNT(*) FILTER yields
// 0, never NULL, so dashboardJobStatusSQL's CASE cannot fall through on
// three-valued logic), but nothing pinned it before this test. DOWNLOADING is
// used rather than a job-level state (DONE, FAILED, ...) because those
// short-circuit dashboardJobStatusSQL before it ever reaches the agg
// branches. Nothing has been delivered (agg.completed = 0), so this lands on
// 'queued', not 'waiting' (issue #416).
//
// The real pipeline never leaves a DOWNLOADING job without an ACTIVE
// candidate, so this fixture is built with direct SQL rather than the
// store's public API, the same way insertDashboardTestJob already does for
// other unreachable-via-API shapes in this file.
func TestJobWithTransferCandidatelessDownloadingJobIsQueued(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	var jobID int64
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO album_jobs (lidarr_album_id, source, state, retries, created_at, updated_at, title, artist_name)
		 VALUES ($1, $2, $3, $4, $5, $5, $6, $7) RETURNING id`,
		9010, string(core.SourceLidarr), string(core.StateDownloading), 0, now, "No Candidate", "Artist").Scan(&jobID); err != nil {
		t.Fatalf("insert candidateless job: %v", err)
	}

	view, found, err := s.JobWithTransfer(ctx, jobID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
	}
	if view.Status != "queued" {
		t.Errorf("Status = %q, want queued", view.Status)
	}
	if view.AlbumBytesDone != 0 || view.AlbumBytesTotal != 0 || view.AlbumBytesRemaining != 0 {
		t.Errorf("AlbumBytes = %d/%d remaining=%d, want all zero", view.AlbumBytesDone, view.AlbumBytesTotal, view.AlbumBytesRemaining)
	}
	if view.Attempt != nil {
		t.Errorf("Attempt = %+v, want nil", view.Attempt)
	}
}

// TestListDashboardJobsPerRowStatusMatchesFacetsAndFilter guards the drift
// issue #269 found between this package's dashboardJobStatusSQL and the Go
// copy that used to live in observ.dashboardStatus (IMPORTING mapped to
// 'importing' here but 'active' there). Now that jobDTO.Status is read
// straight from core.JobView.Status — computed once by dashboardJobStatusSQL
// and reused by every filter — a per-row status, its own filter, and the
// facet count built around the same CASE can never disagree. This checks
// that invariant across one fixture of every dashboard status.
func TestListDashboardJobsPerRowStatusMatchesFacetsAndFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	fixtures := []struct {
		id     int64
		state  core.AlbumJobState
		tstate core.TransferState
		peer   string
		status string
	}{
		{9001, core.StateDownloading, core.TransferInProgress, "peer_active", "active"},
		{9002, core.StateImporting, "", "", "importing"},
		{9003, core.StateWanted, "", "", "wanted"},
		{9004, core.StateDownloading, core.TransferStalled, "peer_stalled", "stalled"},
		{9005, core.StateDownloading, core.TransferErrored, "peer_failed", "failed"},
		{9006, core.StateParked, "", "", "parked"},
		{9007, core.StateDone, "", "", "done"},
		{9008, core.StateSelecting, "", "", "selecting"},
		{9009, core.StateDownloading, core.TransferPending, "peer_queued", "queued"},
		{9010, core.StateDownloading, core.TransferCompleted, "peer_waiting", "waiting"},
		// A legacy state nothing in production writes any more (issue #416):
		// every dead pre-download state falls to the CASE's ELSE, 'wanted'.
		{9011, core.AlbumJobState("COOLDOWN"), "", "", "wanted"},
	}
	statusByID := map[int64]string{}
	for i, f := range fixtures {
		id := insertDashboardTestJob(t, s, f.id, core.SourceLidarr, f.state, f.tstate, fmt.Sprintf("Job %d", i), "Artist", f.peer, 0, now.Add(time.Duration(i)*time.Second))
		statusByID[id] = f.status
	}

	page, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: "all", Source: "all"})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	if len(page.Jobs) != len(fixtures) {
		t.Fatalf("expected %d jobs, got %d", len(fixtures), len(page.Jobs))
	}
	seen := map[int64]bool{}
	for _, job := range page.Jobs {
		want, ok := statusByID[job.Job.ID]
		if !ok {
			t.Fatalf("unexpected job id %d in page", job.Job.ID)
		}
		seen[job.Job.ID] = true
		if job.Status != want {
			t.Errorf("job %d: Status = %q, want %q", job.Job.ID, job.Status, want)
		}

		// The per-row status must also be exactly the value its own filter
		// selects it under.
		filtered, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: want, Source: "all"})
		if err != nil {
			t.Fatalf("ListDashboardJobs filter=%s: %v", want, err)
		}
		found := false
		for _, fj := range filtered.Jobs {
			if fj.Job.ID == job.Job.ID {
				found = true
			}
		}
		if !found {
			t.Errorf("job %d (status %q) not found when filtering by its own status", job.Job.ID, want)
		}
	}
	if len(seen) != len(fixtures) {
		t.Fatalf("expected every fixture job returned, got %d of %d", len(seen), len(fixtures))
	}
}

// TestListDashboardJobsStatusInProgressOutranksCompleted covers issue #416's
// 'queued'/'waiting' split: a DOWNLOADING candidate with one file still
// IN_PROGRESS and another already COMPLETED must report 'active', not
// 'waiting' — agg.in_progress is checked before agg.completed in
// dashboardJobStatusSQL, so a file actually moving always wins over one that
// merely finished.
func TestListDashboardJobsStatusInProgressOutranksCompleted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	jobID := insertDashboardTestJob(t, s, 9101, core.SourceLidarr, core.StateDownloading, core.TransferCompleted, "Mixed Progress Album", "Artist", "peer_mixed", 0, now)

	var candidateID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM candidates WHERE album_job_id = $1`, jobID).Scan(&candidateID); err != nil {
		t.Fatalf("select candidate: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO transfers (candidate_id, username, filename, state, deadline, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		candidateID, "peer_mixed", "track2.flac", string(core.TransferInProgress), now.Add(time.Hour), now); err != nil {
		t.Fatalf("insert second transfer: %v", err)
	}

	view, found, err := s.JobWithTransfer(ctx, jobID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
	}
	if view.Status != "active" {
		t.Errorf("Status = %q, want active (one file still IN_PROGRESS outranks the other's COMPLETED)", view.Status)
	}
}

// TestJobViewStatusNotImportedForNotImportedState covers issue #59's status
// mapping: dashboardJobStatusSQL must classify NOT_IMPORTED as "notImported"
// on its own, checked before the transfer-aggregate fallback that would
// otherwise (wrongly) read it as "queued" - a NOT_IMPORTED job's completed
// transfers leave agg.in_progress/stalled/live/failed all at their zero
// values, which is exactly the shape the ELSE 'queued' branch matches.
// Deliberately not folded into
// TestListDashboardJobsPerRowStatusMatchesFacetsAndFilter's fixture table:
// "notImported" is not (yet) an accepted dashboard Filter value, so this
// checks the JobView projection directly instead of round-tripping through
// ListDashboardJobs's filter.
func TestJobViewStatusNotImportedForNotImportedState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	id := insertDashboardTestJob(t, s, 0, core.SourceManual, core.StateNotImported, core.TransferCompleted, "Job", "Artist", "peer_one", 0, now)

	view, found, err := s.JobWithTransfer(ctx, id)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: %v found=%v", err, found)
	}
	if view.Status != "notImported" {
		t.Errorf("Status = %q, want notImported", view.Status)
	}
}

// TestListDashboardJobsAggregateActiveMatchesFacetAndFilter is the
// ListDashboardJobs-level counterpart of
// TestListJobsWithTransferAggregateActiveOutranksLatestUpdatedRow: that test
// covers the aggregate rule only through JobWithTransfer's single-row path.
// Here a job's candidate has many transfers with exactly one IN_PROGRESS and
// the rest PENDING/COMPLETED, so the drift issue #269 removed — per-row
// status, the status facet count, and filter=active — can only be caught by
// checking all three against the same fixture.
func TestListDashboardJobsAggregateActiveMatchesFacetAndFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 9020, now)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{
		{Username: "multi_peer", Score: 1.0, Files: []core.CandidateFile{
			{Filename: "01.flac", Size: 1000},
			{Filename: "02.flac", Size: 1000},
			{Filename: "03.flac", Size: 1000},
		}},
	}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	ok, _, err := s.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, 5, now)
	if err != nil || !ok {
		t.Fatalf("ActivateCandidateWithTransfers: ok=%v err=%v", ok, err)
	}
	transfers, err := s.TransfersForCandidate(ctx, cand.ID)
	if err != nil || len(transfers) != 3 {
		t.Fatalf("TransfersForCandidate: %v (%d transfers)", err, len(transfers))
	}
	byFilename := map[string]core.Transfer{}
	for _, tr := range transfers {
		byFilename[tr.Filename] = tr
	}
	// One IN_PROGRESS, one COMPLETED, one left PENDING (untouched since
	// activation) — exactly the mixed aggregate the "most recently updated
	// row" join used to get wrong.
	if err := s.UpdateTransferProgress(ctx, byFilename["01.flac"].ID, core.TransferInProgress, 500, 1000, now.Add(time.Minute)); err != nil {
		t.Fatalf("UpdateTransferProgress 01: %v", err)
	}
	if err := s.UpdateTransferProgress(ctx, byFilename["02.flac"].ID, core.TransferCompleted, 1000, 1000, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("UpdateTransferProgress 02: %v", err)
	}

	page, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: "all", Source: "all"})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	var row *core.JobView
	for i := range page.Jobs {
		if page.Jobs[i].Job.ID == job.ID {
			row = &page.Jobs[i]
		}
	}
	if row == nil {
		t.Fatalf("job %d not found in all-jobs page", job.ID)
	}
	if row.Status != "active" {
		t.Errorf("row Status = %q, want active", row.Status)
	}
	if page.Facets.Status.Active != 1 {
		t.Errorf("Status facet Active = %d, want 1", page.Facets.Status.Active)
	}

	filtered, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: "active", Source: "all"})
	if err != nil {
		t.Fatalf("ListDashboardJobs filter=active: %v", err)
	}
	found = false
	for _, fj := range filtered.Jobs {
		if fj.Job.ID == job.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("job %d not returned by filter=active", job.ID)
	}
}

func TestListDashboardJobsFilterInflightSelectsByStateNotStatus(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// DOWNLOADING with a file actually moving -> status 'active'.
	moving := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "Moving", "A", "peer1", 0, now)
	// DOWNLOADING with everything still PENDING -> status 'queued', which
	// the transferring union never selected. This is the whole point of inflight.
	pending := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateDownloading, core.TransferPending, "Pending", "B", "peer2", 0, now.Add(time.Second))
	// DOWNLOADING with a stalled file -> status 'stalled'.
	stalled := insertDashboardTestJob(t, s, 3, core.SourceLidarr, core.StateDownloading, core.TransferStalled, "Stalled", "C", "peer3", 0, now.Add(2*time.Second))
	// IMPORTING -> status 'importing', also never in the transferring union.
	importing := insertDashboardTestJob(t, s, 4, core.SourceLidarr, core.StateImporting, "", "Importing", "D", "peer4", 0, now.Add(3*time.Second))
	// Excluded: not yet started, and already finished.
	insertDashboardTestJob(t, s, 5, core.SourceLidarr, core.StateWanted, "", "Wanted", "E", "", 0, now.Add(4*time.Second))
	insertDashboardTestJob(t, s, 6, core.SourceLidarr, core.StateSelecting, "", "Selecting", "F", "", 0, now.Add(5*time.Second))
	insertDashboardTestJob(t, s, 7, core.SourceLidarr, core.StateDone, "", "Done", "G", "", 0, now.Add(6*time.Second))
	insertDashboardTestJob(t, s, 8, core.SourceLidarr, core.StateFailed, "", "Failed", "H", "", 0, now.Add(7*time.Second))
	insertDashboardTestJob(t, s, 9, core.SourceLidarr, core.StateParked, "", "Parked", "I", "", 0, now.Add(8*time.Second))

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "st", Dir: "asc", Filter: "inflight", Source: "all", PageSize: 20,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	got := map[int64]bool{}
	for _, view := range page.Jobs {
		got[view.Job.ID] = true
	}
	for _, want := range []int64{moving, pending, stalled, importing} {
		if !got[want] {
			t.Errorf("job %d missing from inflight page", want)
		}
	}
	if len(page.Jobs) != 4 {
		t.Fatalf("len(jobs) = %d, want 4; got ids %v", len(page.Jobs), got)
	}
	if page.Total != 4 {
		t.Errorf("Total = %d, want 4", page.Total)
	}
	// Status facets must ignore the selected filter (same contract every
	// other filter value has — see TestListDashboardJobsStatusOrderAndIndependentFacets)
	// and so report the FULL unfiltered counts, not just the inflight subset.
	// moving=active, pending=queued, wanted=wanted, selecting=selecting,
	// stalled=stalled, importing=importing, done=done, failed=failed,
	// parked=parked (issue #416 split what used to collapse into 'queued').
	if page.Facets.Status.All != 9 || page.Facets.Status.Active != 1 || page.Facets.Status.Queued != 1 ||
		page.Facets.Status.Wanted != 1 || page.Facets.Status.Selecting != 1 ||
		page.Facets.Status.Stalled != 1 || page.Facets.Status.Importing != 1 || page.Facets.Status.Done != 1 ||
		page.Facets.Status.Failed != 1 || page.Facets.Status.Parked != 1 {
		t.Errorf("status facets did not ignore filter=inflight: %+v", page.Facets.Status)
	}
	if page.Facets.Status.Waiting != 0 {
		t.Errorf("Waiting facet = %d, want 0 (no fixture has a COMPLETED transfer)", page.Facets.Status.Waiting)
	}
}

func TestListDashboardJobsFilterInflightAndFinishedAreDisjoint(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// A DOWNLOADING job whose only transfer errored reports status 'failed'
	// via the candidate aggregate (dashboard.go:142) while its state is still
	// DOWNLOADING. Filtering on status would place it in BOTH regions; both
	// filters go through j.state precisely so it cannot.
	id := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDownloading, core.TransferErrored, "Errored", "A", "peer1", 0, now)

	inflight, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "st", Dir: "asc", Filter: "inflight", Source: "all", PageSize: 20,
	})
	if err != nil {
		t.Fatalf("inflight: %v", err)
	}
	if len(inflight.Jobs) != 1 || inflight.Jobs[0].Job.ID != id {
		t.Fatalf("inflight jobs = %+v, want exactly job %d", inflight.Jobs, id)
	}
	if inflight.Jobs[0].Status != "failed" {
		t.Errorf("Status = %q, want %q (the aggregate-derived status is unchanged)", inflight.Jobs[0].Status, "failed")
	}

	// The other half of the disjointness claim: the same job must NOT show up
	// under 'finished'. Use the same 'now' the job was inserted with, so it
	// sits squarely inside DashboardFinishedWindow — if 'finished' were ever
	// switched back to a status-keyed predicate (matching status 'failed'
	// instead of state DONE/FAILED), this job would wrongly appear here, and
	// the window itself couldn't be blamed for excluding it.
	finished, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 20, Now: now,
	})
	if err != nil {
		t.Fatalf("finished: %v", err)
	}
	if len(finished.Jobs) != 0 {
		t.Fatalf("finished jobs = %+v, want none (state is DOWNLOADING, not DONE/FAILED)", finished.Jobs)
	}
}

// TestListDashboardJobsFilterFailuresSelectsByStateNotStatus is the fix for
// Overview's FAILED panel (issue #310 review follow-up): filter=failed falls
// through to dashboardJobsWhere's default case, which matches
// dashboardJobStatusSQL's status-derived 'failed' — and that status also
// covers a job still in DOWNLOADING whose current candidate's transfers all
// errored (agg.live = 0 AND agg.failed > 0), which the pipeline will retry
// with the next candidate. filter=failures must be keyed on j.state instead,
// so it selects only a terminal StateFailed job and excludes that
// mid-retry DOWNLOADING one. Both filters are asserted against the same
// fixture so the difference between them is the thing under test — if
// "failures" were ever routed to the default case (dashboardJobStatusSQL),
// this test would catch it because the DOWNLOADING/errored job would then
// wrongly appear in its results too.
func TestListDashboardJobsFilterFailuresSelectsByStateNotStatus(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Terminal failure: state FAILED. Must appear under both "failed" and
	// "failures".
	terminal := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateFailed, "", "Terminal", "A", "", 0, now)
	// Mid-retry: state DOWNLOADING, but its only transfer errored, so its
	// dashboard status is 'failed' too. Must appear under "failed" but NOT
	// under "failures".
	midRetry := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateDownloading, core.TransferErrored, "MidRetry", "B", "peer2", 0, now.Add(time.Second))

	failuresPage, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "recent", Dir: "desc", Filter: "failures", Source: "all", PageSize: 20, Now: now,
	})
	if err != nil {
		t.Fatalf("filter=failures: %v", err)
	}
	gotFailures := map[int64]bool{}
	for _, view := range failuresPage.Jobs {
		gotFailures[view.Job.ID] = true
	}
	if !gotFailures[terminal] {
		t.Errorf("filter=failures missing terminal job %d", terminal)
	}
	if gotFailures[midRetry] {
		t.Errorf("filter=failures wrongly includes mid-retry DOWNLOADING job %d", midRetry)
	}
	if len(failuresPage.Jobs) != 1 {
		t.Fatalf("filter=failures jobs = %+v, want exactly [%d]", failuresPage.Jobs, terminal)
	}

	failedPage, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "st", Dir: "asc", Filter: "failed", Source: "all", PageSize: 20,
	})
	if err != nil {
		t.Fatalf("filter=failed: %v", err)
	}
	gotFailed := map[int64]bool{}
	for _, view := range failedPage.Jobs {
		gotFailed[view.Job.ID] = true
	}
	// filter=failed is the status-derived predicate: it DOES include the
	// mid-retry job, unlike filter=failures above — that's the whole point
	// of the distinction this test exists to lock in.
	if !gotFailed[terminal] || !gotFailed[midRetry] {
		t.Errorf("filter=failed jobs = %+v, want both %d and %d", failedPage.Jobs, terminal, midRetry)
	}
}

func TestListDashboardJobsSortTransferRanksImportingAfterWaiting(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Inserted in reverse rank order so a passing test cannot be an accident
	// of insertion order: importing first, active last.
	importing := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateImporting, "", "Importing", "A", "peer1", 0, now)
	waiting := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateDownloading, core.TransferPending, "Waiting", "B", "peer2", 0, now.Add(time.Second))
	stalled := insertDashboardTestJob(t, s, 3, core.SourceLidarr, core.StateDownloading, core.TransferStalled, "Stalled", "C", "peer3", 0, now.Add(2*time.Second))
	active := insertDashboardTestJob(t, s, 4, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "Active", "D", "peer4", 0, now.Add(3*time.Second))

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "transfer", Dir: "asc", Filter: "inflight", Source: "all", PageSize: 20,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	got := make([]int64, 0, len(page.Jobs))
	for _, view := range page.Jobs {
		got = append(got, view.Job.ID)
	}
	want := []int64{active, stalled, waiting, importing}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v (active, stalled, waiting, importing)", got, want)
	}
}

func TestListDashboardJobsSortTransferKeepsAgeOrderWithinGroup(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	older := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateImporting, "", "Older", "A", "peer1", 0, now)
	newer := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateImporting, "", "Newer", "B", "peer2", 0, now.Add(time.Minute))

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "transfer", Dir: "asc", Filter: "inflight", Source: "all", PageSize: 20,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	if len(page.Jobs) != 2 || page.Jobs[0].Job.ID != older || page.Jobs[1].Job.ID != newer {
		t.Fatalf("order = %+v, want [%d %d] (created_at ascending within a group)", page.Jobs, older, newer)
	}
}

func TestListDashboardJobsFilterFinishedHonoursTheWindow(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// insertDashboardTestJob writes its `at` argument to both created_at and
	// updated_at, which is exactly what the window reads.
	justDone := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDone, "", "Just done", "A", "peer1", 0, now.Add(-time.Minute))
	justFailed := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateFailed, "", "Just failed", "B", "peer2", 0, now.Add(-30*time.Minute))
	// One second inside the window survives; one second outside does not.
	insideEdge := insertDashboardTestJob(t, s, 3, core.SourceLidarr, core.StateDone, "", "Inside edge", "C", "peer3", 0, now.Add(-DashboardFinishedWindow).Add(time.Second))
	insertDashboardTestJob(t, s, 4, core.SourceLidarr, core.StateDone, "", "Outside edge", "D", "peer4", 0, now.Add(-DashboardFinishedWindow).Add(-time.Second))
	// Excluded by state regardless of how fresh they are.
	insertDashboardTestJob(t, s, 5, core.SourceLidarr, core.StateParked, "", "Parked", "E", "", 0, now)
	insertDashboardTestJob(t, s, 6, core.SourceLidarr, core.StateDownloading, core.TransferInProgress, "Downloading", "F", "peer6", 0, now)
	insertDashboardTestJob(t, s, 7, core.SourceLidarr, core.StateWanted, "", "Wanted", "G", "", 0, now)

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 20, Now: now,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	got := make([]int64, 0, len(page.Jobs))
	for _, view := range page.Jobs {
		got = append(got, view.Job.ID)
	}
	// sort=recent is updated_at descending, so newest finish first.
	want := []int64{justDone, justFailed, insideEdge}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

// A manual job that finishes as NOT_IMPORTED (issue #59) downloaded
// successfully — it just had no Lidarr album to import into. Overview's
// recently-finished panel is the one place a user watches for completions, so
// leaving it out of filter=finished would make a download that worked look
// like nothing happened. PARKED and CANCELLED stay out (see the predicate's
// comment); this test pins all three decisions together so a future state
// cannot be added to PipelineTerminal() and silently inherit the wrong answer.
func TestListDashboardJobsFinishedIncludesNotImported(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Minute)

	notImported := insertDashboardTestJob(t, s, 0, core.SourceManual, core.StateNotImported, "", "Kid A", "Radiohead", "peer1", 0, at)
	done := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDone, "", "Rounds", "Four Tet", "peer2", 0, at)
	insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateParked, "", "Parked", "P", "peer3", 0, at)
	insertDashboardTestJob(t, s, 3, core.SourceLidarr, core.StateCancelled, "", "Cancelled", "C", "peer4", 0, at)

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 20, Now: now,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	got := map[int64]bool{}
	for _, j := range page.Jobs {
		got[j.Job.ID] = true
	}
	if !got[notImported] {
		t.Errorf("NOT_IMPORTED job %d missing from filter=finished, want present", notImported)
	}
	if !got[done] {
		t.Errorf("DONE job %d missing from filter=finished, want present", done)
	}
	if len(got) != 2 {
		t.Errorf("finished returned %d jobs (%v), want exactly the DONE and NOT_IMPORTED ones — PARKED and CANCELLED must stay out", len(got), got)
	}
}

// A NOT_IMPORTED job must render as its own status, never collapsed into
// 'done' (nothing was imported) or 'failed' (nothing went wrong).
func TestListDashboardJobsNotImportedStatus(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	id := insertDashboardTestJob(t, s, 0, core.SourceManual, core.StateNotImported, "", "Kid A", "Radiohead", "peer1", 0, now.Add(-time.Minute))

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "recent", Dir: "desc", Filter: "all", Source: "all", PageSize: 20, Now: now,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	if len(page.Jobs) != 1 || page.Jobs[0].Job.ID != id {
		t.Fatalf("jobs = %+v, want just %d", page.Jobs, id)
	}
	if page.Jobs[0].Status != "notImported" {
		t.Errorf("status = %q, want %q", page.Jobs[0].Status, "notImported")
	}
	if page.Facets.Status.All != 1 {
		t.Errorf("facets.status.all = %d, want 1 (a NOT_IMPORTED job still counts under ALL, "+
			"even though it has no facet or filter of its own yet — #368)", page.Facets.Status.All)
	}
}

func TestListDashboardJobsSortRecentBreaksTiesById(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := now.Add(-time.Minute)

	low := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDone, "", "Low id", "A", "peer1", 0, finishedAt)
	high := insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateDone, "", "High id", "B", "peer2", 0, finishedAt)

	page, err := s.ListDashboardJobs(context.Background(), DashboardJobsQuery{
		Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 20, Now: now,
	})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	// Equal updated_at: without the id tiebreaker Postgres' order is
	// undefined and the same job could appear on two pages.
	if len(page.Jobs) != 2 || page.Jobs[0].Job.ID != high || page.Jobs[1].Job.ID != low {
		t.Fatalf("order = %+v, want [%d %d] (id descending on an updated_at tie)", page.Jobs, high, low)
	}
}

func TestValidateDashboardJobsQueryRejectsRecentAscendingAndMissingNow(t *testing.T) {
	base := DashboardJobsQuery{Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 5, Now: time.Now()}

	ascending := base
	ascending.Dir = "asc"
	if err := validateDashboardJobsQuery(ascending); err == nil {
		t.Error("dir=asc accepted for sort=recent, want rejected")
	}

	noNow := base
	noNow.Now = time.Time{}
	if err := validateDashboardJobsQuery(noNow); err == nil {
		t.Error("filter=finished accepted with a zero Now, want rejected")
	}

	// A zero Now is fine for every other filter: nothing reads it.
	otherFilter := base
	otherFilter.Filter = "done"
	otherFilter.Now = time.Time{}
	if err := validateDashboardJobsQuery(otherFilter); err != nil {
		t.Errorf("filter=done with zero Now: %v, want nil", err)
	}
}

func TestListDashboardJobsSkipFacetsReturnsPageWithoutCounts(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	first := insertDashboardTestJob(t, s, 1, core.SourceLidarr, core.StateDone, "", "First", "A", "peer1", 0, now.Add(-time.Minute))
	insertDashboardTestJob(t, s, 2, core.SourceLidarr, core.StateDone, "", "Second", "B", "peer2", 0, now.Add(-2*time.Minute))
	insertDashboardTestJob(t, s, 3, core.SourceLidarr, core.StateWanted, "", "Wanted", "C", "", 0, now)

	query := DashboardJobsQuery{
		Page: 0, Sort: "recent", Dir: "desc", Filter: "finished", Source: "all", PageSize: 1, Now: now,
		SkipFacets: true,
	}
	page, err := s.ListDashboardJobs(context.Background(), query)
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	if len(page.Jobs) != 1 || page.Jobs[0].Job.ID != first {
		t.Fatalf("jobs = %+v, want exactly job %d", page.Jobs, first)
	}
	if page.Total != 0 {
		t.Errorf("Total = %d, want 0 when facets are skipped", page.Total)
	}
	if page.Facets != (DashboardJobsFacets{}) {
		t.Errorf("Facets = %+v, want the zero value when skipped", page.Facets)
	}

	// The same query without SkipFacets still reports both.
	query.SkipFacets = false
	withFacets, err := s.ListDashboardJobs(context.Background(), query)
	if err != nil {
		t.Fatalf("ListDashboardJobs without SkipFacets: %v", err)
	}
	if withFacets.Total != 2 {
		t.Errorf("Total = %d, want 2", withFacets.Total)
	}
	if withFacets.Facets.Status.Done != 2 {
		t.Errorf("Facets.Status.Done = %d, want 2", withFacets.Facets.Status.Done)
	}
	// Facets ignore the status filter, so the WANTED job still counts in All.
	if withFacets.Facets.Status.All != 3 {
		t.Errorf("Facets.Status.All = %d, want 3", withFacets.Facets.Status.All)
	}
}

// TestLatestFailureDetails covers issue #310's LatestFailureDetails: which
// job_events rows qualify as a failure explanation, and which of several
// candidates for one job wins.
func TestLatestFailureDetails(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	t.Run("empty input", func(t *testing.T) {
		got, err := s.LatestFailureDetails(ctx, nil)
		if err != nil {
			t.Fatalf("LatestFailureDetails: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Errorf("got %#v, want empty non-nil map", got)
		}
	})

	t.Run("newest event wins", func(t *testing.T) {
		job, err := s.UpsertWantedJob(ctx, 100, now)
		if err != nil {
			t.Fatalf("UpsertWantedJob: %v", err)
		}
		if err := s.AddJobEvent(ctx, job.ID, core.EventAttemptFailed, "older reason", now); err != nil {
			t.Fatalf("AddJobEvent older: %v", err)
		}
		if err := s.AddJobEvent(ctx, job.ID, core.EventImportRejected, "newer reason", now.Add(time.Minute)); err != nil {
			t.Fatalf("AddJobEvent newer: %v", err)
		}
		got, err := s.LatestFailureDetails(ctx, []int64{job.ID})
		if err != nil {
			t.Fatalf("LatestFailureDetails: %v", err)
		}
		if got[job.ID] != "newer reason" {
			t.Errorf("detail = %q, want %q", got[job.ID], "newer reason")
		}
	})

	t.Run("created_at tie broken by id", func(t *testing.T) {
		job, err := s.UpsertWantedJob(ctx, 101, now)
		if err != nil {
			t.Fatalf("UpsertWantedJob: %v", err)
		}
		// Both rows share created_at, mirroring a single pipeline pass that
		// threads one `now` into every recordEvent call — see
		// LatestFailureDetails' id DESC comment. Insertion order (and so id
		// order) is "first" then "second"; "second" must win.
		if err := s.AddJobEvent(ctx, job.ID, core.EventAttemptFailed, "first", now); err != nil {
			t.Fatalf("AddJobEvent first: %v", err)
		}
		if err := s.AddJobEvent(ctx, job.ID, core.EventAttemptFailed, "second", now); err != nil {
			t.Fatalf("AddJobEvent second: %v", err)
		}
		got, err := s.LatestFailureDetails(ctx, []int64{job.ID})
		if err != nil {
			t.Fatalf("LatestFailureDetails: %v", err)
		}
		if got[job.ID] != "second" {
			t.Errorf("detail = %q, want %q (the higher id)", got[job.ID], "second")
		}
	})

	t.Run("empty detail is skipped in favor of an older non-empty one", func(t *testing.T) {
		job, err := s.UpsertWantedJob(ctx, 102, now)
		if err != nil {
			t.Fatalf("UpsertWantedJob: %v", err)
		}
		if err := s.AddJobEvent(ctx, job.ID, core.EventAttemptFailed, "has a reason", now); err != nil {
			t.Fatalf("AddJobEvent non-empty: %v", err)
		}
		if err := s.AddJobEvent(ctx, job.ID, core.EventJobFailed, "", now.Add(time.Minute)); err != nil {
			t.Fatalf("AddJobEvent empty: %v", err)
		}
		got, err := s.LatestFailureDetails(ctx, []int64{job.ID})
		if err != nil {
			t.Fatalf("LatestFailureDetails: %v", err)
		}
		if got[job.ID] != "has a reason" {
			t.Errorf("detail = %q, want %q", got[job.ID], "has a reason")
		}
	})

	// The two subtests below pin the two-tier selection. They replace an
	// earlier "non-allowlisted event is never returned" test, which asserted
	// the allowlist was a hard filter: on a real database that hid the reason
	// for most failed jobs, because the commonest failure (a search returning
	// nothing) records only a 'search' event plus a detail-less job_failed.
	t.Run("a lesser event is used when the job has no explanatory one", func(t *testing.T) {
		job, err := s.UpsertWantedJob(ctx, 103, now)
		if err != nil {
			t.Fatalf("UpsertWantedJob: %v", err)
		}
		// Exactly what Discovery + backoff write for an album nobody shares.
		if err := s.AddJobEvent(ctx, job.ID, core.EventSearch, `searched album, query="X", results=0 candidates=0`, now); err != nil {
			t.Fatalf("AddJobEvent search: %v", err)
		}
		if err := s.AddJobEvent(ctx, job.ID, core.EventJobFailed, "", now.Add(time.Minute)); err != nil {
			t.Fatalf("AddJobEvent job_failed: %v", err)
		}
		got, err := s.LatestFailureDetails(ctx, []int64{job.ID})
		if err != nil {
			t.Fatalf("LatestFailureDetails: %v", err)
		}
		want := `searched album, query="X", results=0 candidates=0`
		if got[job.ID] != want {
			t.Errorf("detail = %q, want %q", got[job.ID], want)
		}
	})

	t.Run("an explanatory event outranks a newer lesser one", func(t *testing.T) {
		job, err := s.UpsertWantedJob(ctx, 107, now)
		if err != nil {
			t.Fatalf("UpsertWantedJob: %v", err)
		}
		if err := s.AddJobEvent(ctx, job.ID, core.EventImportRejected, "lidarr said no", now); err != nil {
			t.Fatalf("AddJobEvent import_rejected: %v", err)
		}
		// Newer, but merely a search summary: recency must not beat rank here.
		if err := s.AddJobEvent(ctx, job.ID, core.EventSearch, "searched album, results=3", now.Add(time.Hour)); err != nil {
			t.Fatalf("AddJobEvent search: %v", err)
		}
		got, err := s.LatestFailureDetails(ctx, []int64{job.ID})
		if err != nil {
			t.Fatalf("LatestFailureDetails: %v", err)
		}
		if got[job.ID] != "lidarr said no" {
			t.Errorf("detail = %q, want %q", got[job.ID], "lidarr said no")
		}
	})

	t.Run("multiple job ids resolve independently, missing ids are absent", func(t *testing.T) {
		jobA, err := s.UpsertWantedJob(ctx, 104, now)
		if err != nil {
			t.Fatalf("UpsertWantedJob A: %v", err)
		}
		jobB, err := s.UpsertWantedJob(ctx, 105, now)
		if err != nil {
			t.Fatalf("UpsertWantedJob B: %v", err)
		}
		jobC, err := s.UpsertWantedJob(ctx, 106, now)
		if err != nil {
			t.Fatalf("UpsertWantedJob C: %v", err)
		}
		if err := s.AddJobEvent(ctx, jobA.ID, core.EventCandidateRejected, "reason A", now); err != nil {
			t.Fatalf("AddJobEvent A: %v", err)
		}
		if err := s.AddJobEvent(ctx, jobB.ID, core.EventImportRejected, "reason B", now); err != nil {
			t.Fatalf("AddJobEvent B: %v", err)
		}
		// jobC gets no event at all.
		got, err := s.LatestFailureDetails(ctx, []int64{jobA.ID, jobB.ID, jobC.ID})
		if err != nil {
			t.Fatalf("LatestFailureDetails: %v", err)
		}
		if got[jobA.ID] != "reason A" {
			t.Errorf("job A detail = %q, want %q", got[jobA.ID], "reason A")
		}
		if got[jobB.ID] != "reason B" {
			t.Errorf("job B detail = %q, want %q", got[jobB.ID], "reason B")
		}
		if _, ok := got[jobC.ID]; ok {
			t.Errorf("job C should be absent, got %q", got[jobC.ID])
		}
	})
}

// TestCountDashboardStatusesMatchesListFacets is the regression test for
// issue #417: /status used to derive its counts itself (and left two of them
// unassigned), so it disagreed with the Jobs page about the word "queued" —
// a live instance reported queued=0 while /api/jobs reported 2. Both surfaces
// now read the same facet query, and this pins that they agree field for
// field over one fixture of every dashboard status.
//
// A second derivation is exactly the drift issue #269 removed; if anyone
// gives CountDashboardStatuses its own counting rule, this fails.
func TestCountDashboardStatusesMatchesListFacets(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	fixtures := []struct {
		id     int64
		state  core.AlbumJobState
		tstate core.TransferState
		peer   string
	}{
		{9101, core.StateDownloading, core.TransferInProgress, "peer_active"},
		{9102, core.StateImporting, "", ""},
		{9103, core.StateWanted, "", ""},
		{9104, core.StateDownloading, core.TransferStalled, "peer_stalled"},
		{9105, core.StateDownloading, core.TransferErrored, "peer_failed"},
		{9106, core.StateParked, "", ""},
		{9107, core.StateOrphaned, "", ""},
		{9108, core.StateDone, "", ""},
		{9109, core.StateSelecting, "", ""},
		{9110, core.StateDownloading, core.TransferPending, "peer_queued"},
		{9111, core.StateDownloading, core.TransferCompleted, "peer_waiting"},
		// Cancelled jobs are outside the facet query's row scope; the
		// counts-only query must use the same scope, so this must not
		// appear in All.
		{9112, core.StateCancelled, "", ""},
	}
	for i, f := range fixtures {
		insertDashboardTestJob(t, s, f.id, core.SourceLidarr, f.state, f.tstate, fmt.Sprintf("Job %d", i), "Artist", f.peer, 0, now.Add(time.Duration(i)*time.Second))
	}

	page, err := s.ListDashboardJobs(ctx, DashboardJobsQuery{Sort: "st", Dir: "asc", Filter: "all", Source: "all"})
	if err != nil {
		t.Fatalf("ListDashboardJobs: %v", err)
	}
	counts, err := s.CountDashboardStatuses(ctx)
	if err != nil {
		t.Fatalf("CountDashboardStatuses: %v", err)
	}
	if counts != page.Facets.Status {
		t.Fatalf("CountDashboardStatuses = %+v, want the /api/jobs facets %+v", counts, page.Facets.Status)
	}

	// Pin the fixture's own shape too, so a change that broke both queries
	// identically would still be caught.
	want := DashboardStatusFacets{
		All: 11, Active: 1, Importing: 1, Queued: 1, Waiting: 1, Selecting: 1,
		Wanted: 1, Stalled: 1, Failed: 1, Parked: 2, Done: 1,
	}
	if counts != want {
		t.Errorf("CountDashboardStatuses = %+v, want %+v", counts, want)
	}
}
