package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
)

// fakePeers is an in-memory PeerNetwork for tests.
type fakePeers struct {
	downloads []slskd.Transfer
	cancelled []string
	cancelErr error
}

func (f *fakePeers) ListDownloads(ctx context.Context) ([]slskd.Transfer, error) {
	return f.downloads, nil
}
func (f *fakePeers) Cancel(ctx context.Context, username, id string) error {
	f.cancelled = append(f.cancelled, id)
	return f.cancelErr
}

// fakeStore is an in-memory JobStore for tests.
type fakeStore struct {
	active   []core.Transfer
	overdue  []core.Transfer
	progress map[int64]core.TransferState
	attached map[int64]string
}

func (f *fakeStore) ActiveTransfers(ctx context.Context) ([]core.Transfer, error) {
	return f.active, nil
}
func (f *fakeStore) TransfersPastDeadline(ctx context.Context, now time.Time) ([]core.Transfer, error) {
	return f.overdue, nil
}
func (f *fakeStore) UpdateTransferProgress(ctx context.Context, id int64, state core.TransferState, done, total int64, now time.Time) error {
	if f.progress == nil {
		f.progress = map[int64]core.TransferState{}
	}
	f.progress[id] = state
	return nil
}
func (f *fakeStore) RetryTransfer(ctx context.Context, transferID int64, now time.Time) error {
	if f.progress == nil {
		f.progress = map[int64]core.TransferState{}
	}
	f.progress[transferID] = core.TransferPending
	return nil
}
func (f *fakeStore) AttachTransferID(ctx context.Context, transferID int64, slskdID string, now time.Time) error {
	if f.attached == nil {
		f.attached = map[int64]string{}
	}
	f.attached[transferID] = slskdID
	return nil
}
func (f *fakeStore) FindTransferByFallback(ctx context.Context, username, filename string) (core.Transfer, bool, error) {
	for _, t := range f.active {
		if t.Username == username && t.Filename == filename {
			return t, true, nil
		}
	}
	return core.Transfer{}, false, nil
}

// The core scenario: after a restart, one of our transfers is still live in
// slskd (adopt it), and one is past its deadline with no progress (cancel it).
// No orphan is left behind.
func TestReconcileAdoptsLiveAndCancelsOverdue(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		active: []core.Transfer{
			{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress},
		},
		overdue: []core.Transfer{
			{ID: 2, SlskdID: "g2", Username: "eve", Filename: "b.flac", State: core.TransferQueued},
		},
	}
	peers := &fakePeers{
		downloads: []slskd.Transfer{
			{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 60},
			{ID: "g2", Username: "eve", Filename: "b.flac", State: "Queued", Size: 100, BytesTransferred: 0},
		},
	}
	r := NewReconciler(peers, store, 3, time.Hour)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", stats.Adopted)
	}
	if stats.Cancelled != 1 {
		t.Errorf("Cancelled = %d, want 1", stats.Cancelled)
	}
	if len(peers.cancelled) != 1 || peers.cancelled[0] != "g2" {
		t.Errorf("expected g2 cancelled, got %v", peers.cancelled)
	}
	if store.progress[1] != core.TransferInProgress {
		t.Errorf("adopted transfer 1 should have progress recorded")
	}
}

// A transfer we recorded but slskd no longer knows about is "lost" (e.g. a
// slskd restart wiped its live list). With retry budget left, it goes back to
// PENDING for a resend instead of failing the attempt outright.
func TestReconcileRetriesLostWhenAbsentFromSlskd(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{
		active: []core.Transfer{
			{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress, Retries: 0},
		},
	}
	peers := &fakePeers{downloads: nil} // slskd forgot everything
	r := NewReconciler(peers, store, 3, time.Hour)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Retried != 1 {
		t.Errorf("Retried = %d, want 1", stats.Retried)
	}
	if stats.Lost != 0 {
		t.Errorf("Lost = %d, want 0", stats.Lost)
	}
	if store.progress[1] != core.TransferPending {
		t.Errorf("lost transfer with retries left should be requeued PENDING, got %v", store.progress[1])
	}
}

// Once a lost transfer's retry budget is exhausted, it is finally marked
// ERRORED instead of retrying forever.
func TestReconcileMarksLostWhenAbsentFromSlskdAndRetriesExhausted(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{
		active: []core.Transfer{
			{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress, Retries: 3},
		},
	}
	peers := &fakePeers{downloads: nil} // slskd forgot everything
	r := NewReconciler(peers, store, 3, time.Hour)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Lost != 1 {
		t.Errorf("Lost = %d, want 1", stats.Lost)
	}
	if stats.Retried != 0 {
		t.Errorf("Retried = %d, want 0", stats.Retried)
	}
	if store.progress[1] != core.TransferErrored {
		t.Errorf("lost transfer with retries exhausted should be marked ERRORED, got %v", store.progress[1])
	}
}

// A past-deadline transfer that ALSO appears in the active list (the realistic
// overlap, since past-deadline transfers are still non-terminal) must be
// processed exactly once: cancelled, not also adopted.
func TestReconcileOverlapCountedOnce(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := core.Transfer{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress}
	store := &fakeStore{active: []core.Transfer{tr}, overdue: []core.Transfer{tr}}
	peers := &fakePeers{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 10},
	}}
	r := NewReconciler(peers, store, 3, time.Hour)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Cancelled != 1 {
		t.Errorf("Cancelled = %d, want 1", stats.Cancelled)
	}
	if stats.Adopted != 0 {
		t.Errorf("Adopted = %d, want 0 (must not double-process the overlap)", stats.Adopted)
	}
	if store.progress[1] != core.TransferCancelled {
		t.Errorf("final state = %v, want CANCELLED", store.progress[1])
	}
}

// When Cancel fails, the transfer must NOT be marked cancelled — it stays
// non-terminal so the next reconcile pass retries it (no silent orphan).
func TestReconcileCancelFailureLeavesNonTerminal(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := core.Transfer{ID: 2, SlskdID: "g2", Username: "eve", Filename: "b.flac", State: core.TransferQueued}
	store := &fakeStore{overdue: []core.Transfer{tr}}
	peers := &fakePeers{
		downloads: []slskd.Transfer{{ID: "g2", Username: "eve", Filename: "b.flac", State: "Queued", Size: 100}},
		cancelErr: errors.New("peer offline"),
	}
	r := NewReconciler(peers, store, 3, time.Hour)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Cancelled != 0 {
		t.Errorf("Cancelled = %d, want 0 (cancel failed)", stats.Cancelled)
	}
	if _, marked := store.progress[2]; marked {
		t.Errorf("transfer 2 must not be marked terminal after cancel failure, got %v", store.progress[2])
	}
}

// A persisted transfer whose slskd id was lost (empty) but is still live gets its
// id backfilled when matched by (username, filename) in the active pass.
func TestReconcileBackfillsMissingID(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{active: []core.Transfer{
		{ID: 1, SlskdID: "", Username: "bob", Filename: "a.flac", State: core.TransferInProgress},
	}}
	peers := &fakePeers{downloads: []slskd.Transfer{
		{ID: "g-recovered", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 20},
	}}
	r := NewReconciler(peers, store, 3, time.Hour)
	if _, err := r.Reconcile(context.Background(), now); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if store.attached[1] != "g-recovered" {
		t.Errorf("expected slskd id backfilled to g-recovered, got %q", store.attached[1])
	}
}

// An empty-slskd_id transfer that is past deadline AND still live must be cancelled
// in slskd via the backfilled id — not silently marked cancelled (which would orphan it).
func TestReconcileOverdueEmptyIDCancelsViaBackfill(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := core.Transfer{ID: 2, SlskdID: "", Username: "eve", Filename: "b.flac", State: core.TransferQueued}
	store := &fakeStore{overdue: []core.Transfer{tr}, active: []core.Transfer{tr}}
	peers := &fakePeers{downloads: []slskd.Transfer{
		{ID: "g-live", Username: "eve", Filename: "b.flac", State: "Queued", Size: 100},
	}}
	r := NewReconciler(peers, store, 3, time.Hour)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if store.attached[2] != "g-live" {
		t.Errorf("expected backfill to g-live before cancel, got %q", store.attached[2])
	}
	if len(peers.cancelled) != 1 || peers.cancelled[0] != "g-live" {
		t.Errorf("expected cancel via backfilled live id g-live, got %v", peers.cancelled)
	}
	if stats.Cancelled != 1 {
		t.Errorf("Cancelled = %d, want 1", stats.Cancelled)
	}
}

// A peer rejection with a retryable reason (e.g. "Too many megabytes" - the
// peer's per-user queued-megabyte limit) and retries left must go back to
// PENDING for a later resend, not ERRORED. Marking it ERRORED would fail the
// whole attempt and discard a peer that actually has the album.
func TestReconcileRetriesRejectedTransferWhenRetryable(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{active: []core.Transfer{
		{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferQueued, Retries: 0},
	}}
	peers := &fakePeers{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "Completed, Rejected",
			Size: 100, BytesTransferred: 0, Exception: "Too many megabytes"},
	}}
	r := NewReconciler(peers, store, 3, time.Hour)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if store.progress[1] != core.TransferPending {
		t.Errorf("retryable rejection should be reset to PENDING, got %v", store.progress[1])
	}
	if stats.Retried != 1 {
		t.Errorf("Retried = %d, want 1", stats.Retried)
	}
}

// A rejection whose retry budget is spent must go terminal (ERRORED), so the
// attempt eventually fails instead of retrying forever.
func TestReconcileRejectedTransferErrorsWhenRetriesExhausted(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{active: []core.Transfer{
		{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferQueued, Retries: 3},
	}}
	peers := &fakePeers{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "Completed, Rejected",
			Size: 100, BytesTransferred: 0, Exception: "Too many megabytes"},
	}}
	r := NewReconciler(peers, store, 3, time.Hour)
	if _, err := r.Reconcile(context.Background(), now); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if store.progress[1] != core.TransferErrored {
		t.Errorf("exhausted retries should error out, got %v", store.progress[1])
	}
}

// A rejection whose reason is permanent (the peer will never serve the file)
// must go terminal immediately, regardless of remaining retry budget.
func TestReconcileRejectedTransferTerminalReasonDoesNotRetry(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{active: []core.Transfer{
		{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferQueued, Retries: 0},
	}}
	peers := &fakePeers{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "Completed, Rejected",
			Size: 100, BytesTransferred: 0, Exception: "File not shared."},
	}}
	r := NewReconciler(peers, store, 3, time.Hour)
	if _, err := r.Reconcile(context.Background(), now); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if store.progress[1] != core.TransferErrored {
		t.Errorf("permanent reason should error out without retrying, got %v", store.progress[1])
	}
}

// An IN_PROGRESS transfer whose last byte progress is older than the stall
// timeout is dead: it must be cancelled in slskd and retried (within budget),
// not left to wait out its enqueue-relative deadline.
func TestReconcileStalledTransferCancelsAndRetries(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	store := &fakeStore{active: []core.Transfer{
		{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac",
			State: core.TransferInProgress, Retries: 0, LastProgressAt: &stale},
	}}
	peers := &fakePeers{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 40},
	}}
	r := NewReconciler(peers, store, 3, time.Hour)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(peers.cancelled) != 1 || peers.cancelled[0] != "g1" {
		t.Errorf("expected stalled transfer g1 cancelled, got %v", peers.cancelled)
	}
	if store.progress[1] != core.TransferPending {
		t.Errorf("stalled transfer should be reset to PENDING, got %v", store.progress[1])
	}
	if stats.Stalled != 1 {
		t.Errorf("Stalled = %d, want 1", stats.Stalled)
	}
	if stats.Adopted != 0 {
		t.Errorf("Adopted = %d, want 0 (must not also adopt the stalled transfer)", stats.Adopted)
	}
}

// A stalled transfer whose retry budget is spent must go terminal (ERRORED) so
// the attempt fails and moves on to another candidate rather than retrying forever.
func TestReconcileStalledTransferErrorsWhenRetriesExhausted(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	store := &fakeStore{active: []core.Transfer{
		{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac",
			State: core.TransferInProgress, Retries: 3, LastProgressAt: &stale},
	}}
	peers := &fakePeers{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 40},
	}}
	r := NewReconciler(peers, store, 3, time.Hour)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(peers.cancelled) != 1 || peers.cancelled[0] != "g1" {
		t.Errorf("expected stalled transfer g1 cancelled, got %v", peers.cancelled)
	}
	if store.progress[1] != core.TransferErrored {
		t.Errorf("exhausted stall retries should error out, got %v", store.progress[1])
	}
	if stats.Stalled != 1 {
		t.Errorf("Stalled = %d, want 1", stats.Stalled)
	}
}

// A transfer that made byte progress within the stall timeout is healthy: it
// must be adopted normally, never cancelled.
func TestReconcileFreshProgressNotStalled(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Minute)
	store := &fakeStore{active: []core.Transfer{
		{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac",
			State: core.TransferInProgress, LastProgressAt: &fresh},
	}}
	peers := &fakePeers{downloads: []slskd.Transfer{
		{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 40},
	}}
	r := NewReconciler(peers, store, 3, time.Hour)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(peers.cancelled) != 0 {
		t.Errorf("healthy transfer must not be cancelled, got %v", peers.cancelled)
	}
	if stats.Stalled != 0 {
		t.Errorf("Stalled = %d, want 0", stats.Stalled)
	}
	if stats.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", stats.Adopted)
	}
	if store.progress[1] != core.TransferInProgress {
		t.Errorf("healthy transfer should record IN_PROGRESS, got %v", store.progress[1])
	}
}

// If cancelling a stalled transfer in slskd fails, it must be left untouched so
// the next pass retries the cancel (no orphaned in-flight download).
func TestReconcileStalledCancelFailureLeavesUntouched(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	store := &fakeStore{active: []core.Transfer{
		{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac",
			State: core.TransferInProgress, LastProgressAt: &stale},
	}}
	peers := &fakePeers{
		downloads: []slskd.Transfer{
			{ID: "g1", Username: "bob", Filename: "a.flac", State: "InProgress", Size: 100, BytesTransferred: 40},
		},
		cancelErr: errors.New("peer offline"),
	}
	r := NewReconciler(peers, store, 3, time.Hour)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(peers.cancelled) != 1 {
		t.Errorf("expected a cancel attempt, got %v", peers.cancelled)
	}
	if _, marked := store.progress[1]; marked {
		t.Errorf("transfer must not be modified after cancel failure, got %v", store.progress[1])
	}
	if stats.Stalled != 0 {
		t.Errorf("Stalled = %d, want 0 (cancel failed)", stats.Stalled)
	}
}
