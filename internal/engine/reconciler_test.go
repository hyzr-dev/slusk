package engine

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
)

// fakePeers is an in-memory PeerNetwork for tests.
type fakePeers struct {
	downloads []slskd.Transfer
	cancelled []string
}

func (f *fakePeers) ListDownloads(ctx context.Context) ([]slskd.Transfer, error) {
	return f.downloads, nil
}
func (f *fakePeers) Cancel(ctx context.Context, username, id string) error {
	f.cancelled = append(f.cancelled, id)
	return nil
}

// fakeStore is an in-memory JobStore for tests.
type fakeStore struct {
	active   []core.Transfer
	overdue  []core.Transfer
	progress map[int64]core.TransferState
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
	r := NewReconciler(peers, store)
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

// A transfer we recorded but slskd no longer knows about is "lost".
func TestReconcileMarksLostWhenAbsentFromSlskd(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{
		active: []core.Transfer{
			{ID: 1, SlskdID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferInProgress},
		},
	}
	peers := &fakePeers{downloads: nil} // slskd forgot everything
	r := NewReconciler(peers, store)
	stats, err := r.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Lost != 1 {
		t.Errorf("Lost = %d, want 1", stats.Lost)
	}
	if store.progress[1] != core.TransferErrored {
		t.Errorf("lost transfer should be marked ERRORED, got %v", store.progress[1])
	}
}
