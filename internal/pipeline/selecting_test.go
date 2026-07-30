package pipeline

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// newSelectingParams builds SelectingParams over a fresh store-backed
// fixture, with generous defaults each test can override on the returned
// struct before constructing a Selecting.
type barrierAfterTransferSnapshot struct {
	SelectingParams
	afterSnapshot func()
}

func (d *barrierAfterTransferSnapshot) TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error) {
	transfers, err := d.SelectingParams.TransfersForCandidate(ctx, candidateID)
	if err == nil && d.afterSnapshot != nil {
		d.afterSnapshot()
		d.afterSnapshot = nil
	}
	return transfers, err
}

func newSelectingParams(t *testing.T, searcher *fakeSearcher) (SelectingParams, *store.Store) {
	t.Helper()
	st := newBackedStore(t)
	return SelectingParams{
		Store:              st,
		Peers:              searcher,
		MaxActive:          5,
		MaxRetries:         3,
		BackoffBase:        15 * time.Minute,
		BackoffCap:         24 * time.Hour,
		CandidateTTL:       24 * time.Hour,
		MaxInflightPerPeer: 2,
		MaxTransferRetries: 3,
		TransferDeadline:   time.Hour,
		Interval:           30 * time.Second,
		Logger:             slog.New(slog.NewTextHandler(testDiscard{}, nil)),
	}, st
}

func TestSelectingActivatesBestCandidateAndEnqueues(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)

	job, err := st.UpsertWantedJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	cands := []store.NewCandidate{
		{Username: "good1", Score: 2.0, Files: []core.CandidateFile{
			{Filename: "good1/01.flac", Size: 10},
			{Filename: "good1/02.flac", Size: 10},
			{Filename: "good1/03.flac", Size: 10},
		}},
		{Username: "good2", Score: 1.0, Files: []core.CandidateFile{
			{Filename: "good2/01.flac", Size: 10},
		}},
	}
	if err := st.InsertCandidates(ctx, job.ID, cands, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	sel := NewSelecting(p)
	if err := sel.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateDownloading, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job DOWNLOADING, got %+v (%v)", jobs, err)
	}

	active, found, err := st.ActiveCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	if active.Username != "good1" {
		t.Fatalf("expected higher-scoring candidate good1 activated, got %q", active.Username)
	}

	transfers, err := st.TransfersForCandidate(ctx, active.ID)
	if err != nil {
		t.Fatalf("TransfersForCandidate: %v", err)
	}
	if len(transfers) != 3 {
		t.Fatalf("expected 3 PENDING-written transfers for good1's files, got %d", len(transfers))
	}
	var queued, pending int
	for _, tr := range transfers {
		switch tr.State {
		case core.TransferQueued:
			queued++
		case core.TransferPending:
			pending++
		default:
			t.Errorf("unexpected transfer state %q for %s", tr.State, tr.Filename)
		}
	}
	if queued != p.MaxInflightPerPeer {
		t.Errorf("queued transfers = %d, want MaxInflightPerPeer=%d", queued, p.MaxInflightPerPeer)
	}
	if pending != len(transfers)-p.MaxInflightPerPeer {
		t.Errorf("pending transfers = %d, want %d", pending, len(transfers)-p.MaxInflightPerPeer)
	}
	if len(searcher.enqueued) != p.MaxInflightPerPeer {
		t.Errorf("enqueued to fakeSearcher = %d, want MaxInflightPerPeer=%d", len(searcher.enqueued), p.MaxInflightPerPeer)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	found = false
	for _, e := range events {
		if e.Event == core.EventCandidateSelected {
			found = true
		}
	}
	if !found {
		t.Errorf("expected EventCandidateSelected recorded, got events %+v", events)
	}
}

func TestTopUpCandidateStopsOnStalePendingSnapshot(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)
	jobID, candidateID := seedActiveCandidate(t, st, 901, "peer", []core.CandidateFile{{Filename: "album/01.flac", Size: 10}}, now)

	deps := &barrierAfterTransferSnapshot{SelectingParams: p}
	deps.afterSnapshot = func() {
		if _, found, err := st.CancelJob(ctx, jobID, now.Add(time.Second)); err != nil || !found {
			t.Fatalf("CancelJob: found=%v err=%v", found, err)
		}
	}
	sent, err := topUpCandidate(ctx, deps, candidateID, now, 1, 3, time.Hour, p.Logger)
	if err != nil {
		t.Fatalf("topUpCandidate: %v", err)
	}
	if sent != 0 || len(searcher.enqueued) != 0 {
		t.Fatalf("stale top-up sent=%d remote=%v, want no enqueue", sent, searcher.enqueued)
	}
	if got := transferStatesFor(t, st, candidateID)["album/01.flac"].State; got != core.TransferCancelled {
		t.Fatalf("transfer state = %v, want CANCELLED", got)
	}
}

func TestTopUpCandidateCompensatesEnqueueReturningAfterLifecycleBarrier(t *testing.T) {
	for _, tc := range []struct {
		name    string
		barrier func(context.Context, *store.Store, int64, time.Time)
	}{
		{
			name: "cancel",
			barrier: func(ctx context.Context, st *store.Store, jobID int64, now time.Time) {
				if _, found, err := st.CancelJob(ctx, jobID, now); err != nil || !found {
					t.Fatalf("CancelJob: found=%v err=%v", found, err)
				}
			},
		},
		{
			name: "delete",
			barrier: func(ctx context.Context, st *store.Store, jobID int64, now time.Time) {
				if _, found, err := st.PrepareDeleteJob(ctx, jobID, now); err != nil || !found {
					t.Fatalf("PrepareDeleteJob: found=%v err=%v", found, err)
				}
				if deleted, err := st.DeleteJob(ctx, jobID); err != nil || !deleted {
					t.Fatalf("DeleteJob: deleted=%v err=%v", deleted, err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
			searcher := &fakeSearcher{}
			p, st := newSelectingParams(t, searcher)
			jobID, candidateID := seedActiveCandidate(t, st, 902, "peer", []core.CandidateFile{{Filename: "album/01.flac", Size: 10}}, now)
			searcher.enqueueHook = func() { tc.barrier(ctx, st, jobID, now.Add(time.Second)) }

			sent, err := topUpCandidate(ctx, p, candidateID, now, 1, 3, time.Hour, p.Logger)
			if err != nil {
				t.Fatalf("topUpCandidate: %v", err)
			}
			if sent != 0 || len(searcher.enqueued) != 1 {
				t.Fatalf("top-up sent=%d enqueued=%v, want one bounced enqueue", sent, searcher.enqueued)
			}
			wantRemoteID := "slskd-album/01.flac"
			if len(searcher.cancelled) != 1 || searcher.cancelled[0] != wantRemoteID {
				t.Fatalf("compensating cancellations = %v, want [%s]", searcher.cancelled, wantRemoteID)
			}
		})
	}
}

func TestSelectingRespectsMaxActive(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)
	p.MaxActive = 2

	// Fill the cap with two already-DOWNLOADING jobs (their own candidate
	// bookkeeping is irrelevant to ActivateCandidate's cap check, which just
	// counts album_jobs in DOWNLOADING/IMPORTING).
	for _, albumID := range []int64{10, 11} {
		j, err := st.UpsertWantedJob(ctx, albumID, now)
		if err != nil {
			t.Fatalf("UpsertWantedJob: %v", err)
		}
		if err := st.AdvanceJobState(ctx, j.ID, core.StateDownloading, now); err != nil {
			t.Fatalf("AdvanceJobState: %v", err)
		}
	}

	job, err := st.UpsertWantedJob(ctx, 12, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.InsertCandidates(ctx, job.ID, []store.NewCandidate{
		{Username: "alice", Score: 1.0, Files: []core.CandidateFile{{Filename: "a.flac", Size: 1}}},
	}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	cand, found, err := st.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}

	sel := NewSelecting(p)
	if err := sel.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateSelecting, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job to stay SELECTING, got %+v (%v)", jobs, err)
	}
	stillNew, found, err := st.NextNewCandidate(ctx, job.ID)
	if err != nil || !found || stillNew.ID != cand.ID {
		t.Fatalf("expected candidate to stay NEW, got %+v found=%v (%v)", stillNew, found, err)
	}
	if len(searcher.enqueued) != 0 {
		t.Errorf("expected nothing enqueued, got %v", searcher.enqueued)
	}
}

func TestSelectingLiveOwnerConflictDoesNotStarveLaterJob(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)
	p.MaxActive = 3

	seed := func(albumID int64, username, filename string, updatedAt time.Time) (core.AlbumJob, core.Candidate) {
		t.Helper()
		job, err := st.UpsertWantedJob(ctx, albumID, updatedAt)
		if err != nil {
			t.Fatalf("UpsertWantedJob: %v", err)
		}
		if err := st.InsertCandidates(ctx, job.ID, []store.NewCandidate{{
			Username: username, Score: 1,
			Files: []core.CandidateFile{{Filename: filename, Size: 10}},
		}}, updatedAt); err != nil {
			t.Fatalf("InsertCandidates: %v", err)
		}
		if err := st.AdvanceJobState(ctx, job.ID, core.StateSelecting, updatedAt); err != nil {
			t.Fatalf("AdvanceJobState: %v", err)
		}
		cand, found, err := st.NextNewCandidate(ctx, job.ID)
		if err != nil || !found {
			t.Fatalf("NextNewCandidate: found=%v err=%v", found, err)
		}
		return job, cand
	}

	ownerJob, owner := seed(13, "shared", "same.flac", now.Add(-5*time.Minute))
	if activated, _, err := st.ActivateCandidateWithTransfers(ctx, owner.ID, ownerJob.ID, p.MaxActive, now.Add(-5*time.Minute)); err != nil || !activated {
		t.Fatalf("activate live owner: activated=%v err=%v", activated, err)
	}

	// Fill the entire first FIFO query (limit=MaxActive) with conflicts. If
	// skips remain at the head unchanged, laterJob is permanently invisible.
	var blockedJobs []core.AlbumJob
	for i := 0; i < p.MaxActive; i++ {
		job, _ := seed(int64(14+i), "shared", "same.flac", now.Add(time.Duration(-4+i)*time.Minute))
		blockedJobs = append(blockedJobs, job)
	}
	laterJob, _ := seed(17, "other", "other.flac", now.Add(-time.Minute))

	sel := NewSelecting(p)
	if err := sel.Tick(ctx, now); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	if got := jobStateFor(t, st, laterJob.ID); got != core.StateSelecting {
		t.Fatalf("later job state after conflict-only batch = %v, want SELECTING", got)
	}
	if err := sel.Tick(ctx, now.Add(time.Second)); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	for _, blockedJob := range blockedJobs {
		if got := jobStateFor(t, st, blockedJob.ID); got != core.StateSelecting {
			t.Errorf("conflicting job %d state = %v, want SELECTING for retry", blockedJob.ID, got)
		}
	}
	if got := jobStateFor(t, st, laterJob.ID); got != core.StateDownloading {
		t.Errorf("later unrelated job state = %v, want DOWNLOADING", got)
	}
	if len(searcher.enqueued) != 1 || searcher.enqueued[0] != "other.flac" {
		t.Errorf("enqueued = %v, want only later unrelated file", searcher.enqueued)
	}
}

func TestSelectingExhaustionBacksOffToWanted(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)

	job, err := st.UpsertWantedJob(ctx, 20, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.InsertCandidates(ctx, job.ID, []store.NewCandidate{
		{Username: "alice", Score: 1.0, Files: []core.CandidateFile{{Filename: "a.flac", Size: 1}}},
	}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	// Exhaust the only candidate: fail it so NextNewCandidate finds nothing.
	cand, found, err := st.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}
	if err := st.FailCandidate(ctx, cand.ID, "timeout", now); err != nil {
		t.Fatalf("FailCandidate: %v", err)
	}

	sel := NewSelecting(p)
	if err := sel.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateWanted, now.Add(2*time.Hour), 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job back to WANTED, got %+v (%v)", jobs, err)
	}
	if jobs[0].Retries != 1 {
		t.Errorf("Retries = %d, want 1", jobs[0].Retries)
	}
	if jobs[0].NotBefore == nil {
		t.Errorf("NotBefore = nil, want set after backoff")
	}
	if _, found, err := st.NextNewCandidate(ctx, job.ID); err != nil || found {
		t.Fatalf("expected candidates deleted, found=%v (%v)", found, err)
	}
	if trs, err := st.TransfersForCandidate(ctx, cand.ID); err != nil || len(trs) != 0 {
		t.Fatalf("expected candidate's transfers deleted too, got %d (%v)", len(trs), err)
	}
}

func TestSelectingExhaustionAtMaxRetriesFails(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)
	p.MaxRetries = 1 // job.Retries(0)+1 >= 1 -> FAILED on first exhaustion

	job, err := st.UpsertWantedJob(ctx, 30, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.InsertCandidates(ctx, job.ID, []store.NewCandidate{
		{Username: "alice", Score: 1.0, Files: []core.CandidateFile{{Filename: "a.flac", Size: 1}}},
	}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	cand, found, err := st.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}
	if err := st.FailCandidate(ctx, cand.ID, "timeout", now); err != nil {
		t.Fatalf("FailCandidate: %v", err)
	}

	sel := NewSelecting(p)
	if err := sel.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateFailed, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job FAILED, got %+v (%v)", jobs, err)
	}
}

// seedExhaustedJobWithLeftovers builds a SELECTING job whose single candidate
// has already been activated (so it has transfers naming a remote folder) and
// then failed, plus that folder sitting in completeDir as leftover files. One
// Tick over it takes the exhaustion path.
func seedExhaustedJobWithLeftovers(t *testing.T, ctx context.Context, st *store.Store, p SelectingParams, lidarrID int64, now time.Time) (core.AlbumJob, string) {
	t.Helper()
	const leaf = "Leftover Album (2014)"

	job, err := st.UpsertWantedJob(ctx, lidarrID, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.InsertCandidates(ctx, job.ID, []store.NewCandidate{{
		Username: "alice",
		Score:    1.0,
		Files: []core.CandidateFile{
			{Filename: `music\Sia\` + leaf + `\01.flac`, Size: 1},
			{Filename: `music\Sia\` + leaf + `\02.flac`, Size: 1},
		},
	}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	cand, found, err := st.NextNewCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("NextNewCandidate: %v found=%v", err, found)
	}
	// Activation is what writes the transfer rows quarantineLeftovers reads.
	activated, _, err := st.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, p.MaxActive, now)
	if err != nil || !activated {
		t.Fatalf("ActivateCandidateWithTransfers: %v activated=%v", err, activated)
	}
	if err := st.FailCandidate(ctx, cand.ID, "timeout", now); err != nil {
		t.Fatalf("FailCandidate: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState back to SELECTING: %v", err)
	}

	folder := filepath.Join(p.CompleteDir, leaf)
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(folder, "01.flac.part"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return job, folder
}

func TestSelectingTerminalFailureQuarantinesLeftovers(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	p, st := newSelectingParams(t, &fakeSearcher{})
	p.MaxRetries = 1 // job.Retries(0)+1 >= 1 -> FAILED on first exhaustion
	p.CompleteDir = t.TempDir()

	job, folder := seedExhaustedJobWithLeftovers(t, ctx, st, p, 40, now)

	if err := NewSelecting(p).Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateFailed, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job FAILED, got %+v (%v)", jobs, err)
	}
	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Errorf("expected leftovers gone from the download root, stat err = %v", err)
	}
	quarantined := filepath.Join(p.CompleteDir, quarantineDirName, filepath.Base(folder), "01.flac.part")
	if _, err := os.Stat(quarantined); err != nil {
		t.Errorf("expected leftovers under %s, stat err = %v", quarantineDirName, err)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Event == core.EventQuarantined {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a %q job event, got %+v", core.EventQuarantined, events)
	}
}

func TestSelectingNonTerminalExhaustionLeavesFilesAlone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	p, st := newSelectingParams(t, &fakeSearcher{})
	p.MaxRetries = 3 // job.Retries(0)+1 < 3 -> back off, not terminal
	p.CompleteDir = t.TempDir()

	job, folder := seedExhaustedJobWithLeftovers(t, ctx, st, p, 41, now)

	if err := NewSelecting(p).Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateWanted, now.Add(2*time.Hour), 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job back to WANTED, got %+v (%v)", jobs, err)
	}
	if _, err := os.Stat(folder); err != nil {
		t.Errorf("expected leftovers left in place for the next attempt, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.CompleteDir, quarantineDirName)); !os.IsNotExist(err) {
		t.Errorf("expected no quarantine dir on a non-terminal failure, stat err = %v", err)
	}
}

func TestSelectingExpiresStaleCache(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	cachedAt := now.Add(-25 * time.Hour)

	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)
	p.CandidateTTL = 24 * time.Hour

	job, err := st.UpsertWantedJob(ctx, 40, cachedAt)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.InsertCandidates(ctx, job.ID, []store.NewCandidate{
		{Username: "alice", Score: 1.0, Files: []core.CandidateFile{{Filename: "a.flac", Size: 1}}},
	}, cachedAt); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateSelecting, cachedAt); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	// Bump retries directly to a nonzero value so the test can assert TTL
	// expiry leaves it UNCHANGED (as opposed to exhaustion, which increments
	// it) - SetJobBackoff is the only store method that sets retries without
	// also touching the candidate cache.
	if err := st.SetJobBackoff(ctx, job.ID, 2, cachedAt, cachedAt); err != nil {
		t.Fatalf("SetJobBackoff: %v", err)
	}

	sel := NewSelecting(p)
	if err := sel.Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateWanted, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job back to WANTED, got %+v (%v)", jobs, err)
	}
	if jobs[0].Retries != 2 {
		t.Errorf("Retries = %d, want 2 (unchanged by TTL expiry)", jobs[0].Retries)
	}
	if jobs[0].NotBefore != nil {
		t.Errorf("NotBefore = %v, want nil after TTL expiry reset", jobs[0].NotBefore)
	}
	if _, found, err := st.NextNewCandidate(ctx, job.ID); err != nil || found {
		t.Fatalf("expected candidates deleted after TTL expiry, found=%v (%v)", found, err)
	}
	if len(searcher.enqueued) != 0 {
		t.Errorf("expected nothing enqueued for an expired candidate, got %v", searcher.enqueued)
	}
}
