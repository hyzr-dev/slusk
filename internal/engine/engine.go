package engine

import (
	"context"
	"sync/atomic"
	"time"
)

// Params configures an Engine.
type Params struct {
	Reconciler *Reconciler
	StatusPoll time.Duration
	LidarrPoll time.Duration
}

// Engine runs the scheduler loops until its context is cancelled.
type Engine struct {
	p              Params
	reconcileCount atomic.Int64
}

// New constructs an Engine.
func New(p Params) *Engine {
	return &Engine{p: p}
}

// ReconcileCount reports how many reconcile passes have run (for tests/metrics).
func (e *Engine) ReconcileCount() int64 {
	return e.reconcileCount.Load()
}

// Run starts the reconcile loop (and, later, the discovery loop) and blocks
// until ctx is cancelled, at which point it returns nil. In-flight downloads
// are left in slskd and re-adopted on the next start via reconciliation, so no
// draining is required for a clean shutdown.
func (e *Engine) Run(ctx context.Context) error {
	statusTicker := time.NewTicker(e.p.StatusPoll)
	defer statusTicker.Stop()

	// Reconcile once immediately on startup before entering the loop.
	e.reconcileOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-statusTicker.C:
			e.reconcileOnce(ctx)
		}
	}
}

func (e *Engine) reconcileOnce(ctx context.Context) {
	_, _ = e.p.Reconciler.Reconcile(ctx, time.Now().UTC())
	e.reconcileCount.Add(1)
}
