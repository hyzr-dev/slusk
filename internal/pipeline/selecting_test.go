package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/store"
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
	sent, err := topUpCandidate(ctx, deps, jobID, candidateID, now, 1, 3, time.Hour, p.Logger)
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

			sent, err := topUpCandidate(ctx, p, jobID, candidateID, now, 1, 3, time.Hour, p.Logger)
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

	ownerSeededAt := now.Add(-5 * time.Minute)
	ownerJob, owner := seed(13, "shared", "same.flac", ownerSeededAt)
	if activated, _, err := st.ActivateCandidateWithTransfers(ctx, owner.ID, ownerJob.ID, p.MaxActive, ownerSeededAt.Add(time.Hour), ownerSeededAt); err != nil || !activated {
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
	registerSeedFolders(t, st, job.ID, cand.Files, now)
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
	registerSeedFolders(t, st, job.ID, cand.Files, now)
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

// Writing a detail on job_failed (issue #318) is only half the fix: the
// dashboard's FAILED JOBS panel reads whichever explanatory event
// LatestFailureDetails ranks first, and one pipeline pass shares a single now,
// so candidate_rejected and job_failed are separated by insertion order alone.
// This asserts the whole path rather than the store call, because a unit test
// over a fake store cannot see the ranking at all - the ordering bug it guards
// against passed every such test.
func TestSelectingTerminalFailureDetailWinsOverCandidateRejected(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)
	p.MaxRetries = 1 // job.Retries(0)+1 >= 1 -> FAILED on first exhaustion

	job, err := st.UpsertWantedJob(ctx, 31, now)
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

	if err := NewSelecting(p).Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	details, err := st.LatestFailureDetails(ctx, []int64{job.ID})
	if err != nil {
		t.Fatalf("LatestFailureDetails: %v", err)
	}
	got, ok := details[job.ID]
	if !ok {
		t.Fatal("no failure detail for the failed job, want the job_failed reason")
	}
	if !strings.Contains(got, "gave up") {
		t.Errorf("detail = %q, want the terminal job_failed reason, not an earlier event", got)
	}
	if !strings.Contains(got, "tried and failed to download") {
		t.Errorf("detail = %q, want it to say why the job gave up", got)
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
	activated, _, err := st.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, p.MaxActive, now.Add(time.Hour), now)
	if err != nil || !activated {
		t.Fatalf("ActivateCandidateWithTransfers: %v activated=%v", err, activated)
	}
	registerSeedFolders(t, st, job.ID, cand.Files, now)
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

// TestSelectingNonTerminalExhaustionKeepsCandidateFoldersWhenResetBounces is
// what actually pins selectJob's `if failed` guard.
//
// TestSelectingNonTerminalExhaustionLeavesFilesAlone above cannot: on the
// ordinary non-terminal path ResetJobToWanted has already deleted the job's
// candidates and transfers by the time quarantineLeftovers would run, so it
// finds no folder to move and dropping the guard changes nothing observable.
//
// The state where the guard does real work is the one ResetJobToWanted's
// from-guard exists for: WantedSync cancels the job between Tick reading its
// SELECTING batch and selectJob acting on that snapshot. The UPDATE then
// matches no row, the candidates and transfers survive, and an unguarded
// quarantineLeftovers would move a merely-cancelled job's files into .failed.
// Tick offers no interleaving point, so the test drives selectJob with the
// stale snapshot Tick would have been holding.
func TestSelectingNonTerminalExhaustionKeepsCandidateFoldersWhenResetBounces(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	p, st := newSelectingParams(t, &fakeSearcher{})
	p.MaxRetries = 3 // job.Retries(0)+1 < 3 -> back off, not terminal
	p.CompleteDir = t.TempDir()

	job, folder := seedExhaustedJobWithLeftovers(t, ctx, st, p, 42, now)

	// WantedSync's own cancellation path, run against a wanted set the album is
	// no longer in - exactly what races Tick in production.
	if n, err := st.CancelJobsNotWanted(ctx, []int64{9999}, now); err != nil || n != 1 {
		t.Fatalf("CancelJobsNotWanted: n=%d err=%v, want n=1", n, err)
	}

	// job is the SELECTING snapshot Tick read before the cancellation.
	if _, err := NewSelecting(p).selectJob(ctx, job, now); err != nil {
		t.Fatalf("selectJob: %v", err)
	}

	// The premise: ResetJobToWanted bounced, so the candidate and its transfers
	// are still there and quarantineLeftovers would find a folder to move.
	cands, err := st.CandidatesForJob(ctx, job.ID)
	if err != nil || len(cands) != 1 {
		t.Fatalf("CandidatesForJob = %d candidates (%v), want 1 surviving the bounced reset", len(cands), err)
	}
	transfers, err := st.TransfersForCandidate(ctx, cands[0].ID)
	if err != nil || len(transfers) == 0 {
		t.Fatalf("TransfersForCandidate = %d (%v), want the candidate's transfers to survive", len(transfers), err)
	}

	if _, err := os.Stat(folder); err != nil {
		t.Errorf("expected leftovers left in place for a non-terminal failure, stat err = %v", err)
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

// TestSelectingManualJobExhaustionFailsOnFirstFailure covers issue #347: a
// manual job's candidate failing must never send it back to WANTED for a
// re-search - the user picked that peer deliberately. It reaches SELECTING
// the same way production does: CreateManualJob straight into DOWNLOADING,
// then FailCandidateAndAdvance bounces it to SELECTING with its only
// candidate FAILED, exactly like a real transfer failure
// (downloading.go:629) or import failure (importing.go:173,464) would.
func TestSelectingManualJobExhaustionFailsOnFirstFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	p, st := newSelectingParams(t, &fakeSearcher{})
	p.MaxRetries = 3 // would NOT be terminal for a lidarr job at this retry count

	job, err := st.CreateManualJob(ctx, "Album", "Artist", "alice", "",
		[]store.ManualJobFile{{Filename: "a.flac", Size: 1}}, now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	cand, found, err := st.ActiveCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	if _, err := st.FailCandidateAndAdvance(ctx, cand.ID, job.ID, "transfer failed", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("FailCandidateAndAdvance: %v", err)
	}

	if err := NewSelecting(p).Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateFailed, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job FAILED on first failure, got %+v (%v)", jobs, err)
	}
	if jobs[0].Retries != 0 {
		t.Errorf("Retries = %d, want 0 (unconsumed - a manual job never backs off)", jobs[0].Retries)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	found = false
	for _, e := range events {
		if e.Event == core.EventJobFailed {
			found = true
		}
	}
	if !found {
		t.Errorf("expected EventJobFailed recorded, got %+v", events)
	}

	if wanted, err := st.RunnableJobsInState(ctx, core.StateWanted, now, 10); err != nil || len(wanted) != 0 {
		t.Fatalf("expected job never resurrected to WANTED, got %+v (%v)", wanted, err)
	}
}

// TestSelectingManualJobExhaustionQuarantinesLeftovers is the manual-job
// counterpart to TestSelectingTerminalFailureQuarantinesLeftovers: the same
// post-mortem must run on the manual-job terminal path too.
func TestSelectingManualJobExhaustionQuarantinesLeftovers(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	const leaf = "Leftover Manual Album"

	p, st := newSelectingParams(t, &fakeSearcher{})
	p.CompleteDir = t.TempDir()

	job, err := st.CreateManualJob(ctx, "Album", "Artist", "alice", "", []store.ManualJobFile{
		{Filename: `music\` + leaf + `\01.flac`, Size: 1},
	}, now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	cand, found, err := st.ActiveCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	registerSeedFolders(t, st, job.ID, cand.Files, now)
	if _, err := st.FailCandidateAndAdvance(ctx, cand.ID, job.ID, "transfer failed", core.StateDownloading, core.StateSelecting, now); err != nil {
		t.Fatalf("FailCandidateAndAdvance: %v", err)
	}

	folder := filepath.Join(p.CompleteDir, leaf)
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(folder, "01.flac.part"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := NewSelecting(p).Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateFailed, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected job FAILED, got %+v (%v)", jobs, err)
	}
	quarantined := filepath.Join(p.CompleteDir, quarantineDirName, leaf, "01.flac.part")
	if _, err := os.Stat(quarantined); err != nil {
		t.Errorf("expected leftovers quarantined for a manual job too, stat err = %v", err)
	}
}

// TestSelectingManualJobIgnoresCandidateTTL covers the CandidateTTL bypass
// (issue #347): a manual job retried via RetryManualJob revives a candidate
// whose created_at is by definition old (it dates back to the original
// manual job creation), which would otherwise trip CandidateTTL. The
// candidate must still be tried, not discarded for a re-search that does not
// exist for a manual job.
func TestSelectingManualJobIgnoresCandidateTTL(t *testing.T) {
	ctx := context.Background()
	createdAt := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	now := createdAt.Add(25 * time.Hour) // past the 24h CandidateTTL below

	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)
	p.CandidateTTL = 24 * time.Hour

	job, err := st.CreateManualJob(ctx, "Album", "Artist", "alice", "",
		[]store.ManualJobFile{{Filename: "a.flac", Size: 1}}, createdAt.Add(time.Hour), createdAt)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	cand, found, err := st.ActiveCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	if _, err := st.FailCandidateAndAdvance(ctx, cand.ID, job.ID, "transfer failed", core.StateDownloading, core.StateSelecting, createdAt); err != nil {
		t.Fatalf("FailCandidateAndAdvance: %v", err)
	}
	// The manual-job exhaustion branch fails the job on this SELECTING tick
	// (no NEW candidate left); only a FAILED job is retryable, matching the
	// real dashboard flow.
	if err := NewSelecting(p).Tick(ctx, createdAt); err != nil {
		t.Fatalf("Tick (initial failure): %v", err)
	}
	ok, err := st.RetryManualJob(ctx, job.ID, createdAt)
	if err != nil || !ok {
		t.Fatalf("RetryManualJob: %v ok=%v", err, ok)
	}

	if err := NewSelecting(p).Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	jobs, err := st.RunnableJobsInState(ctx, core.StateDownloading, now, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("expected manual job activated despite an expired candidate, got %+v (%v)", jobs, err)
	}
	if len(searcher.enqueued) == 0 {
		t.Errorf("expected the revived candidate enqueued, got none")
	}
}

// TestSelectingRegistersDownloadFolderOnce pins the registration seam (issue
// #314): topUpCandidate is the single chokepoint every enqueue on either
// backend passes through, and it registers the local folder each file is
// written into. Three files in one remote folder must leave exactly one row —
// the idempotence the UNIQUE (album_job_id, leaf) constraint buys.
func TestSelectingRegistersDownloadFolderOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	p, st := newSelectingParams(t, &fakeSearcher{})
	p.MaxInflightPerPeer = 3 // send all three files in one go

	job, err := st.UpsertWantedJob(ctx, 77, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.InsertCandidates(ctx, job.ID, []store.NewCandidate{{
		Username: "alice", Score: 1.0, Files: []core.CandidateFile{
			{Filename: `music\Sia\1000 Forms of Fear (2014)\01.flac`, Size: 10},
			{Filename: `music\Sia\1000 Forms of Fear (2014)\02.flac`, Size: 10},
			{Filename: `music\Sia\1000 Forms of Fear (2014)\03.flac`, Size: 10},
		},
	}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}

	if err := NewSelecting(p).Tick(ctx, now); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	leaves, err := st.DownloadFoldersForJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("DownloadFoldersForJob: %v", err)
	}
	if len(leaves) != 1 || leaves[0] != "1000 Forms of Fear (2014)" {
		t.Errorf("registered leaves = %v, want exactly [1000 Forms of Fear (2014)]", leaves)
	}
}

// TestRegisteredLeafMatchesAlbumFolder locks the register against the import
// scan: the leaf topUpCandidate registers must be the same folder AlbumFolder
// tells Lidarr to scan, or cleanup would be pointed at a directory nothing ever
// downloaded into. A filename with no usable folder registers nothing, matching
// AlbumFolder's fallback to the download root.
func TestRegisteredLeafMatchesAlbumFolder(t *testing.T) {
	const completeDir = "/music/dl"
	for _, f := range []string{
		`Music\Artist - Album\01 Track.flac`,
		"Music/Artist - Album/02 Track.flac",
		`@@abcd\Shared\Some Album [2020]\1-01 Intro.mp3`,
		"single-level/file.flac",
		"noleaf.flac",
		`..\track.flac`,
	} {
		leaf := commonLeaf([]string{f})
		folder := AlbumFolder(completeDir, []string{f})
		if leaf == "" {
			if folder != completeDir {
				t.Errorf("%q registers nothing but AlbumFolder = %q, want the root", f, folder)
			}
			continue
		}
		if want := filepath.Join(completeDir, leaf); folder != want {
			t.Errorf("%q registers leaf %q but AlbumFolder = %q, want %q", f, leaf, folder, want)
		}
	}
}

// seedCollidingJob creates a SELECTING job with one candidate whose single file
// lives in a remote folder named leaf. The artist part of the path differs per
// job so the two candidates are not the same remote file — that is a separate
// conflict, resolved inside ActivateCandidateWithTransfers, and it would mask
// the folder collision these tests are about.
func seedCollidingJob(t *testing.T, st *store.Store, albumID int64, artist, leaf string, now time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	job, err := st.UpsertWantedJob(ctx, albumID, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	filename := fmt.Sprintf(`music\%s\%s\01.flac`, artist, leaf)
	if err := st.InsertCandidates(ctx, job.ID, []store.NewCandidate{{
		Username: artist, Score: 1.0,
		Files: []core.CandidateFile{{Filename: filename, Size: 10}},
	}}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	if err := st.AdvanceJobState(ctx, job.ID, core.StateSelecting, now); err != nil {
		t.Fatalf("AdvanceJobState: %v", err)
	}
	return job.ID
}

func eventTypes(t *testing.T, st *store.Store, jobID int64) []core.JobEventType {
	t.Helper()
	events, err := st.JobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	types := make([]core.JobEventType, 0, len(events))
	for _, e := range events {
		types = append(types, e.Event)
	}
	return types
}

func countEvent(types []core.JobEventType, want core.JobEventType) int {
	n := 0
	for _, e := range types {
		if e == want {
			n++
		}
	}
	return n
}

// TestSelectingDefersOnDownloadFolderCollision is issue #471 at the seam that
// matters: not the cleanup, but the download itself. Two peers sharing a folder
// name — `cd1`, `Digital Media 02`, a format directory — is enough for two jobs
// to write into one local directory at the same time, because neither backend
// lets slusk choose that directory. The second job must enqueue nothing.
//
// It is deliberately not failed or rejected. A collision says "not right now",
// never "bad candidate": failing it would burn a retry, and a rejection (#317)
// would blacklist a good peer permanently because a neighbour happened to be
// downloading at the same moment.
func TestSelectingDefersOnDownloadFolderCollision(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)

	seedCollidingJob(t, st, 1, "Artist A", "cd1", now)
	second := seedCollidingJob(t, st, 2, "Artist B", "cd1", now.Add(time.Second))

	if err := NewSelecting(p).Tick(ctx, now.Add(time.Minute)); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(searcher.enqueued) != 1 || !strings.Contains(searcher.enqueued[0], "Artist A") {
		t.Fatalf("enqueued = %v, want only the first job's file", searcher.enqueued)
	}
	if leaves, err := st.DownloadFoldersForJob(ctx, second); err != nil || len(leaves) != 0 {
		t.Errorf("deferred job registered %v (err %v), want nothing — a row is a licence to delete", leaves, err)
	}

	types := eventTypes(t, st, second)
	if countEvent(types, core.EventCandidateDeferred) != 1 {
		t.Errorf("deferred job's events = %v, want exactly one candidate_deferred", types)
	}
	if countEvent(types, core.EventCandidateRejected) != 0 {
		t.Errorf("deferred job was recorded as rejected: %v", types)
	}
	if state := jobStateFor(t, st, second); state != core.StateDownloading {
		t.Errorf("deferred job state = %s, want DOWNLOADING — it keeps its candidate and retries", state)
	}
}

// TestDeferredCandidateReportsOncePerWait: the event is written when the wait
// begins, not on every tick. Downloading calls topUpCandidate once per tick for
// every DOWNLOADING job, so a per-tick event would bury the job's own history
// under one row per interval — on the one screen whose purpose is explaining
// stuck jobs.
func TestDeferredCandidateReportsOncePerWait(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)

	seedCollidingJob(t, st, 1, "Artist A", "cd1", now)
	second := seedCollidingJob(t, st, 2, "Artist B", "cd1", now.Add(time.Second))

	if err := NewSelecting(p).Tick(ctx, now.Add(time.Minute)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	cand, found, err := st.ActiveCandidate(ctx, second)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	for i := 1; i <= 3; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		if _, err := topUpCandidate(ctx, p, second, cand.ID, at, p.MaxInflightPerPeer, p.MaxTransferRetries, p.TransferDeadline, p.Logger); err != nil {
			t.Fatalf("topUpCandidate: %v", err)
		}
	}
	if n := countEvent(eventTypes(t, st, second), core.EventCandidateDeferred); n != 1 {
		t.Errorf("candidate_deferred written %d times across four deferred ticks, want 1", n)
	}
}

// TestDeferredCandidateFailsAtTheCeiling: nothing else can break this wait.
// TransfersPastDeadline and StallTimeout only look at transfers already
// QUEUED/IN_PROGRESS/STALLED, and a deferred candidate's are all still PENDING
// with no deadline set — so without this the job waits on its owner forever.
func TestDeferredCandidateFailsAtTheCeiling(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)

	seedCollidingJob(t, st, 1, "Artist A", "cd1", now)
	second := seedCollidingJob(t, st, 2, "Artist B", "cd1", now.Add(time.Second))

	if err := NewSelecting(p).Tick(ctx, now.Add(time.Minute)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	cand, found, err := st.ActiveCandidate(ctx, second)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}

	// One tick inside the deadline changes nothing; the next one past it gives up.
	inside := now.Add(time.Minute + p.TransferDeadline)
	if _, err := topUpCandidate(ctx, p, second, cand.ID, inside, p.MaxInflightPerPeer, p.MaxTransferRetries, p.TransferDeadline, p.Logger); err != nil {
		t.Fatalf("topUpCandidate: %v", err)
	}
	if state := jobStateFor(t, st, second); state != core.StateDownloading {
		t.Fatalf("job gave up at exactly the deadline (state %s), want DOWNLOADING", state)
	}

	past := inside.Add(time.Minute)
	if _, err := topUpCandidate(ctx, p, second, cand.ID, past, p.MaxInflightPerPeer, p.MaxTransferRetries, p.TransferDeadline, p.Logger); err != nil {
		t.Fatalf("topUpCandidate: %v", err)
	}
	if state := jobStateFor(t, st, second); state != core.StateSelecting {
		t.Errorf("job state after the ceiling = %s, want SELECTING so another candidate can be tried", state)
	}
	if len(searcher.enqueued) != 1 {
		t.Errorf("enqueued = %v, want the first job's file only", searcher.enqueued)
	}
}

// TestDeferredCandidateProceedsWhenTheOwnerFinishes closes the loop: the whole
// design is a wait, so it is only correct if the wait actually ends. Cleanup
// stamping cleaned_at is what releases the folder.
func TestDeferredCandidateProceedsWhenTheOwnerFinishes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	searcher := &fakeSearcher{}
	p, st := newSelectingParams(t, searcher)

	first := seedCollidingJob(t, st, 1, "Artist A", "cd1", now)
	second := seedCollidingJob(t, st, 2, "Artist B", "cd1", now.Add(time.Second))

	if err := NewSelecting(p).Tick(ctx, now.Add(time.Minute)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if err := st.MarkDownloadFolderCleaned(ctx, first, "cd1", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("MarkDownloadFolderCleaned: %v", err)
	}

	cand, found, err := st.ActiveCandidate(ctx, second)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	sent, err := topUpCandidate(ctx, p, second, cand.ID, now.Add(3*time.Minute), p.MaxInflightPerPeer, p.MaxTransferRetries, p.TransferDeadline, p.Logger)
	if err != nil {
		t.Fatalf("topUpCandidate: %v", err)
	}
	if sent != 1 {
		t.Fatalf("sent = %d after the owner released the folder, want 1", sent)
	}
	if leaves, err := st.DownloadFoldersForJob(ctx, second); err != nil || len(leaves) != 1 || leaves[0] != "cd1" {
		t.Errorf("released job's leaves = %v (err %v), want [cd1]", leaves, err)
	}
}
