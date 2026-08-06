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
// takes the `continue` at downloading.go:360-363 and leaves the row untouched:
// no state change, no ReconcileStats increment, no job_event, and Tick returns
// nil. resolveDownloadingJob then sees a non-terminal transfer and returns
// (false, nil), so the job stays in DOWNLOADING holding a MaxActive slot.
//
// Nothing bounds that retry. Unlike the transient-rejection path, which spends
// MaxTransferRetries and then parks the job, this loop has no budget and no
// signature: module health stays green while the job advances forever.
//
// This test pins the current behaviour so the fix in #443 has something to
// flip. It shares its signature with the four-week wedge investigated in #442,
// but was NOT established as that wedge's cause — those jobs were freed by hand
// and re-entry deleted their transfer rows, so nothing distinguishes this path
// from the other silent ones.
func TestDownloadingWedgesSilentlyWhenRemoteCancelKeepsFailing(t *testing.T) {
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
	// deferred" does - so top-up cannot mask the wedge by sending b.flac.
	p.MaxInflightPerPeer = 1

	jobID, candID := seedActiveCandidate(t, st, 1, "bob", []core.CandidateFile{
		{Filename: "a.flac", Size: 100},
		{Filename: "b.flac", Size: 100},
	}, now)
	// a.flac was sent and is past its deadline; b.flac is still deferred.
	seedTransfer(t, st, candID, "bob", "a.flac", txfOpts{
		state: core.TransferQueued, slskdID: "g1", deadline: now.Add(-time.Hour), stampAt: now,
	})

	before, err := st.JobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}

	d := NewDownloading(p)
	const ticks = 20
	for i := 0; i < ticks; i++ {
		if err := d.Tick(ctx, now.Add(time.Duration(i)*p.Interval)); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}

	// Every tick reported success, and nothing moved.
	states := transferStatesFor(t, st, candID)
	if got := states["a.flac"].State; got != core.TransferQueued {
		t.Errorf("overdue transfer state = %v, want %v (unchanged)", got, core.TransferQueued)
	}
	if got := states["a.flac"].Retries; got != 0 {
		t.Errorf("overdue transfer retries = %d, want 0: this path spends no budget", got)
	}
	if got := states["b.flac"].State; got != core.TransferPending {
		t.Errorf("deferred transfer state = %v, want %v (never released)", got, core.TransferPending)
	}
	if len(net.cancelled) != 0 {
		t.Errorf("cancelled = %v, want none: every Cancel failed", net.cancelled)
	}

	// The job is still DOWNLOADING, still occupying a MaxActive slot.
	jobs, err := st.RunnableJobsInState(ctx, core.StateDownloading, now.Add(ticks*p.Interval), 10)
	if err != nil {
		t.Fatalf("RunnableJobsInState: %v", err)
	}
	var found bool
	for _, j := range jobs {
		if j.ID == jobID {
			found = true
		}
	}
	if !found {
		t.Fatalf("job %d left DOWNLOADING; the wedge this test pins no longer reproduces", jobID)
	}

	// And it left no trace at all - the reason four weeks could pass unnoticed.
	after, err := st.JobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("job_events grew from %d to %d; %d silent ticks should write nothing",
			len(before), len(after), ticks)
	}
}
