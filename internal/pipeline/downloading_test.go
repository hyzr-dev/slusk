package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/slskd"
	"github.com/hyzr-dev/slusk/internal/store"
)

// newDownloadingParams builds DownloadingParams over a fresh store-backed
// fixture, with generous defaults each test can override before constructing a
// Downloading.
func newDownloadingParams(t *testing.T, net *fakeNetwork, searcher *fakeSearcher) (DownloadingParams, *store.Store) {
	t.Helper()
	st := newBackedStore(t)
	return DownloadingParams{
		Store:              st,
		Network:            net,
		Peers:              searcher,
		MaxActive:          5,
		MaxTransferRetries: 3,
		StallTimeout:       time.Hour,
		MaxInflightPerPeer: 2,
		TransferDeadline:   time.Hour,
		Interval:           30 * time.Second,
		Logger:             slog.New(slog.NewTextHandler(testDiscard{}, nil)),
	}, st
}

// seedActiveCandidate creates a WANTED job, caches one candidate for it, and
// activates it - leaving the job DOWNLOADING with that candidate ACTIVE, the
// exact state Downloading's resolve/top-up phases expect. Returns the job and
// candidate ids so the test can seed transfers under the candidate.
func seedActiveCandidate(t *testing.T, st *store.Store, albumID int64, username string, files []core.CandidateFile, now time.Time) (jobID, candID int64) {
	t.Helper()
	ctx := context.Background()
	job, err := st.UpsertWantedJob(ctx, albumID, now)
	if err != nil {
		t.Fatalf("UpsertWantedJob: %v", err)
	}
	if err := st.InsertCandidates(ctx, job.ID, []store.NewCandidate{
		{Username: username, Score: 1.0, Files: files},
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
	activated, _, err := st.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, 100, now.Add(time.Hour), now)
	if err != nil || !activated {
		t.Fatalf("ActivateCandidate: %v activated=%v", err, activated)
	}
	return job.ID, cand.ID
}

// txfOpts describes a transfer to seed via seedTransfer.
type txfOpts struct {
	state      core.TransferState
	slskdID    string
	retries    int
	bytesDone  int64
	bytesTotal int64
	deadline   time.Time
	stampAt    time.Time // updated_at / last_progress_at driving timestamp
}

// seedTransfer inserts one transfer for a candidate in a precise state, using
// only real store methods so the DB (including UpdateTransferProgress's
// last_progress_at logic) matches production exactly. retries are applied via
// RetryTransfer (the only method that bumps the counter); stampAt controls the
// last_progress_at timestamp so stall tests can seed a stale download.
func seedTransfer(t *testing.T, st *store.Store, candID int64, username, filename string, o txfOpts) int64 {
	t.Helper()
	ctx := context.Background()
	tid, _, err := st.RecordEnqueueIntent(ctx, candID, username, filename, o.deadline, o.stampAt)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	for i := 0; i < o.retries; i++ {
		if err := st.RetryTransfer(ctx, tid, o.stampAt); err != nil {
			t.Fatalf("RetryTransfer: %v", err)
		}
	}
	if o.slskdID != "" {
		if _, err := st.AttachTransferID(ctx, tid, o.slskdID, o.stampAt); err != nil {
			t.Fatalf("AttachTransferID: %v", err)
		}
	}
	if err := st.UpdateTransferProgress(ctx, tid, o.state, o.bytesDone, o.bytesTotal, o.stampAt); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}
	return tid
}

// transferStatesFor returns a candidate's transfers keyed by filename, so tests
// can assert per-file outcomes.
func transferStatesFor(t *testing.T, st *store.Store, candID int64) map[string]core.Transfer {
	t.Helper()
	transfers, err := st.TransfersForCandidate(context.Background(), candID)
	if err != nil {
		t.Fatalf("TransfersForCandidate: %v", err)
	}
	out := make(map[string]core.Transfer, len(transfers))
	for _, tr := range transfers {
		out[tr.Filename] = tr
	}
	return out
}

type barrierAfterActiveSnapshotStore struct {
	DownloadingStore
	afterSnapshot func([]core.Transfer)
}

func (s *barrierAfterActiveSnapshotStore) ActiveTransfers(ctx context.Context) ([]core.Transfer, error) {
	transfers, err := s.DownloadingStore.ActiveTransfers(ctx)
	if err == nil && s.afterSnapshot != nil {
		s.afterSnapshot(transfers)
		s.afterSnapshot = nil
	}
	return transfers, err
}

// failOnceDownloadingStore decorates a real DownloadingStore and injects one
// failure into a selected reconcile mutation. Keeping every other method on the
// real store makes recovery assertions exercise the production persistence
// behavior rather than a hand-maintained fake.
type failOnceDownloadingStore struct {
	DownloadingStore
	mutation      string
	skipMutations int
	err           error
	failed        bool
}

func (s *failOnceDownloadingStore) fail(mutation string) error {
	if s.mutation != mutation || s.failed {
		return nil
	}
	if s.skipMutations > 0 {
		s.skipMutations--
		return nil
	}
	s.failed = true
	return s.err
}

func (s *failOnceDownloadingStore) AttachTransferID(ctx context.Context, transferID int64, remoteID string, now time.Time) (bool, error) {
	if err := s.fail("attach"); err != nil {
		return false, err
	}
	return s.DownloadingStore.AttachTransferID(ctx, transferID, remoteID, now)
}

func (s *failOnceDownloadingStore) RetryTransfer(ctx context.Context, transferID int64, now time.Time) error {
	if err := s.fail("retry"); err != nil {
		return err
	}
	return s.DownloadingStore.RetryTransfer(ctx, transferID, now)
}

func (s *failOnceDownloadingStore) UpdateTransferProgress(ctx context.Context, transferID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) error {
	if err := s.fail("update"); err != nil {
		return err
	}
	return s.DownloadingStore.UpdateTransferProgress(ctx, transferID, state, bytesDone, bytesTotal, now)
}

func (s *failOnceDownloadingStore) ParkJobForCandidate(ctx context.Context, transferID, candidateID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) (bool, error) {
	if err := s.fail("park"); err != nil {
		return false, err
	}
	return s.DownloadingStore.ParkJobForCandidate(ctx, transferID, candidateID, state, bytesDone, bytesTotal, now)
}

func requireMutationContext(t *testing.T, err, injected error, transferID, candidateID int64, remoteID string) {
	t.Helper()
	if !errors.Is(err, injected) {
		t.Fatalf("error = %v, want wrapped injected error", err)
	}
	for _, want := range []string{
		fmt.Sprintf("transfer %d", transferID),
		fmt.Sprintf("candidate %d", candidateID),
		fmt.Sprintf("remote %q", remoteID),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing context %q", err, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Phase 1: Reconcile — ported from internal/engine/reconciler_test.go.
// These call reconcile directly (as the engine tests call Reconcile directly)
// to isolate the reconcile phase from resolve/top-up.
// ---------------------------------------------------------------------------

// The core scenario: after a restart, one of our transfers is still live in
// slskd (adopt it), and one is past its deadline with no progress (cancel it).
// No orphan is left behind.
func TestDownloadingReconcileAdoptsLiveAndCancelsOverdue(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress, Size: 100, BytesDone: 60},
		{ID: "g2", Username: "eve", Filename: "b.flac", State: core.TransferQueued, Size: 100, BytesDone: 0},
	}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", bytesDone: 10, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})
	seedTransfer(t, st, candID, "eve", "b.flac", txfOpts{state: core.TransferQueued, slskdID: "g2", deadline: now.Add(-time.Hour), stampAt: now})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", stats.Adopted)
	}
	if stats.Cancelled != 1 {
		t.Errorf("Cancelled = %d, want 1", stats.Cancelled)
	}
	if len(net.cancelled) != 1 || net.cancelled[0] != "g2" {
		t.Errorf("expected g2 cancelled, got %v", net.cancelled)
	}
	// The overdue transfer is now terminal (CANCELLED) and was matched live in
	// slskd, so its record must be purged; the still-in-flight adopted transfer
	// (a.flac) must NOT be removed.
	if len(net.removed) != 1 || net.removed[0] != "g2" {
		t.Errorf("expected g2 removed from slskd, got %v", net.removed)
	}
	states := transferStatesFor(t, st, candID)
	if states["a.flac"].State != core.TransferInProgress {
		t.Errorf("adopted transfer should be IN_PROGRESS, got %v", states["a.flac"].State)
	}
	if states["b.flac"].State != core.TransferCancelled {
		t.Errorf("overdue transfer should be CANCELLED, got %v", states["b.flac"].State)
	}
}

// A transfer that reaches a terminal state (Completed, Succeeded) via the main
// adoption path must have its leftover slskd record purged once the store
// write lands - see removeFromSlskd's doc comment.
func TestDownloadingReconcileRemovesTerminalTransfer(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferCompleted, Size: 100, BytesDone: 100},
	}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", bytesDone: 10, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Completed != 1 {
		t.Errorf("Completed = %d, want 1", stats.Completed)
	}
	if len(net.removed) != 1 || net.removed[0] != "g1" {
		t.Errorf("expected g1 removed from slskd, got %v", net.removed)
	}
}

// A transfer we recorded but slskd no longer knows about is "lost". With retry
// budget left it goes back to PENDING for a resend instead of failing outright.
func TestDownloadingReconcileRetriesLostWhenAbsentFromSlskd(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: nil} // slskd forgot everything
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", bytesDone: 10, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Retried != 1 || stats.Parked != 0 {
		t.Errorf("Retried=%d Parked=%d, want 1/0", stats.Retried, stats.Parked)
	}
	states := transferStatesFor(t, st, candID)
	if states["a.flac"].State != core.TransferPending {
		t.Errorf("lost transfer with retries left should be PENDING, got %v", states["a.flac"].State)
	}
	if states["a.flac"].Retries != 1 {
		t.Errorf("retries = %d, want 1", states["a.flac"].Retries)
	}
}

// Once a lost transfer's retry budget is exhausted, the owning job is PARKED
// for manual operator action (issue #158) rather than silently erroring the
// transfer.
func TestDownloadingReconcileParksJobWhenRetriesExhausted(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: nil}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	jobID, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", retries: 3, bytesDone: 10, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Parked != 1 || stats.Retried != 0 {
		t.Errorf("Parked=%d Retried=%d, want 1/0", stats.Parked, stats.Retried)
	}
	job, found, err := st.JobWithTransfer(ctx, jobID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
	}
	if job.Job.State != core.StateParked {
		t.Errorf("job state = %q, want PARKED", job.Job.State)
	}
	if states := transferStatesFor(t, st, candID); states["a.flac"].State != core.TransferErrored {
		t.Errorf("parked job's transfer should be driven terminal (ERRORED) so it stops re-triggering reconcile, got %v", states["a.flac"].State)
	}
}

func TestDownloadingReconcileParkFailureLeavesTransferAndJobLive(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	p, st := newDownloadingParams(t, &fakeNetwork{downloads: nil}, &fakeSearcher{})
	jobID, candID := seedActiveCandidate(t, st, 2, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	tid := seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", retries: 3, bytesDone: 10, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})
	injected := errors.New("atomic park unavailable")
	p.Store = &failOnceDownloadingStore{DownloadingStore: st, mutation: "park", err: injected}

	stats, err := NewDownloading(p).reconcile(ctx, now)
	requireMutationContext(t, err, injected, tid, candID, "g1")
	if stats.Parked != 0 {
		t.Errorf("Parked = %d, want 0", stats.Parked)
	}
	job, found, err := st.JobWithTransfer(ctx, jobID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
	}
	if job.Job.State != core.StateDownloading {
		t.Errorf("job state = %q, want DOWNLOADING", job.Job.State)
	}
	if got := transferStatesFor(t, st, candID)["a.flac"].State; got != core.TransferInProgress {
		t.Errorf("transfer state = %q, want IN_PROGRESS", got)
	}
}

// A second reconcile tick after a job has been parked must be a quiet no-op:
// the transfer was driven ERRORED (terminal) on the first tick, so
// ActiveTransfers no longer returns it and the "lost" branch never re-runs.
// Without this, a PARKED job would produce a false parked=1 heartbeat on every
// subsequent tick for the remainder of TransferDeadline.
func TestDownloadingReconcileSecondTickAfterParkIsQuiet(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: nil}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", retries: 3, bytesDone: 10, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	if _, err := d.reconcile(ctx, now); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	stats, err := d.reconcile(ctx, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if stats.Parked != 0 {
		t.Errorf("Parked = %d on second tick, want 0 (quiet no-op, transfer already terminal)", stats.Parked)
	}
}

func TestDownloadingReconcileParkRacePreservesMovedJob(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	p, st := newDownloadingParams(t, &fakeNetwork{downloads: nil}, &fakeSearcher{})
	jobID, candID := seedActiveCandidate(t, st, 3, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", retries: 3, bytesDone: 10, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})

	guarded := &barrierAfterActiveSnapshotStore{DownloadingStore: st}
	guarded.afterSnapshot = func([]core.Transfer) {
		changed, err := st.AdvanceJobStateFrom(ctx, jobID, core.StateDownloading, core.StateCancelled, now.Add(time.Second))
		if err != nil || !changed {
			t.Fatalf("move job before park: changed=%v err=%v", changed, err)
		}
	}
	p.Store = guarded

	stats, err := NewDownloading(p).reconcile(ctx, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Parked != 0 {
		t.Errorf("Parked = %d, want 0 after guarded transition bounced", stats.Parked)
	}
	job, found, err := st.JobWithTransfer(ctx, jobID)
	if err != nil || !found {
		t.Fatalf("JobWithTransfer: found=%v (%v)", found, err)
	}
	if job.Job.State != core.StateCancelled {
		t.Errorf("job state = %q, want CANCELLED", job.Job.State)
	}
	if got := transferStatesFor(t, st, candID)["a.flac"].State; got != core.TransferErrored {
		t.Errorf("transfer state = %q, want ERRORED", got)
	}
}

// A past-deadline transfer that ALSO appears live must be processed exactly
// once: cancelled, not also adopted.
func TestDownloadingReconcileStaleUpdateCannotResurrectCancelledTransfer(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress, BytesDone: 50, Size: 100}}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	jobID, candID := seedActiveCandidate(t, st, 801, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferQueued, slskdID: "g1", deadline: now.Add(time.Hour), stampAt: now})

	guarded := &barrierAfterActiveSnapshotStore{DownloadingStore: st}
	guarded.afterSnapshot = func(transfers []core.Transfer) {
		captured, found, err := st.CancelJob(ctx, jobID, now.Add(time.Second))
		if err != nil || !found || len(captured) != 1 {
			t.Fatalf("CancelJob: captured=%v found=%v err=%v", captured, found, err)
		}
		if err := net.Cancel(ctx, captured[0].Username, captured[0].SlskdID); err != nil {
			t.Fatalf("remote Cancel: %v", err)
		}
	}
	p.Store = guarded

	if _, err := NewDownloading(p).reconcile(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := transferStatesFor(t, st, candID)["a.flac"].State; got != core.TransferCancelled {
		t.Fatalf("stale reconcile update resurrected state %v, want CANCELLED", got)
	}
}

func TestDownloadingReconcileCompensatesBouncedAttachment(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{{ID: "late-g1", Username: "bob", Filename: "a.flac", State: core.TransferQueued, Size: 100}}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	jobID, candID := seedActiveCandidate(t, st, 802, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferQueued, deadline: now.Add(time.Hour), stampAt: now})

	guarded := &barrierAfterActiveSnapshotStore{DownloadingStore: st}
	guarded.afterSnapshot = func([]core.Transfer) {
		if _, found, err := st.CancelJob(ctx, jobID, now.Add(time.Second)); err != nil || !found {
			t.Fatalf("CancelJob: found=%v err=%v", found, err)
		}
	}
	p.Store = guarded

	if _, err := NewDownloading(p).reconcile(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := transferStatesFor(t, st, candID)["a.flac"].State; got != core.TransferCancelled {
		t.Fatalf("transfer state = %v, want CANCELLED", got)
	}
	if len(net.cancelled) != 1 || net.cancelled[0] != "late-g1" {
		t.Fatalf("compensating cancellations = %v, want [late-g1]", net.cancelled)
	}
}

func TestDownloadingReconcileOverlapCountedOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress, Size: 100, BytesDone: 10},
	}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	// Past-deadline AND still non-terminal: appears in both overdue and active.
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", bytesDone: 10, bytesTotal: 100, deadline: now.Add(-time.Hour), stampAt: now})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Cancelled != 1 {
		t.Errorf("Cancelled = %d, want 1", stats.Cancelled)
	}
	if stats.Adopted != 0 {
		t.Errorf("Adopted = %d, want 0 (must not double-process the overlap)", stats.Adopted)
	}
	if states := transferStatesFor(t, st, candID); states["a.flac"].State != core.TransferCancelled {
		t.Errorf("final state = %v, want CANCELLED", states["a.flac"].State)
	}
}

// When Cancel fails, the transfer must NOT be marked cancelled — it stays
// non-terminal so the next pass retries it (no silent orphan).
func TestDownloadingReconcileCancelFailureLeavesNonTerminal(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{
		downloads: []core.RemoteTransfer{{ID: "g2", Username: "eve", Filename: "b.flac", State: core.TransferQueued, Size: 100}},
		cancelErr: errors.New("peer offline"),
	}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "eve", []core.CandidateFile{{Filename: "b.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "eve", "b.flac", txfOpts{state: core.TransferQueued, slskdID: "g2", deadline: now.Add(-time.Hour), stampAt: now})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Cancelled != 0 {
		t.Errorf("Cancelled = %d, want 0 (cancel failed)", stats.Cancelled)
	}
	if states := transferStatesFor(t, st, candID); states["b.flac"].State == core.TransferCancelled {
		t.Errorf("transfer must not be terminal after cancel failure, got %v", states["b.flac"].State)
	}
}

// A persisted transfer whose slskd id was lost (empty) but is still live gets
// its id backfilled when matched by (username, filename) in the active pass.
func TestDownloadingReconcileBackfillsMissingID(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{
		{ID: "g-recovered", Username: "bob", Filename: "a.flac", State: core.TransferInProgress, Size: 100, BytesDone: 20},
	}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "", bytesDone: 10, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	if _, err := d.reconcile(ctx, now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states := transferStatesFor(t, st, candID); states["a.flac"].SlskdID != "g-recovered" {
		t.Errorf("expected slskd id backfilled to g-recovered, got %q", states["a.flac"].SlskdID)
	}
}

// An empty-slskd_id transfer that is past deadline AND still live must be
// cancelled in slskd via the backfilled id — not silently marked cancelled.
func TestDownloadingReconcileOverdueEmptyIDCancelsViaBackfill(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{
		{ID: "g-live", Username: "eve", Filename: "b.flac", State: core.TransferQueued, Size: 100},
	}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "eve", []core.CandidateFile{{Filename: "b.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "eve", "b.flac", txfOpts{state: core.TransferQueued, slskdID: "", deadline: now.Add(-time.Hour), stampAt: now})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states := transferStatesFor(t, st, candID); states["b.flac"].SlskdID != "g-live" {
		t.Errorf("expected backfill to g-live before cancel, got %q", states["b.flac"].SlskdID)
	}
	if len(net.cancelled) != 1 || net.cancelled[0] != "g-live" {
		t.Errorf("expected cancel via backfilled live id g-live, got %v", net.cancelled)
	}
	if stats.Cancelled != 1 {
		t.Errorf("Cancelled = %d, want 1", stats.Cancelled)
	}
}

// A retryable rejection ("Too many megabytes") with retries left goes back to
// PENDING for a resend, not ERRORED.
func TestDownloadingReconcileRetriesRejectedWhenRetryable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferErrored, Size: 100, Failure: "Too many megabytes", Retryable: true},
	}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferQueued, slskdID: "g1", deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states := transferStatesFor(t, st, candID); states["a.flac"].State != core.TransferPending {
		t.Errorf("retryable rejection should be PENDING, got %v", states["a.flac"].State)
	}
	if stats.Retried != 1 {
		t.Errorf("Retried = %d, want 1", stats.Retried)
	}
	// The native backend's Enqueue is idempotent, so the stale remote record
	// must be purged for the re-enqueue to actually start fresh.
	if len(net.removed) != 1 || net.removed[0] != "g1" {
		t.Errorf("expected g1 removed from the peer backend, got %v", net.removed)
	}
}

// A rejection whose retry budget is spent must go terminal (ERRORED).
func TestDownloadingReconcileRejectedErrorsWhenExhausted(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferErrored, Size: 100, Failure: "Too many megabytes", Retryable: true},
	}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferQueued, slskdID: "g1", retries: 3, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	if _, err := d.reconcile(ctx, now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states := transferStatesFor(t, st, candID); states["a.flac"].State != core.TransferErrored {
		t.Errorf("exhausted rejection should ERROR, got %v", states["a.flac"].State)
	}
	// The terminal-state path removes the remote record exactly once; it must
	// not also be removed by the retryable-errored path above.
	if len(net.removed) != 1 || net.removed[0] != "g1" {
		t.Errorf("expected g1 removed exactly once via the terminal path, got %v", net.removed)
	}
}

// A permanent rejection reason must go terminal immediately regardless of budget.
func TestDownloadingReconcileRejectedTerminalReasonDoesNotRetry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferErrored, Size: 100, Failure: "File not shared.", Retryable: false},
	}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferQueued, slskdID: "g1", retries: 0, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	if _, err := d.reconcile(ctx, now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states := transferStatesFor(t, st, candID); states["a.flac"].State != core.TransferErrored {
		t.Errorf("permanent reason should ERROR without retry, got %v", states["a.flac"].State)
	}
}

// A stalled IN_PROGRESS transfer must be cancelled in slskd and retried.
func TestDownloadingReconcileStalledCancelsAndRetries(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress, Size: 100, BytesDone: 40},
	}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", bytesDone: 40, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: stale})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(net.cancelled) != 1 || net.cancelled[0] != "g1" {
		t.Errorf("expected stalled transfer g1 cancelled, got %v", net.cancelled)
	}
	if states := transferStatesFor(t, st, candID); states["a.flac"].State != core.TransferPending {
		t.Errorf("stalled transfer should be PENDING, got %v", states["a.flac"].State)
	}
	if stats.Stalled != 1 || stats.Adopted != 0 {
		t.Errorf("Stalled=%d Adopted=%d, want 1/0", stats.Stalled, stats.Adopted)
	}
}

// A stalled transfer whose retry budget is spent must go ERRORED.
func TestDownloadingReconcileStalledErrorsWhenExhausted(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress, Size: 100, BytesDone: 40},
	}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", retries: 3, bytesDone: 40, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: stale})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(net.cancelled) != 1 || net.cancelled[0] != "g1" {
		t.Errorf("expected stalled transfer g1 cancelled, got %v", net.cancelled)
	}
	if states := transferStatesFor(t, st, candID); states["a.flac"].State != core.TransferErrored {
		t.Errorf("exhausted stall should ERROR, got %v", states["a.flac"].State)
	}
	if stats.Stalled != 1 {
		t.Errorf("Stalled = %d, want 1", stats.Stalled)
	}
	// Now that the store has marked it ERRORED (terminal), its leftover slskd
	// record must be purged so slskd's transfer list does not accumulate it.
	if len(net.removed) != 1 || net.removed[0] != "g1" {
		t.Errorf("expected stalled-and-exhausted transfer g1 removed from slskd, got %v", net.removed)
	}
}

// A transfer that made byte progress within the stall timeout is healthy:
// adopted normally, never cancelled.
func TestDownloadingReconcileFreshProgressNotStalled(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Minute)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress, Size: 100, BytesDone: 40},
	}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", bytesDone: 40, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: fresh})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(net.cancelled) != 0 {
		t.Errorf("healthy transfer must not be cancelled, got %v", net.cancelled)
	}
	if stats.Stalled != 0 || stats.Adopted != 1 {
		t.Errorf("Stalled=%d Adopted=%d, want 0/1", stats.Stalled, stats.Adopted)
	}
}

// If cancelling a stalled transfer in slskd fails, its durable STALLED intent
// remains so the next pass can retry cancellation without losing why it ran.
func TestDownloadingReconcileStalledCancelFailurePreservesIntent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	net := &fakeNetwork{
		downloads: []core.RemoteTransfer{{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress, Size: 100, BytesDone: 40}},
		cancelErr: errors.New("peer offline"),
	}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", bytesDone: 40, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: stale})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states := transferStatesFor(t, st, candID); states["a.flac"].State != core.TransferStalled {
		t.Errorf("transfer must preserve STALLED intent after failed cancel, got %v", states["a.flac"].State)
	}
	if stats.Stalled != 0 {
		t.Errorf("Stalled = %d, want 0 (cancel failed)", stats.Stalled)
	}
}

// A failed ID backfill must abort before progress or remote cleanup. The next
// pass retries the write, then commits the terminal state and removes exactly
// once.
func TestDownloadingReconcileAttachFailureRecoversBeforeTerminalCleanup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{{
		ID: "g-recovered", Username: "bob", Filename: "a.flac",
		State: core.TransferCompleted, Size: 100, BytesDone: 100,
	}}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	tid := seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{
		state: core.TransferQueued, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now,
	})
	injected := errors.New("attach unavailable")
	p.Store = &failOnceDownloadingStore{DownloadingStore: st, mutation: "attach", err: injected}
	d := NewDownloading(p)

	stats, err := d.reconcile(ctx, now)
	requireMutationContext(t, err, injected, tid, candID, "g-recovered")
	if stats.Completed != 0 || stats.Adopted != 0 {
		t.Errorf("stats changed before persistence: %+v", stats)
	}
	if len(net.removed) != 0 {
		t.Fatalf("remote cleanup ran before persistence: %v", net.removed)
	}
	if tr := transferStatesFor(t, st, candID)["a.flac"]; tr.SlskdID != "" || tr.State != core.TransferQueued {
		t.Fatalf("transfer changed after failed attach: %+v", tr)
	}

	stats, err = d.reconcile(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatalf("reconcile recovery: %v", err)
	}
	if stats.Completed != 1 {
		t.Errorf("Completed = %d, want 1", stats.Completed)
	}
	if len(net.removed) != 1 || net.removed[0] != "g-recovered" {
		t.Errorf("recovery cleanup = %v, want [g-recovered]", net.removed)
	}
	if tr := transferStatesFor(t, st, candID)["a.flac"]; tr.SlskdID != "g-recovered" || tr.State != core.TransferCompleted {
		t.Errorf("recovered transfer = %+v", tr)
	}
}

// A stalled transfer must persist its local intent before touching slskd. If
// that write fails, the remote transfer remains live and a later pass retries
// the complete sequence.
func TestDownloadingReconcileStallIntentFailureDoesNotCancel(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{{
		ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress,
		Size: 100, BytesDone: 40,
	}}}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	tid := seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{
		state: core.TransferInProgress, slskdID: "g1", bytesDone: 40, bytesTotal: 100,
		deadline: now.Add(time.Hour), stampAt: now.Add(-2 * time.Hour),
	})
	injected := errors.New("stall intent unavailable")
	p.Store = &failOnceDownloadingStore{DownloadingStore: st, mutation: "update", err: injected}
	d := NewDownloading(p)

	_, err := d.reconcile(ctx, now)
	requireMutationContext(t, err, injected, tid, candID, "g1")
	if len(net.cancelled) != 0 || len(net.removed) != 0 {
		t.Fatalf("remote changed before stalled intent persisted: cancelled=%v removed=%v", net.cancelled, net.removed)
	}
	if net.downloads[0].State != core.TransferInProgress {
		t.Fatalf("remote state = %q, want InProgress", net.downloads[0].State)
	}
	if tr := transferStatesFor(t, st, candID)["a.flac"]; tr.State != core.TransferInProgress {
		t.Fatalf("local state after failed intent = %s, want IN_PROGRESS", tr.State)
	}

	stats, err := d.reconcile(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatalf("reconcile recovery: %v", err)
	}
	if stats.Stalled != 1 {
		t.Errorf("Stalled = %d, want 1", stats.Stalled)
	}
	if tr := transferStatesFor(t, st, candID)["a.flac"]; tr.State != core.TransferPending || tr.Retries != 1 {
		t.Fatalf("recovered transfer = %+v", tr)
	}
}

// Every terminal route must persist before Remove. The deadline and exhausted
// stall cases also prove successful remote cancellation is retained as a
// terminal record until the following local write commits.
func TestDownloadingReconcileUpdateFailureDefersTerminalCleanup(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		liveState        core.TransferState
		initialState     core.TransferState
		retries          int
		deadline         time.Time
		stampAt          time.Time
		wantState        core.TransferState
		wantAfterFailure core.TransferState
		wantStat         string
		wantCancel       bool
		skipMutations    int
	}{
		{
			name: "completed", liveState: core.TransferCompleted,
			initialState: core.TransferInProgress, deadline: now.Add(time.Hour), stampAt: now,
			wantState: core.TransferCompleted, wantAfterFailure: core.TransferInProgress, wantStat: "completed",
		},
		{
			name: "deadline cancelled", liveState: core.TransferQueued,
			initialState: core.TransferQueued, deadline: now.Add(-time.Hour), stampAt: now,
			wantState: core.TransferCancelled, wantAfterFailure: core.TransferQueued, wantStat: "cancelled", wantCancel: true,
		},
		{
			name: "stalled errored", liveState: core.TransferInProgress, retries: 3,
			initialState: core.TransferInProgress, deadline: now.Add(time.Hour), stampAt: now.Add(-2 * time.Hour),
			wantState: core.TransferErrored, wantAfterFailure: core.TransferStalled, wantStat: "stalled", wantCancel: true,
			skipMutations: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			net := &fakeNetwork{downloads: []core.RemoteTransfer{{
				ID: "g1", Username: "bob", Filename: "a.flac", State: tt.liveState,
				Size: 100, BytesDone: 40,
			}}}
			p, st := newDownloadingParams(t, net, &fakeSearcher{})
			_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
			tid := seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{
				state: tt.initialState, slskdID: "g1", retries: tt.retries,
				bytesDone: 40, bytesTotal: 100, deadline: tt.deadline, stampAt: tt.stampAt,
			})
			injected := errors.New("progress unavailable")
			p.Store = &failOnceDownloadingStore{
				DownloadingStore: st, mutation: "update", skipMutations: tt.skipMutations, err: injected,
			}
			d := NewDownloading(p)

			stats, err := d.reconcile(ctx, now)
			requireMutationContext(t, err, injected, tid, candID, "g1")
			if stats.Completed != 0 || stats.Cancelled != 0 || stats.Parked != 0 || stats.Stalled != 0 || stats.Adopted != 0 {
				t.Errorf("stats changed before persistence: %+v", stats)
			}
			if len(net.removed) != 0 {
				t.Fatalf("remote cleanup ran after failed terminal write: %v", net.removed)
			}
			if got := len(net.cancelled); got != btoi(tt.wantCancel) {
				t.Fatalf("cancel calls after failed write = %d, want %d", got, btoi(tt.wantCancel))
			}
			if tr := transferStatesFor(t, st, candID)["a.flac"]; tr.State != tt.wantAfterFailure {
				t.Fatalf("state after failed write = %s, want %s", tr.State, tt.wantAfterFailure)
			}
			if tt.wantCancel && net.downloads[0].State != core.TransferCancelled {
				t.Fatalf("remote state after Cancel = %q, want Completed, Cancelled", net.downloads[0].State)
			}

			stats, err = d.reconcile(ctx, now.Add(time.Second))
			if err != nil {
				t.Fatalf("reconcile recovery: %v", err)
			}
			switch tt.wantStat {
			case "completed":
				if stats.Completed != 1 {
					t.Errorf("Completed = %d, want 1", stats.Completed)
				}
			case "cancelled":
				if stats.Cancelled != 1 {
					t.Errorf("Cancelled = %d, want 1", stats.Cancelled)
				}
			case "stalled":
				if stats.Stalled != 1 {
					t.Errorf("Stalled = %d, want 1", stats.Stalled)
				}
			}
			if len(net.removed) != 1 || net.removed[0] != "g1" {
				t.Errorf("cleanup after recovery = %v, want [g1]", net.removed)
			}
			if tr := transferStatesFor(t, st, candID)["a.flac"]; tr.State != tt.wantState {
				t.Errorf("state after recovery = %s, want %s", tr.State, tt.wantState)
			}
		})
	}
}

// A failed stalled retry must abort the pass after remote Cancel but before
// stats or top-up. Retrying the pass converges to one PENDING row, which top-up
// enqueues once; a second top-up observes QUEUED and does not duplicate it.
func TestDownloadingReconcileRetryFailureConvergesWithoutDuplicateEnqueue(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []core.RemoteTransfer{{
		ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress,
		Size: 100, BytesDone: 40,
	}}}
	peers := &fakeSearcher{}
	p, st := newDownloadingParams(t, net, peers)
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	tid := seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{
		state: core.TransferInProgress, slskdID: "g1", bytesDone: 40, bytesTotal: 100,
		deadline: now.Add(time.Hour), stampAt: now.Add(-2 * time.Hour),
	})
	injected := errors.New("retry unavailable")
	p.Store = &failOnceDownloadingStore{DownloadingStore: st, mutation: "retry", err: injected}
	d := NewDownloading(p)

	stats, err := d.reconcile(ctx, now)
	requireMutationContext(t, err, injected, tid, candID, "g1")
	if stats.Retried != 0 || stats.Stalled != 0 {
		t.Errorf("stats changed before retry persistence: %+v", stats)
	}
	if len(net.cancelled) != 1 {
		t.Errorf("Cancel must precede retry write, calls = %v", net.cancelled)
	}
	if len(net.removed) != 0 || len(peers.enqueued) != 0 {
		t.Fatalf("cleanup/enqueue ran after retry failure: removed=%v enqueued=%v", net.removed, peers.enqueued)
	}
	if net.downloads[0].State != core.TransferCancelled {
		t.Fatalf("remote state after Cancel = %q, want Completed, Cancelled", net.downloads[0].State)
	}
	if tr := transferStatesFor(t, st, candID)["a.flac"]; tr.State != core.TransferStalled || tr.Retries != 0 {
		t.Fatalf("stalled intent not preserved after failed retry: %+v", tr)
	}

	// Recover after the original transfer deadline to prove the durable STALLED
	// intent still governs our terminal remote cancellation.
	recoveredAt := now.Add(2 * time.Hour)
	stats, err = d.reconcile(ctx, recoveredAt)
	if err != nil {
		t.Fatalf("reconcile recovery: %v", err)
	}
	if stats.Stalled != 1 {
		t.Errorf("Stalled = %d, want 1", stats.Stalled)
	}
	if tr := transferStatesFor(t, st, candID)["a.flac"]; tr.State != core.TransferPending || tr.Retries != 1 {
		t.Fatalf("transfer after retry recovery: %+v", tr)
	}
	if len(net.removed) != 1 || net.removed[0] != "g1" {
		t.Fatalf("terminal cancellation cleanup after retry commit = %v, want [g1]", net.removed)
	}
	if err := d.topUpDownloads(ctx, recoveredAt.Add(time.Second)); err != nil {
		t.Fatalf("first top-up: %v", err)
	}
	if err := d.topUpDownloads(ctx, recoveredAt.Add(2*time.Second)); err != nil {
		t.Fatalf("second top-up: %v", err)
	}
	if len(peers.enqueued) != 1 || peers.enqueued[0] != "a.flac" {
		t.Errorf("enqueues = %v, want exactly [a.flac]", peers.enqueued)
	}
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Phase 2: Resolve — new-model transitions + two-phase fail + stale backlog.
// ---------------------------------------------------------------------------

func TestDownloadingSuccessAdvancesToImporting(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	p, st := newDownloadingParams(t, &fakeNetwork{}, &fakeSearcher{})
	jobID, candID := seedActiveCandidate(t, st, 1, "bob",
		[]core.CandidateFile{{Filename: `A\01.flac`, Size: 10}, {Filename: `A\02.flac`, Size: 10}}, now)
	seedTransfer(t, st, candID, "bob", `A\01.flac`, txfOpts{state: core.TransferCompleted, slskdID: "s1", bytesDone: 10, bytesTotal: 10, deadline: now.Add(time.Hour), stampAt: now})
	seedTransfer(t, st, candID, "bob", `A\02.flac`, txfOpts{state: core.TransferCompleted, slskdID: "s2", bytesDone: 10, bytesTotal: 10, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateImporting {
		t.Errorf("all-completed job should advance to IMPORTING, got %v", got)
	}
}

// TestDownloadingRoutesUnidentifiedManualJobToNotImported covers issue #59: a
// manual job whose download completes but that was never identified against
// a MusicBrainz release group (AlbumMBID == "") must go straight to the
// terminal NOT_IMPORTED, not IMPORTING - IMPORTING would call
// Music.AlbumStatus(0) on every tick since the job has no LidarrAlbumID.
func TestDownloadingRoutesUnidentifiedManualJobToNotImported(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	p, st := newDownloadingParams(t, &fakeNetwork{}, &fakeSearcher{})

	job, err := st.CreateManualJob(ctx, "Album", "Artist", "bob", "",
		[]store.ManualJobFile{{Filename: `A\01.flac`, Size: 10}}, now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("CreateManualJob: %v", err)
	}
	cand, found, err := st.ActiveCandidate(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("ActiveCandidate: %v found=%v", err, found)
	}
	transfers, err := st.TransfersForCandidate(ctx, cand.ID)
	if err != nil || len(transfers) != 1 {
		t.Fatalf("TransfersForCandidate: %v transfers=%+v", err, transfers)
	}
	if err := st.UpdateTransferProgress(ctx, transfers[0].ID, core.TransferCompleted, 10, 10, now); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}

	d := NewDownloading(p)
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := jobStateFor(t, st, job.ID); got != core.StateNotImported {
		t.Errorf("job state = %v, want NOT_IMPORTED", got)
	}

	events, err := st.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	found = false
	for _, e := range events {
		if e.Event == core.EventNotImported {
			found = true
		}
	}
	if !found {
		t.Errorf("job events = %+v, want one EventNotImported", events)
	}
}

func TestDownloadingFailureReturnsJobToSelectingWithoutRetryBump(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	peers := &fakeSearcher{}
	p, st := newDownloadingParams(t, &fakeNetwork{}, peers)
	jobID, candID := seedActiveCandidate(t, st, 1, "bob",
		[]core.CandidateFile{{Filename: `A\01.flac`, Size: 10}}, now)
	// Seed a nonzero retries and an (already-elapsed) not_before so the test can
	// assert a per-candidate failure leaves BOTH untouched: no retries bump, and
	// no fresh cooldown (the legacy fail path added one via SetJobCooldown; the
	// pipeline must not). The seeded not_before is in the past so the job is still
	// runnable in SELECTING.
	seededNotBefore := now.Add(-time.Hour)
	if err := st.SetJobBackoff(ctx, jobID, 2, seededNotBefore, now); err != nil {
		t.Fatalf("SetJobBackoff: %v", err)
	}
	seedTransfer(t, st, candID, "bob", `A\01.flac`, txfOpts{state: core.TransferErrored, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateSelecting {
		t.Errorf("failed candidate should return job to SELECTING, got %v", got)
	}
	// Candidate FAILED.
	if _, found, err := st.ActiveCandidate(ctx, jobID); err != nil || found {
		t.Errorf("candidate should no longer be ACTIVE, found=%v (%v)", found, err)
	}
	// retries unchanged (no bump), no cooldown (not_before nil).
	jobs, err := st.RunnableJobsInState(ctx, core.StateSelecting, now, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("expected the job runnable in SELECTING, got %+v (%v)", jobs, err)
	}
	if jobs[0].Retries != 2 {
		t.Errorf("Retries = %d, want 2 (unchanged by per-candidate failure)", jobs[0].Retries)
	}
	if jobs[0].NotBefore == nil || !jobs[0].NotBefore.Equal(seededNotBefore) {
		t.Errorf("NotBefore = %v, want unchanged %v (no fresh cooldown on per-candidate failure)", jobs[0].NotBefore, seededNotBefore)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("expected the failed candidate's folder cleaned up, got %+v", peers.deletedFolders)
	}
}

func TestDownloadingRecordsFailOutcome(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	p, st := newDownloadingParams(t, &fakeNetwork{}, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob",
		[]core.CandidateFile{{Filename: `A\01.flac`, Size: 10}}, now)
	seedTransfer(t, st, candID, "bob", `A\01.flac`, txfOpts{state: core.TransferErrored, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rel, err := st.ReliabilityFor(ctx, 0, []string{"bob"})
	if err != nil {
		t.Fatalf("ReliabilityFor: %v", err)
	}
	if rel["bob"].Global.FailCount != 1 {
		t.Errorf("bob's global fail count = %d, want 1", rel["bob"].Global.FailCount)
	}
	if rel["bob"].Global.SuccessCount != 0 {
		t.Errorf("bob's global success count = %d, want 0", rel["bob"].Global.SuccessCount)
	}
}

func TestDownloadingAllTerminalFailsFirstTick(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	peers := &fakeSearcher{}
	p, st := newDownloadingParams(t, &fakeNetwork{}, peers)
	jobID, candID := seedActiveCandidate(t, st, 1, "bob",
		[]core.CandidateFile{{Filename: `A\01.flac`, Size: 10}}, now)
	seedTransfer(t, st, candID, "bob", `A\01.flac`, txfOpts{state: core.TransferErrored, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(peers.cancelled) != 0 {
		t.Errorf("an all-terminal candidate has nothing to cancel, got %+v", peers.cancelled)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("an all-terminal failed candidate should be cleaned up on the first tick, got %+v", peers.deletedFolders)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateSelecting {
		t.Errorf("an all-terminal failed candidate should return to SELECTING, got %v", got)
	}
}

func TestDownloadingTwoPhaseFailWaitsForLiveSiblings(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	peers := &fakeSearcher{}
	p, st := newDownloadingParams(t, &fakeNetwork{}, peers)
	jobID, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{
		{Filename: `A\01.flac`, Size: 10}, {Filename: `A\02.flac`, Size: 10}, {Filename: `A\03.flac`, Size: 10},
	}, now)
	// One ERRORED, one IN_PROGRESS in slskd (must be cancelled there), one PENDING.
	seedTransfer(t, st, candID, "bob", `A\01.flac`, txfOpts{state: core.TransferErrored, deadline: now.Add(time.Hour), stampAt: now})
	inProgressID := seedTransfer(t, st, candID, "bob", `A\02.flac`, txfOpts{state: core.TransferInProgress, slskdID: "slskd-inprog", bytesDone: 50, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})
	if err := st.RecordPendingTransfer(ctx, candID, "bob", `A\03.flac`, 10, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}

	d := NewDownloading(p)
	// Tick 1: cancel active + pending, no cleanup, job stays DOWNLOADING.
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve tick 1: %v", err)
	}
	if len(peers.cancelled) != 1 || peers.cancelled[0] != "slskd-inprog" {
		t.Errorf("expected the IN_PROGRESS sibling cancelled in slskd, got %+v", peers.cancelled)
	}
	if len(peers.deletedFolders) != 0 {
		t.Errorf("cleanup must not run while a sibling is still active, got %+v", peers.deletedFolders)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateDownloading {
		t.Errorf("job should stay DOWNLOADING during cancellation, got %v", got)
	}
	states := transferStatesFor(t, st, candID)
	if states[`A\03.flac`].State != core.TransferCancelled {
		t.Errorf("never-sent PENDING sibling should be CANCELLED, got %v", states[`A\03.flac`].State)
	}
	if states[`A\02.flac`].State != core.TransferInProgress {
		t.Errorf("IN_PROGRESS sibling stays non-terminal until reconciler confirms, got %v", states[`A\02.flac`].State)
	}

	// The reconciler picks up slskd's cancellation and marks the sibling terminal.
	if err := st.UpdateTransferProgress(ctx, inProgressID, core.TransferCancelled, 50, 100, now); err != nil {
		t.Fatalf("UpdateTransferProgress: %v", err)
	}

	// Tick 2: everything terminal -> cleanup + FailCandidate + SELECTING.
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve tick 2: %v", err)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("expected the failed candidate's folder cleaned up on tick 2, got %+v", peers.deletedFolders)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateSelecting {
		t.Errorf("job should be SELECTING after all siblings terminal, got %v", got)
	}
}

func TestDownloadingCancelErrorRetriesNextTick(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	peers := &fakeSearcher{cancelErr: errors.New("slskd down")}
	p, st := newDownloadingParams(t, &fakeNetwork{}, peers)
	jobID, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{
		{Filename: `A\01.flac`, Size: 10}, {Filename: `A\02.flac`, Size: 10}, {Filename: `A\03.flac`, Size: 10},
	}, now)
	seedTransfer(t, st, candID, "bob", `A\01.flac`, txfOpts{state: core.TransferErrored, deadline: now.Add(time.Hour), stampAt: now})
	seedTransfer(t, st, candID, "bob", `A\02.flac`, txfOpts{state: core.TransferInProgress, slskdID: "slskd-inprog", bytesDone: 50, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})
	if err := st.RecordPendingTransfer(ctx, candID, "bob", `A\03.flac`, 10, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}

	d := NewDownloading(p)
	// Tick 1: cancel fails -> nothing recorded cancelled, no cleanup, still DOWNLOADING.
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve tick 1: %v", err)
	}
	if len(peers.cancelled) != 0 {
		t.Errorf("a failed Cancel must not be recorded, got %+v", peers.cancelled)
	}
	if len(peers.deletedFolders) != 0 {
		t.Errorf("cleanup must not run while the active sibling is uncancelled, got %+v", peers.deletedFolders)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateDownloading {
		t.Errorf("job should stay DOWNLOADING when the cancel failed, got %v", got)
	}
	if states := transferStatesFor(t, st, candID); states[`A\02.flac`].State != core.TransferInProgress {
		t.Errorf("the active sibling must stay non-terminal after a failed cancel, got %v", states[`A\02.flac`].State)
	}

	// Tick 2: slskd recovers, the cancel now succeeds.
	peers.cancelErr = nil
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve tick 2: %v", err)
	}
	if len(peers.cancelled) != 1 || peers.cancelled[0] != "slskd-inprog" {
		t.Errorf("the retried cancel should reach slskd, got %+v", peers.cancelled)
	}
}

func TestDownloadingCancelNotFoundTreatsAsAlreadyCancelled(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	client := slskd.New(notFound.URL, "key")
	cancelErr := client.Cancel(context.Background(), "someone", "some-id")
	if !errors.Is(cancelErr, core.ErrRemoteNotFound) {
		t.Fatalf("expected a 404 from the test server, got %v", cancelErr)
	}

	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	peers := &fakeSearcher{cancelErr: cancelErr}
	p, st := newDownloadingParams(t, &fakeNetwork{}, peers)
	jobID, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{
		{Filename: `A\01.flac`, Size: 10}, {Filename: `A\02.flac`, Size: 10}, {Filename: `A\03.flac`, Size: 10},
	}, now)
	seedTransfer(t, st, candID, "bob", `A\01.flac`, txfOpts{state: core.TransferErrored, deadline: now.Add(time.Hour), stampAt: now})
	seedTransfer(t, st, candID, "bob", `A\02.flac`, txfOpts{state: core.TransferInProgress, slskdID: "slskd-inprog", bytesDone: 50, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})
	if err := st.RecordPendingTransfer(ctx, candID, "bob", `A\03.flac`, 10, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}

	d := NewDownloading(p)
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve tick 1: %v", err)
	}
	if states := transferStatesFor(t, st, candID); states[`A\02.flac`].State != core.TransferCancelled {
		t.Errorf("a 404 on cancel must mark the sibling cancelled locally, got %v", states[`A\02.flac`].State)
	}
	// Tick 2: everything now terminal -> cleanup + SELECTING, not stuck on a 404.
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve tick 2: %v", err)
	}
	if got := jobStateFor(t, st, jobID); got != core.StateSelecting {
		t.Errorf("job should reach SELECTING once the vanished sibling is treated as cancelled, got %v", got)
	}
}

// TestDownloadingDoesNotTopUpAlreadyFailed reproduces the legacy race: top-up
// must never release a failed candidate's still-PENDING sibling to slskd, into
// a folder resolve is about to delete. resolve runs first (marking the pending
// sibling CANCELLED and advancing the job out of DOWNLOADING), so top-up finds
// nothing to send.
func TestDownloadingDoesNotTopUpAlreadyFailed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	peers := &fakeSearcher{}
	p, st := newDownloadingParams(t, &fakeNetwork{}, peers)
	_, candID := seedActiveCandidate(t, st, 1, "bob",
		[]core.CandidateFile{{Filename: `A\01.flac`, Size: 10}, {Filename: `A\02.flac`, Size: 10}}, now)
	seedTransfer(t, st, candID, "bob", `A\01.flac`, txfOpts{state: core.TransferErrored, deadline: now.Add(time.Hour), stampAt: now})
	if err := st.RecordPendingTransfer(ctx, candID, "bob", `A\02.flac`, 10, now); err != nil {
		t.Fatalf("RecordPendingTransfer: %v", err)
	}

	d := NewDownloading(p)
	// Mirror Tick's resolve-before-top-up ordering (reconcile is a no-op here).
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := d.topUpDownloads(ctx, now); err != nil {
		t.Fatalf("topUpDownloads: %v", err)
	}
	if len(peers.enqueued) != 0 {
		t.Errorf("a failed candidate's pending sibling must not be topped up, got enqueued %+v", peers.enqueued)
	}
	if len(peers.deletedFolders) != 1 || peers.deletedFolders[0] != "A" {
		t.Errorf("expected the failed candidate's folder cleaned up, got %+v", peers.deletedFolders)
	}
}

// TestDownloadingFirstTickResolvesStaleBacklog is the pipeline replacement for
// the legacy SweepStaleDownloads: a backlog of DOWNLOADING jobs whose transfers
// are already terminal (crash-legacy) all resolve on a single tick, since
// resolve is bounded by MaxActive (the DOWNLOADING ceiling) rather than a small
// per-tick batch. No separate startup sweep is needed.
func TestDownloadingFirstTickResolvesStaleBacklog(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	peers := &fakeSearcher{}
	p, st := newDownloadingParams(t, &fakeNetwork{}, peers)

	const n = 4 // < MaxActive(5): one resolve tick handles the whole backlog
	jobIDs := make([]int64, n)
	for i := 0; i < n; i++ {
		// Distinct peer per job keeps this cap test's live transfer fixtures
		// independent; cross-candidate ownership is covered by store tests.
		user := fmt.Sprintf("bob%d", i)
		jobID, candID := seedActiveCandidate(t, st, int64(1000+i), user,
			[]core.CandidateFile{{Filename: `A\01.flac`, Size: 10}}, now)
		seedTransfer(t, st, candID, user, `A\01.flac`, txfOpts{state: core.TransferErrored, deadline: now.Add(time.Hour), stampAt: now})
		jobIDs[i] = jobID
	}

	d := NewDownloading(p)
	if err := d.resolve(ctx, now); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, id := range jobIDs {
		if got := jobStateFor(t, st, id); got != core.StateSelecting {
			t.Errorf("job %d: expected SELECTING after first-tick resolve, got %v", id, got)
		}
	}
}
