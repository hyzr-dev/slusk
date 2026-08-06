package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// A past-deadline transfer that is still live in the backend can only be
// recorded CANCELLED after the backend confirms the cancellation - otherwise we
// orphan an in-flight download. When that remote Cancel keeps failing, reconcile
// retries on the next pass rather than recording anything.
//
// That retry is deliberately bounded (#443). Unbounded it has no signature at
// all: the row never leaves QUEUED, no ReconcileStats field increments so the
// heartbeat stays quiet, resolveDownloadingJob keeps returning (false, nil), and
// the job holds a MaxActive slot forever while module health reads green. These
// two tests pin both halves of the bound - the retry that still happens, and the
// park that ends it.

// Within the grace period the original behaviour is unchanged: keep retrying and
// touch nothing, because a briefly unreachable peer deserves the chance to come
// back before its job is handed to a human.
func TestDownloadingRetriesUncancellableTransferWithinGrace(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 8, 5, 20, 0, 0, time.UTC)

	// The peer still reports the file as queued on every poll, so matchLive
	// keeps matching it and reconcile keeps taking the cancel path.
	net := &fakeNetwork{
		downloads: []core.RemoteTransfer{
			{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferQueued, Size: 100},
		},
		cancelErr: errors.New("peer unreachable"),
	}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	// One in flight fills the budget, exactly as production's "3 sent, N
	// deferred" does - so top-up cannot mask the outcome by sending b.flac.
	p.MaxInflightPerPeer = 1

	jobID, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{
		{Filename: "a.flac", Size: 100},
		{Filename: "b.flac", Size: 100},
	}, now)
	// Overdue by a minute against an hour of grace: 20 ticks stay well inside it.
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{
		state: core.TransferQueued, slskdID: "g1", deadline: now.Add(-time.Minute), stampAt: now,
	})

	d := NewDownloading(p)
	const ticks = 20
	for i := 0; i < ticks; i++ {
		if err := d.Tick(ctx, now.Add(time.Duration(i)*p.Interval)); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}

	states := transferStatesFor(t, st, candID)
	if got := states["a.flac"].State; got != core.TransferQueued {
		t.Errorf("overdue transfer state = %v, want %v (still retrying)", got, core.TransferQueued)
	}
	if got := states["b.flac"].State; got != core.TransferPending {
		t.Errorf("deferred transfer state = %v, want %v", got, core.TransferPending)
	}
	if len(net.cancelled) != 0 {
		t.Errorf("cancelled = %v, want none: every Cancel failed", net.cancelled)
	}
	assertJobInState(t, st, jobID, core.StateDownloading, now.Add(ticks*p.Interval))
}

// Past the grace period the retry ends: the transfer is recorded ERRORED and its
// job is PARKED for a human, so it stops occupying a MaxActive slot silently.
// stats.Parked is what makes the reconcile heartbeat speak - without it the fix
// would end the wedge but keep the silence.
func TestDownloadingParksUncancellableTransferPastGrace(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 8, 5, 20, 0, 0, time.UTC)

	net := &fakeNetwork{
		downloads: []core.RemoteTransfer{
			{ID: "g1", Username: "bob", Filename: "a.flac", State: core.TransferQueued, Size: 100},
		},
		cancelErr: errors.New("peer unreachable"),
	}
	p, st := newDownloadingParams(t, net, &fakeSearcher{})
	p.MaxInflightPerPeer = 1

	jobID, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{
		{Filename: "a.flac", Size: 100},
		{Filename: "b.flac", Size: 100},
	}, now)
	// Overdue by two hours against an hour of grace.
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{
		state: core.TransferQueued, slskdID: "g1", deadline: now.Add(-2 * time.Hour), stampAt: now,
	})

	d := NewDownloading(p)
	stats, err := d.reconcile(ctx, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Parked != 1 {
		t.Errorf("Parked = %d, want 1: the heartbeat only speaks when a stat moves", stats.Parked)
	}

	states := transferStatesFor(t, st, candID)
	if got := states["a.flac"].State; got != core.TransferErrored {
		t.Errorf("uncancellable transfer state = %v, want %v", got, core.TransferErrored)
	}
	// Never handed to the backend, so nothing was cancelled or purged there - the
	// whole point is that the backend is the part we cannot reach.
	if len(net.cancelled) != 0 {
		t.Errorf("cancelled = %v, want none", net.cancelled)
	}
	if len(net.removed) != 0 {
		t.Errorf("removed = %v, want none: we never confirmed the cancellation", net.removed)
	}
	assertJobInState(t, st, jobID, core.StateParked, now)
}

// assertJobInState fails unless the job is runnable in exactly the given state.
func assertJobInState(t *testing.T, st jobStater, jobID int64, want core.AlbumJobState, at time.Time) {
	t.Helper()
	jobs, err := st.RunnableJobsInState(context.Background(), want, at, 50)
	if err != nil {
		t.Fatalf("RunnableJobsInState(%s): %v", want, err)
	}
	for _, j := range jobs {
		if j.ID == jobID {
			return
		}
	}
	t.Fatalf("job %d is not in %s", jobID, want)
}

// jobStater is the sliver of *store.Store assertJobInState needs.
type jobStater interface {
	RunnableJobsInState(ctx context.Context, state core.AlbumJobState, now time.Time, limit int) ([]core.AlbumJob, error)
}
