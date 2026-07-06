// Package pipeline provides the scheduling framework shared by all pipeline
// modules (queue-poll, search-dispatch, etc., added in later tasks): one
// goroutine per module, a per-tick timeout, panic recovery, and per-module
// liveness for /healthz. It has no database dependency of its own; each
// Module owns whatever state it needs.
package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Module is one independently-scheduled unit of pipeline work. Each module
// owns its own polling cadence and reports failures per-tick rather than
// crashing the runner - a persistently erroring module stays visible in
// logs/metrics without taking down its siblings.
type Module interface {
	// Name identifies the module in logs and in Health()'s map.
	Name() string
	// Interval is how often Tick runs, in addition to an immediate first
	// tick when the runner starts.
	Interval() time.Duration
	// Tick performs one unit of work. ctx carries the runner's per-tick
	// timeout; a well-behaved Tick should return promptly once ctx is done,
	// though the runner does not depend on that for correctness - it only
	// depends on Tick eventually returning at all (see Healthy).
	Tick(ctx context.Context, now time.Time) error
}

// moduleState pairs a Module with its last-completed-tick timestamp.
// lastTick is an atomic UnixNano so Health/Healthy never block on - or
// race with - a module's own goroutine mid-tick.
type moduleState struct {
	module   Module
	lastTick atomic.Int64 // UnixNano; 0 = never completed a tick
}

// Runner drives a fixed set of modules, one goroutine each, until its
// context is cancelled. Health tracks liveness (did the module's Tick
// return recently), not success: a module that keeps erroring is healthy;
// a module wedged inside a Tick call that ignores ctx cancellation is not,
// since that is precisely the failure /healthz needs to surface.
type Runner struct {
	logger      *slog.Logger
	tickTimeout time.Duration
	states      []*moduleState
}

// NewRunner constructs a Runner over the given modules. tickTimeout bounds
// each Tick call via context.WithTimeout; it cannot forcibly stop a Tick
// that ignores ctx (Go has no goroutine preemption), so a hung Tick still
// blocks its own module's goroutine until it returns - Healthy() is how
// that condition becomes visible.
func NewRunner(logger *slog.Logger, tickTimeout time.Duration, modules ...Module) *Runner {
	states := make([]*moduleState, len(modules))
	for i, m := range modules {
		states[i] = &moduleState{module: m}
	}
	return &Runner{logger: logger, tickTimeout: tickTimeout, states: states}
}

// Run starts one goroutine per module (immediate first tick, then one per
// the module's Interval), blocks until ctx is cancelled, then waits for
// every module goroutine to return before completing.
func (r *Runner) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, st := range r.states {
		wg.Add(1)
		go func(st *moduleState) {
			defer wg.Done()
			r.loop(ctx, st)
		}(st)
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// loop runs one module: an immediate first tick, then one per Interval,
// until ctx is done.
func (r *Runner) loop(ctx context.Context, st *moduleState) {
	r.runTick(ctx, st)

	ticker := time.NewTicker(st.module.Interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runTick(ctx, st)
		}
	}
}

// runTick executes one bounded, panic-safe tick and records lastTick
// afterwards - including when Tick errors or panics, since health measures
// liveness (it returned at all), not success. lastTick is only stored once
// Tick has actually returned, so a Tick that hangs past tickTimeout (by
// ignoring ctx) leaves lastTick stale for as long as it is blocked.
func (r *Runner) runTick(ctx context.Context, st *moduleState) {
	tickCtx, cancel := context.WithTimeout(ctx, r.tickTimeout)
	defer cancel()

	defer func() {
		if rec := recover(); rec != nil {
			if r.logger != nil {
				r.logger.Error("tick panicked", "module", st.module.Name(), "panic", rec)
			}
		}
		st.lastTick.Store(time.Now().UnixNano())
	}()

	if err := st.module.Tick(tickCtx, time.Now()); err != nil {
		if r.logger != nil {
			r.logger.Error("tick failed", "module", st.module.Name(), "err", err)
		}
	}
}

// Health reports each module's last completed tick, keyed by Name(). A zero
// time means the module has never completed a tick.
func (r *Runner) Health() map[string]time.Time {
	out := make(map[string]time.Time, len(r.states))
	for _, st := range r.states {
		if ns := st.lastTick.Load(); ns != 0 {
			out[st.module.Name()] = time.Unix(0, ns)
		} else {
			out[st.module.Name()] = time.Time{}
		}
	}
	return out
}

// Healthy reports whether every module has completed at least one tick and
// none is stale beyond its own staleness window (Interval()*3 +
// tickTimeout). It is false until every module has ticked at least once.
func (r *Runner) Healthy() bool {
	now := time.Now()
	for _, st := range r.states {
		ns := st.lastTick.Load()
		if ns == 0 {
			return false
		}
		staleAfter := st.module.Interval()*3 + r.tickTimeout
		if now.Sub(time.Unix(0, ns)) > staleAfter {
			return false
		}
	}
	return true
}
