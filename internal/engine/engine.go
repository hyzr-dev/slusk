package engine

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/lidarr"
)

// MetricsSink receives reconciliation metrics. A nil sink is a no-op, so the
// engine never depends on the observ package directly.
type MetricsSink interface {
	IncReconcile()
	SetUnknownTransfers(n int)
	SetDownloadsActive(n int)
}

// Params configures an Engine.
type Params struct {
	Reconciler   *Reconciler
	Discoverer   *Discoverer // nil → discovery loop is disabled
	StatusPoll   time.Duration
	LidarrPoll   time.Duration
	TickInterval time.Duration
	Logger       *slog.Logger // nil → reconcile errors are not logged
	Metrics      MetricsSink  // nil → metrics are not fed
}

// Engine runs the scheduler loops until its context is cancelled.
type Engine struct {
	p              Params
	reconcileCount atomic.Int64
	discoverCount  atomic.Int64
	wanted         map[int64]lidarr.WantedAlbum // cached by syncWantedOnce, consumed by advanceOnce
}

// New constructs an Engine.
func New(p Params) *Engine {
	return &Engine{p: p}
}

// ReconcileCount reports how many reconcile passes have run (for tests/metrics).
func (e *Engine) ReconcileCount() int64 {
	return e.reconcileCount.Load()
}

// DiscoverCount reports how many discovery passes have run.
func (e *Engine) DiscoverCount() int64 { return e.discoverCount.Load() }

// Run starts the reconcile loop and, when a Discoverer is configured, the
// discovery loop, then blocks until ctx is cancelled, at which point it
// returns nil. In-flight downloads are left in slskd and re-adopted on the
// next start via reconciliation, so no draining is required for a clean
// shutdown.
func (e *Engine) Run(ctx context.Context) error {
	statusTicker := time.NewTicker(e.p.StatusPoll)
	defer statusTicker.Stop()

	var lidarrC, tickC <-chan time.Time
	if e.p.Discoverer != nil {
		lidarrTicker := time.NewTicker(e.p.LidarrPoll)
		defer lidarrTicker.Stop()
		lidarrC = lidarrTicker.C
		discoveryTicker := time.NewTicker(e.p.TickInterval)
		defer discoveryTicker.Stop()
		tickC = discoveryTicker.C
		e.syncWantedOnce(ctx) // run once immediately on startup
		e.advanceOnce(ctx)
	}
	e.reconcileOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-statusTicker.C:
			e.reconcileOnce(ctx)
		case <-lidarrC:
			e.syncWantedOnce(ctx)
		case <-tickC:
			e.advanceOnce(ctx)
		}
	}
}

// syncWantedOnce refreshes the cached wanted-missing list from Lidarr. It runs
// on LidarrPoll, independent of how often the state machine advances.
func (e *Engine) syncWantedOnce(ctx context.Context) {
	wanted, err := e.p.Discoverer.SyncWanted(ctx, time.Now().UTC())
	if err != nil {
		if e.p.Logger != nil {
			e.p.Logger.Error("sync wanted failed", "err", err)
		}
		return
	}
	e.wanted = wanted
}

// advanceOnce takes every job one transition forward using the cached wanted
// list. It runs on TickInterval, independent of how often Lidarr is polled.
func (e *Engine) advanceOnce(ctx context.Context) {
	if e.wanted == nil {
		if e.p.Logger != nil {
			e.p.Logger.Info("skipping advance, wanted list not yet synced")
		}
		return
	}
	if err := e.p.Discoverer.Advance(ctx, e.wanted, time.Now().UTC()); err != nil {
		if e.p.Logger != nil {
			e.p.Logger.Error("discovery advance failed", "err", err)
		}
	}
	e.discoverCount.Add(1)
}

func (e *Engine) reconcileOnce(ctx context.Context) {
	stats, err := e.p.Reconciler.Reconcile(ctx, time.Now().UTC())
	if e.p.Metrics != nil {
		e.p.Metrics.IncReconcile()
	}
	if err != nil {
		if e.p.Logger != nil {
			e.p.Logger.Error("reconcile failed", "err", err)
		}
	} else {
		if e.p.Metrics != nil {
			e.p.Metrics.SetUnknownTransfers(stats.Unknown)
			e.p.Metrics.SetDownloadsActive(stats.Adopted)
		}
		// Log a heartbeat only when the pass actually changed something, so a quiet
		// tick stays silent but real transfer activity is visible.
		if e.p.Logger != nil && (stats.Adopted+stats.Completed+stats.Cancelled+stats.Lost+stats.Stalled) > 0 {
			e.p.Logger.Info("reconciled transfers",
				"adopted", stats.Adopted, "completed", stats.Completed,
				"cancelled", stats.Cancelled, "lost", stats.Lost,
				"stalled", stats.Stalled, "unknown", stats.Unknown)
		}
	}
	e.reconcileCount.Add(1)
}
