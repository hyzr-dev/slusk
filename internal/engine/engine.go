package engine

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
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
	Reconciler *Reconciler
	Discoverer *Discoverer // nil → discovery loop is disabled
	StatusPoll time.Duration
	LidarrPoll time.Duration
	Logger     *slog.Logger // nil → reconcile errors are not logged
	Metrics    MetricsSink  // nil → metrics are not fed
}

// Engine runs the scheduler loops until its context is cancelled.
type Engine struct {
	p              Params
	reconcileCount atomic.Int64
	discoverCount  atomic.Int64
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

	var lidarrC <-chan time.Time
	if e.p.Discoverer != nil {
		lidarrTicker := time.NewTicker(e.p.LidarrPoll)
		defer lidarrTicker.Stop()
		lidarrC = lidarrTicker.C
		e.discoverOnce(ctx) // run once immediately on startup
	}
	e.reconcileOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-statusTicker.C:
			e.reconcileOnce(ctx)
		case <-lidarrC:
			e.discoverOnce(ctx)
		}
	}
}

func (e *Engine) discoverOnce(ctx context.Context) {
	if err := e.p.Discoverer.RunOnce(ctx, time.Now().UTC()); err != nil {
		if e.p.Logger != nil {
			e.p.Logger.Error("discovery failed", "err", err)
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
	} else if e.p.Metrics != nil {
		e.p.Metrics.SetUnknownTransfers(stats.Unknown)
		e.p.Metrics.SetDownloadsActive(stats.Adopted)
	}
	e.reconcileCount.Add(1)
}
