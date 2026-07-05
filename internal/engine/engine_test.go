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
	r := NewReconciler(peers, store, 3, time.Hour)

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
	rec := NewReconciler(peers, store, 3, time.Hour)

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
	rec := NewReconciler(peers, store, 3, time.Hour)

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

// TestHealthyReflectsReconcileProgress verifies Healthy is false before the
// first reconcile pass, true shortly after one completes, and false again
// once staleAfter has elapsed with no further passes (simulating a hung
// reconcile call blocking the loop without crashing the process).
func TestHealthyReflectsReconcileProgress(t *testing.T) {
	store := &fakeStore{}
	peers := &fakePeers{}
	r := NewReconciler(peers, store, 3, time.Hour)

	eng := New(Params{
		Reconciler: r,
		StatusPoll: time.Hour, // won't fire again during this test
		LidarrPoll: time.Hour,
	})

	if eng.Healthy(time.Second) {
		t.Error("expected unhealthy before any reconcile pass has run")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	time.Sleep(25 * time.Millisecond) // allow the immediate startup reconcile pass

	if !eng.Healthy(time.Second) {
		t.Error("expected healthy right after a reconcile pass completed")
	}
	if eng.Healthy(0) {
		t.Error("expected unhealthy once staleAfter has already elapsed")
	}
}

// TestReconcileOnceBoundedByTickTimeout verifies a reconcile pass stuck on an
// unresponsive call (e.g. a silently dead pooled DB/network connection) is
// aborted by a per-tick timeout rather than hanging the engine loop forever.
// Without this bound, one stuck call freezes every loop iteration permanently
// since reconcileOnce runs synchronously inside the engine's single goroutine.
func TestReconcileOnceBoundedByTickTimeout(t *testing.T) {
	store := &fakeStore{}
	peers := &fakePeers{hang: true}
	r := NewReconciler(peers, store, 3, time.Hour)

	eng := New(Params{
		Reconciler:  r,
		StatusPoll:  time.Hour, // won't fire again during this test
		LidarrPoll:  time.Hour,
		TickTimeout: 20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.After(200 * time.Millisecond)
	for eng.ReconcileCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("reconcileOnce did not return within the tick timeout; stuck call hung the engine loop")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
