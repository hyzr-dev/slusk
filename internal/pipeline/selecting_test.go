package pipeline

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// newSelectingParams builds SelectingParams over a fresh store-backed
// fixture, with generous defaults each test can override on the returned
// struct before constructing a Selecting.
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
