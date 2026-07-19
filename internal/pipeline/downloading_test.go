package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
	"github.com/samuelenocsson/slskdarr/internal/store"
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
	activated, _, err := st.ActivateCandidateWithTransfers(ctx, cand.ID, job.ID, 100, now)
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
	tid, err := st.RecordEnqueueIntent(ctx, candID, username, filename, o.deadline, o.stampAt)
	if err != nil {
		t.Fatalf("RecordEnqueueIntent: %v", err)
	}
	for i := 0; i < o.retries; i++ {
		if err := st.RetryTransfer(ctx, tid, o.stampAt); err != nil {
			t.Fatalf("RetryTransfer: %v", err)
		}
	}
	if o.slskdID != "" {
		if err := st.AttachTransferID(ctx, tid, o.slskdID, o.stampAt); err != nil {
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
	net := &fakeNetwork{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 60},
		{ID: "g2", Username: "eve", Filename: "b.flac", State: "Queued", Size: 100, BytesTransferred: 0},
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
	net := &fakeNetwork{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "Completed, Succeeded", Size: 100, BytesTransferred: 100},
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
	if stats.Retried != 1 || stats.Lost != 0 {
		t.Errorf("Retried=%d Lost=%d, want 1/0", stats.Retried, stats.Lost)
	}
	states := transferStatesFor(t, st, candID)
	if states["a.flac"].State != core.TransferPending {
		t.Errorf("lost transfer with retries left should be PENDING, got %v", states["a.flac"].State)
	}
	if states["a.flac"].Retries != 1 {
		t.Errorf("retries = %d, want 1", states["a.flac"].Retries)
	}
}

// Once a lost transfer's retry budget is exhausted, it is finally ERRORED.
func TestDownloadingReconcileMarksLostWhenRetriesExhausted(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: nil}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	_, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{{Filename: "a.flac", Size: 100}}, now)
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{state: core.TransferInProgress, slskdID: "g1", retries: 3, bytesDone: 10, bytesTotal: 100, deadline: now.Add(time.Hour), stampAt: now})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Lost != 1 || stats.Retried != 0 {
		t.Errorf("Lost=%d Retried=%d, want 1/0", stats.Lost, stats.Retried)
	}
	if states := transferStatesFor(t, st, candID); states["a.flac"].State != core.TransferErrored {
		t.Errorf("exhausted lost transfer should be ERRORED, got %v", states["a.flac"].State)
	}
}

// A past-deadline transfer that ALSO appears live must be processed exactly
// once: cancelled, not also adopted.
func TestDownloadingReconcileOverlapCountedOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 10},
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
		downloads: []slskd.Transfer{{ID: "g2", Username: "eve", Filename: "b.flac", State: "Queued", Size: 100}},
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
	net := &fakeNetwork{downloads: []slskd.Transfer{
		{ID: "g-recovered", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 20},
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
	net := &fakeNetwork{downloads: []slskd.Transfer{
		{ID: "g-live", Username: "eve", Filename: "b.flac", State: "Queued", Size: 100},
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
	net := &fakeNetwork{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "Completed, Rejected", Size: 100, Exception: "Too many megabytes"},
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
}

// A rejection whose retry budget is spent must go terminal (ERRORED).
func TestDownloadingReconcileRejectedErrorsWhenExhausted(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "Completed, Rejected", Size: 100, Exception: "Too many megabytes"},
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
}

// A permanent rejection reason must go terminal immediately regardless of budget.
func TestDownloadingReconcileRejectedTerminalReasonDoesNotRetry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	net := &fakeNetwork{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "Completed, Rejected", Size: 100, Exception: "File not shared."},
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
	net := &fakeNetwork{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 40},
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
	net := &fakeNetwork{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 40},
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
	net := &fakeNetwork{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 40},
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

// If cancelling a stalled transfer in slskd fails, it must be left untouched.
func TestDownloadingReconcileStalledCancelFailureLeavesUntouched(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	net := &fakeNetwork{
		downloads: []slskd.Transfer{{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 40}},
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
	if states := transferStatesFor(t, st, candID); states["a.flac"].State != core.TransferInProgress {
		t.Errorf("transfer must stay IN_PROGRESS after failed cancel, got %v", states["a.flac"].State)
	}
	if stats.Stalled != 0 {
		t.Errorf("Stalled = %d, want 0 (cancel failed)", stats.Stalled)
	}
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
	if !slskd.IsNotFound(cancelErr) {
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
