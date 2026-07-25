package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// newActiveCandidate creates a WANTED job, caches a single NEW candidate for
// it, and returns the candidate's id — the pipeline-era equivalent of the
// legacy CreateAttempt helper these tests used before the engine deletion.
func newActiveCandidate(t *testing.T, s *Store, ctx context.Context, lidarrAlbumID int64, username string, score float64, now time.Time) (core.AlbumJob, int64) {
	t.Helper()
	job, err := s.UpsertWantedJob(ctx, lidarrAlbumID, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := s.InsertCandidates(ctx, job.ID, []NewCandidate{{Username: username, Score: score}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	cand, found, err := s.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: found=%v (%v)", found, err)
	}
	return job, cand.ID
}

type jobTransferFixture struct {
	targetJob  core.AlbumJob
	otherJob   core.AlbumJob
	candidate1 int64
	candidate2 int64
	ids        map[core.TransferState]int64
	otherID    int64
	seededAt   time.Time
}

func seedJobTransferFixture(t *testing.T, s *Store, ctx context.Context) jobTransferFixture {
	t.Helper()
	seededAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	targetJob, err := s.UpsertWantedJob(ctx, 1701, seededAt)
	if err != nil {
		t.Fatalf("UpsertWantedJob target: %v", err)
	}
	otherJob, err := s.UpsertWantedJob(ctx, 1702, seededAt)
	if err != nil {
		t.Fatalf("UpsertWantedJob other: %v", err)
	}
	if err := s.AdvanceJobState(ctx, targetJob.ID, core.StateDownloading, seededAt); err != nil {
		t.Fatalf("AdvanceJobState target: %v", err)
	}
	if err := s.AdvanceJobState(ctx, otherJob.ID, core.StateDownloading, seededAt); err != nil {
		t.Fatalf("AdvanceJobState other: %v", err)
	}

	insertCandidate := func(jobID int64, username string) int64 {
		t.Helper()
		var id int64
		if err := s.db.QueryRowContext(ctx,
			`INSERT INTO candidates (album_job_id, username, score, files, state, created_at, updated_at)
			 VALUES ($1, $2, 1, '[]'::jsonb, $3, $4, $4) RETURNING id`,
			jobID, username, string(core.CandidateNew), seededAt).Scan(&id); err != nil {
			t.Fatalf("insert candidate %s: %v", username, err)
		}
		return id
	}
	candidate1 := insertCandidate(targetJob.ID, "peer-one")
	candidate2 := insertCandidate(targetJob.ID, "peer-two")
	otherCandidate := insertCandidate(otherJob.ID, "peer-other")

	insertTransfer := func(candidateID int64, username string, state core.TransferState) int64 {
		t.Helper()
		var id int64
		filename := string(state) + "-" + username + ".flac"
		if err := s.db.QueryRowContext(ctx,
			`INSERT INTO transfers (candidate_id, slskd_id, username, filename, state, deadline, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			candidateID, "remote-"+filename, username, filename, string(state), seededAt.Add(time.Hour), seededAt).Scan(&id); err != nil {
			t.Fatalf("insert %s transfer: %v", state, err)
		}
		return id
	}

	ids := make(map[core.TransferState]int64)
	ids[core.TransferPending] = insertTransfer(candidate1, "peer-one", core.TransferPending)
	ids[core.TransferQueued] = insertTransfer(candidate1, "peer-one", core.TransferQueued)
	ids[core.TransferCompleted] = insertTransfer(candidate1, "peer-one", core.TransferCompleted)
	ids[core.TransferErrored] = insertTransfer(candidate1, "peer-one", core.TransferErrored)
	ids[core.TransferInProgress] = insertTransfer(candidate2, "peer-two", core.TransferInProgress)
	ids[core.TransferStalled] = insertTransfer(candidate2, "peer-two", core.TransferStalled)
	ids[core.TransferCancelled] = insertTransfer(candidate2, "peer-two", core.TransferCancelled)
	otherID := insertTransfer(otherCandidate, "peer-other", core.TransferQueued)

	return jobTransferFixture{
		targetJob: targetJob, otherJob: otherJob,
		candidate1: candidate1, candidate2: candidate2,
		ids: ids, otherID: otherID, seededAt: seededAt,
	}
}

func TestActiveTransfersForJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fixture := seedJobTransferFixture(t, s, ctx)

	transfers, err := s.ActiveTransfersForJob(ctx, fixture.targetJob.ID)
	if err != nil {
		t.Fatalf("ActiveTransfersForJob: %v", err)
	}
	wantIDs := []int64{
		fixture.ids[core.TransferPending],
		fixture.ids[core.TransferQueued],
		fixture.ids[core.TransferInProgress],
		fixture.ids[core.TransferStalled],
	}
	if len(transfers) != len(wantIDs) {
		t.Fatalf("ActiveTransfersForJob returned %d rows, want %d: %+v", len(transfers), len(wantIDs), transfers)
	}
	for i, transfer := range transfers {
		if transfer.ID != wantIDs[i] {
			t.Errorf("transfer[%d].ID = %d, want %d", i, transfer.ID, wantIDs[i])
		}
		if transfer.CandidateID != fixture.candidate1 && transfer.CandidateID != fixture.candidate2 {
			t.Errorf("transfer[%d] belongs to unrelated candidate %d", i, transfer.CandidateID)
		}
	}
}

func TestCancelJobCancelsAllLiveTransfersAtomically(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fixture := seedJobTransferFixture(t, s, ctx)
	cancelledAt := fixture.seededAt.Add(45 * time.Minute)

	ok, err := s.CancelJob(ctx, fixture.targetJob.ID, cancelledAt)
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if !ok {
		t.Fatal("CancelJob returned false for an existing job")
	}

	type transferResult struct {
		state     core.TransferState
		updatedAt time.Time
	}
	results := make(map[int64]transferResult)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, state, updated_at FROM transfers
		 WHERE candidate_id IN ($1, $2) OR id = $3 ORDER BY id`,
		fixture.candidate1, fixture.candidate2, fixture.otherID)
	if err != nil {
		t.Fatalf("read transfers: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var state string
		var updatedAt time.Time
		if err := rows.Scan(&id, &state, &updatedAt); err != nil {
			t.Fatalf("scan transfer: %v", err)
		}
		results[id] = transferResult{state: core.TransferState(state), updatedAt: updatedAt}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate transfers: %v", err)
	}

	for _, state := range []core.TransferState{
		core.TransferPending, core.TransferQueued, core.TransferInProgress, core.TransferStalled,
	} {
		got := results[fixture.ids[state]]
		if got.state != core.TransferCancelled || !got.updatedAt.Equal(cancelledAt) {
			t.Errorf("formerly %s transfer = state %s updated %v, want CANCELLED at %v",
				state, got.state, got.updatedAt, cancelledAt)
		}
	}
	for _, state := range []core.TransferState{
		core.TransferCompleted, core.TransferErrored, core.TransferCancelled,
	} {
		got := results[fixture.ids[state]]
		if got.state != state || !got.updatedAt.Equal(fixture.seededAt) {
			t.Errorf("terminal %s transfer changed to state %s updated %v", state, got.state, got.updatedAt)
		}
	}
	other := results[fixture.otherID]
	if other.state != core.TransferQueued || !other.updatedAt.Equal(fixture.seededAt) {
		t.Errorf("unrelated transfer changed: state %s updated %v", other.state, other.updatedAt)
	}

	var targetState, otherState string
	var targetUpdated, otherUpdated time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT state, updated_at FROM album_jobs WHERE id = $1`, fixture.targetJob.ID).Scan(&targetState, &targetUpdated); err != nil {
		t.Fatalf("read target job: %v", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT state, updated_at FROM album_jobs WHERE id = $1`, fixture.otherJob.ID).Scan(&otherState, &otherUpdated); err != nil {
		t.Fatalf("read other job: %v", err)
	}
	if core.AlbumJobState(targetState) != core.StateCancelled || !targetUpdated.Equal(cancelledAt) {
		t.Errorf("target job = state %s updated %v, want CANCELLED at %v", targetState, targetUpdated, cancelledAt)
	}
	if core.AlbumJobState(otherState) != core.StateDownloading || !otherUpdated.Equal(fixture.seededAt) {
		t.Errorf("unrelated job changed: state %s updated %v", otherState, otherUpdated)
	}
}

func TestCancelJobNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fixture := seedJobTransferFixture(t, s, ctx)

	ok, err := s.CancelJob(ctx, 999999, fixture.seededAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if ok {
		t.Fatal("CancelJob returned true for an unknown job")
	}
	var state string
	var updatedAt time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT state, updated_at FROM transfers WHERE id = $1`, fixture.otherID).Scan(&state, &updatedAt); err != nil {
		t.Fatalf("read unrelated transfer: %v", err)
	}
	if core.TransferState(state) != core.TransferQueued || !updatedAt.Equal(fixture.seededAt) {
		t.Errorf("unknown-job cancel changed unrelated transfer: state %s updated %v", state, updatedAt)
	}
}

func TestWriteAheadEnqueueAndRecover(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	_, candidateID := newActiveCandidate(t, s, ctx, 42, "bob", 1.5, now)

	// Step 1 of write-ahead: intent persisted, no slskd id yet.
	deadline := now.Add(30 * time.Minute)
	tid, err := s.RecordEnqueueIntent(ctx, candidateID, "bob", "album/01.flac", deadline, now)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}

	// Simulate a crash before AttachTransferID: recover by fallback key.
	tr, found, err := s.FindTransferByFallback(ctx, candidateID, "bob", "album/01.flac")
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
	tr2, _, _ := s.FindTransferByFallback(ctx, candidateID, "bob", "album/01.flac")
	if tr2.SlskdID != "slskd-guid-1" {
		t.Errorf("slskd_id = %q, want slskd-guid-1", tr2.SlskdID)
	}
}

func TestTransfersPastDeadline(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	_, candidateID := newActiveCandidate(t, s, ctx, 1, "bob", 1.0, now)
	// Deadline already in the past.
	_, _ = s.RecordEnqueueIntent(ctx, candidateID, "bob", "f.flac", now.Add(-time.Minute), now)

	overdue, err := s.TransfersPastDeadline(ctx, now)
	if err != nil {
		t.Fatalf("TransfersPastDeadline: %v", err)
	}
	if len(overdue) != 1 {
		t.Fatalf("expected 1 overdue transfer, got %d", len(overdue))
	}
}

func TestRecordEnqueueIntentIsConflictSafeWithinCandidate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	_, a1 := newActiveCandidate(t, s, ctx, 300, "bob", 1.0, now)

	id1, err := s.RecordEnqueueIntent(ctx, a1, "bob", "same.flac", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("first intent: %v", err)
	}
	id2, err := s.RecordEnqueueIntent(ctx, a1, "bob", "same.flac", now.Add(2*time.Hour), now)
	if err != nil {
		t.Fatalf("second intent (conflict) errored: %v", err)
	}
	if id1 != id2 {
		t.Errorf("expected same transfer row within one candidate, got %d and %d", id1, id2)
	}
}

func TestRecordEnqueueIntentAllowsTerminalHistoryReuseWithoutMovingOwnership(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	_, a1 := newActiveCandidate(t, s, ctx, 301, "bob", 1.0, now)
	_, a2 := newActiveCandidate(t, s, ctx, 302, "bob", 1.0, now)

	id1, err := s.RecordEnqueueIntent(ctx, a1, "bob", "same.flac", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("first candidate intent: %v", err)
	}
	if err := s.UpdateTransferProgress(ctx, id1, core.TransferCompleted, 10, 10, now); err != nil {
		t.Fatalf("complete first candidate transfer: %v", err)
	}
	if _, found, err := s.FindTransferByFallback(ctx, a1, "bob", "same.flac"); err != nil || found {
		t.Fatalf("terminal transfer must not participate in fallback: found=%v err=%v", found, err)
	}

	id2, err := s.RecordEnqueueIntent(ctx, a2, "bob", "same.flac", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("reuse terminal remote key for second candidate: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("different candidates unexpectedly share transfer row %d", id1)
	}
	for _, tc := range []struct {
		candidateID int64
		transferID  int64
	}{{a1, id1}, {a2, id2}} {
		transfers, err := s.TransfersForCandidate(ctx, tc.candidateID)
		if err != nil || len(transfers) != 1 {
			t.Fatalf("candidate %d transfers: count=%d err=%v", tc.candidateID, len(transfers), err)
		}
		if transfers[0].ID != tc.transferID || transfers[0].CandidateID != tc.candidateID {
			t.Errorf("candidate %d ownership moved: %+v", tc.candidateID, transfers[0])
		}
	}
}

func TestRetryTransferAccumulatesAndSurvivesResend(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	_, a := newActiveCandidate(t, s, ctx, 500, "bob", 1.0, now)

	only := func() core.Transfer {
		t.Helper()
		trs, err := s.TransfersForCandidate(ctx, a)
		if err != nil || len(trs) != 1 {
			t.Fatalf("TransfersForCandidate: %v (n=%d)", err, len(trs))
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

// last_progress_at must track real byte progress, not reconcile frequency: it is
// stamped when the download starts (QUEUED→IN_PROGRESS) and only moves forward
// when the byte counter actually grows, so a stalled transfer keeps an old
// timestamp and can be detected.
func TestUpdateTransferProgressLastProgressAtTracksBytes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	_, a := newActiveCandidate(t, s, ctx, 900, "bob", 1.0, t0)
	tid, _ := s.RecordEnqueueIntent(ctx, a, "bob", "p.flac", t0.Add(time.Hour), t0)

	read := func() core.Transfer {
		t.Helper()
		tr, found, err := s.FindTransferByFallback(ctx, a, "bob", "p.flac")
		if err != nil || !found {
			t.Fatalf("FindTransferByFallback found=%v err=%v", found, err)
		}
		return tr
	}

	// A freshly enqueued transfer has no progress timestamp yet.
	if read().LastProgressAt != nil {
		t.Fatalf("last_progress_at should be NULL before the transfer starts")
	}

	// QUEUED→IN_PROGRESS: the stall clock starts even without bytes yet.
	tStart := t0.Add(time.Minute)
	if err := s.UpdateTransferProgress(ctx, tid, core.TransferInProgress, 0, 100, tStart); err != nil {
		t.Fatalf("UpdateTransferProgress start: %v", err)
	}
	if got := read().LastProgressAt; got == nil || !got.Equal(tStart) {
		t.Fatalf("last_progress_at after start = %v, want %v", got, tStart)
	}

	// Same byte count on a later poll: timestamp must not move (a stall).
	tStall := tStart.Add(5 * time.Minute)
	if err := s.UpdateTransferProgress(ctx, tid, core.TransferInProgress, 0, 100, tStall); err != nil {
		t.Fatalf("UpdateTransferProgress unchanged: %v", err)
	}
	if got := read().LastProgressAt; got == nil || !got.Equal(tStart) {
		t.Fatalf("last_progress_at with unchanged bytes = %v, want %v (unmoved)", got, tStart)
	}

	// Bytes increased: the timestamp advances.
	tProgress := tStall.Add(5 * time.Minute)
	if err := s.UpdateTransferProgress(ctx, tid, core.TransferInProgress, 50, 100, tProgress); err != nil {
		t.Fatalf("UpdateTransferProgress progress: %v", err)
	}
	if got := read().LastProgressAt; got == nil || !got.Equal(tProgress) {
		t.Fatalf("last_progress_at after byte progress = %v, want %v", got, tProgress)
	}
}

// A stall-retried transfer must get a FRESH stall clock on its re-attempt:
// RetryTransfer clears last_progress_at, so the first IN_PROGRESS observation
// of the re-sent transfer (still at 0 bytes) stamps a new timestamp instead of
// keeping the already-expired one — which would re-trip the stall branch
// immediately and burn the retry budget without a genuine retry.
func TestRetryTransferResetsStallClock(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	_, a := newActiveCandidate(t, s, ctx, 901, "bob", 1.0, t0)
	tid, _ := s.RecordEnqueueIntent(ctx, a, "bob", "r.flac", t0.Add(time.Hour), t0)

	read := func() core.Transfer {
		t.Helper()
		tr, found, err := s.FindTransferByFallback(ctx, a, "bob", "r.flac")
		if err != nil || !found {
			t.Fatalf("FindTransferByFallback found=%v err=%v", found, err)
		}
		return tr
	}

	// The transfer starts and makes some progress, then stalls.
	if err := s.UpdateTransferProgress(ctx, tid, core.TransferInProgress, 40, 100, t0.Add(time.Minute)); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}

	// The reconciler detects the stall and retries the transfer.
	tRetry := t0.Add(2 * time.Hour)
	if err := s.RetryTransfer(ctx, tid, tRetry); err != nil {
		t.Fatalf("RetryTransfer: %v", err)
	}
	if got := read().LastProgressAt; got != nil {
		t.Fatalf("last_progress_at after retry = %v, want NULL (fresh stall clock)", got)
	}

	// topUpAttempt re-enqueues it and the download starts again at 0 bytes: the
	// stall clock must restart from now, not from before the retry.
	if _, err := s.RecordEnqueueIntent(ctx, a, "bob", "r.flac", tRetry.Add(time.Hour), tRetry); err != nil {
		t.Fatalf("re-enqueue RecordEnqueueIntent: %v", err)
	}
	tRestart := tRetry.Add(time.Minute)
	if err := s.UpdateTransferProgress(ctx, tid, core.TransferInProgress, 0, 100, tRestart); err != nil {
		t.Fatalf("UpdateTransferProgress restart: %v", err)
	}
	if got := read().LastProgressAt; got == nil || !got.Equal(tRestart) {
		t.Fatalf("last_progress_at after restart = %v, want %v", got, tRestart)
	}
}

func TestUpsertWantedJobIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a, _ := s.UpsertWantedJob(ctx, 7, now)
	b, err := s.UpsertWantedJob(ctx, 7, now)
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

	job, err := s.UpsertWantedJob(ctx, 42, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if job.Title != "" || job.ArtistName != "" {
		t.Fatalf("expected empty title/artist before UpdateJobMetadata, got %q / %q", job.Title, job.ArtistName)
	}

	if err := s.UpdateJobMetadata(ctx, job.ID, "Untrue", "Burial", "", 0, now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}

	jobs, err := s.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
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

	job, err := s.UpsertWantedJob(ctx, 50, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}

	if err := s.BackfillJobMetadataIfEmpty(ctx, job.ID, "Title A", "Artist A", "", 0); err != nil {
		t.Fatalf("BackfillJobMetadataIfEmpty: %v", err)
	}

	jobs, err := s.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
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

	job, _ := s.UpsertWantedJob(ctx, 51, now)
	if err := s.UpdateJobMetadata(ctx, job.ID, "Original Title", "Original Artist", "2020-01-01", 7, now); err != nil {
		t.Fatalf("UpdateJobMetadata: %v", err)
	}

	if err := s.BackfillJobMetadataIfEmpty(ctx, job.ID, "New Title", "New Artist", "2025-01-01", 99); err != nil {
		t.Fatalf("BackfillJobMetadataIfEmpty: %v", err)
	}

	jobs, err := s.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	if jobs[0].Title != "Original Title" || jobs[0].ArtistName != "Original Artist" || jobs[0].ReleaseDate != "2020-01-01" || jobs[0].ArtistID != 7 {
		t.Errorf("expected existing metadata preserved, got %q / %q / %q / artist_id %d",
			jobs[0].Title, jobs[0].ArtistName, jobs[0].ReleaseDate, jobs[0].ArtistID)
	}
}

func TestBackfillJobMetadataIfEmptyDoesNotTouchUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createdAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertWantedJob(ctx, 52, createdAt)
	if err := s.AdvanceJobState(ctx, job.ID, core.StateFailed, createdAt); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	if err := s.BackfillJobMetadataIfEmpty(ctx, job.ID, "Legacy Title", "Legacy Artist", "", 0); err != nil {
		t.Fatalf("BackfillJobMetadataIfEmpty: %v", err)
	}

	failed, err := s.RunnableJobsInState(ctx, core.StateFailed, createdAt, 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
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
