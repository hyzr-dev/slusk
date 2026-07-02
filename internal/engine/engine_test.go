package engine

import (
	"context"
	"testing"
	"time"
)

// TestRunReconcilesThenStopsOnContextCancel verifies the loop ticks at least
// once and returns promptly when the context is cancelled (graceful shutdown).
func TestRunReconcilesThenStopsOnContextCancel(t *testing.T) {
	store := &fakeStore{}
	peers := &fakePeers{}
	r := NewReconciler(peers, store, 3)

	eng := New(Params{
		Reconciler: r,
		StatusPoll: 10 * time.Millisecond,
		LidarrPoll: time.Hour, // irrelevant to this test
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()

	time.Sleep(25 * time.Millisecond) // allow at least one tick
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancel (no graceful shutdown)")
	}
	if eng.ReconcileCount() == 0 {
		t.Errorf("expected at least one reconcile tick")
	}
}

// TestRunTicksDiscoveryLoop verifies the discovery loop ticks independently
// of the reconcile loop when a Discoverer is configured.
func TestRunTicksDiscoveryLoop(t *testing.T) {
	store := &fakeStore{}
	peers := &fakePeers{}
	rec := NewReconciler(peers, store, 3)

	music := &fakeMusic{}
	searcher := &fakeSearcher{}
	dp, _ := newDiscoParams(t, music, searcher)
	disco := NewDiscoverer(dp)

	eng := New(Params{
		Reconciler:   rec,
		Discoverer:   disco,
		StatusPoll:   time.Hour,
		LidarrPoll:   10 * time.Millisecond,
		TickInterval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	if eng.DiscoverCount() == 0 {
		t.Error("expected at least one discovery tick")
	}
}

// TestRunAdvancesIndependentlyOfLidarrPoll verifies the discovery state
// machine (Advance) can tick multiple times between SyncWanted calls, i.e.
// TickInterval and LidarrPoll drive separate tickers.
func TestRunAdvancesIndependentlyOfLidarrPoll(t *testing.T) {
	store := &fakeStore{}
	peers := &fakePeers{}
	rec := NewReconciler(peers, store, 3)

	music := &fakeMusic{}
	searcher := &fakeSearcher{}
	dp, _ := newDiscoParams(t, music, searcher)
	disco := NewDiscoverer(dp)

	eng := New(Params{
		Reconciler:   rec,
		Discoverer:   disco,
		StatusPoll:   time.Hour,
		LidarrPoll:   time.Hour, // effectively never re-fires after startup
		TickInterval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
	time.Sleep(55 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	if eng.DiscoverCount() < 2 {
		t.Errorf("expected multiple discovery ticks driven by TickInterval alone, got %d", eng.DiscoverCount())
	}
}
