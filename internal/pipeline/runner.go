// Package pipeline provides the scheduling framework shared by all pipeline
// modules: one goroutine per module, a per-tick timeout, panic recovery, and
// separate liveness/readiness state for each module.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// ReadinessFailureThreshold is the number of consecutive failed ticks that
	// makes a module unready. A later successful tick immediately recovers it.
	ReadinessFailureThreshold    = 3
	defaultRunnerShutdownTimeout = 10 * time.Second
)

// ErrShutdownTimeout is returned when a module ignores cancellation long
// enough to exhaust the runner's bounded shutdown grace period.
var ErrShutdownTimeout = errors.New("pipeline module shutdown timed out")

// Module is one independently-scheduled unit of pipeline work.
type Module interface {
	Name() string
	Interval() time.Duration
	Tick(ctx context.Context, now time.Time) error
}

// ModuleStatus is an immutable snapshot of one module's runtime state.
// LastAttempt records tick start while LastCompleted changes only after Tick
// returns. LastError is retained after recovery for diagnostics;
// ConsecutiveFailures resets to zero on the first successful tick.
type ModuleStatus struct {
	LastAttempt         time.Time
	LastCompleted       time.Time
	LastSuccess         time.Time
	LastErrorAt         time.Time
	LastError           string
	ConsecutiveFailures int
	StaleDeadline       time.Time
	Live                bool
	Ready               bool
}

type moduleState struct {
	module Module
	status atomic.Pointer[ModuleStatus]
}

// Runner drives a fixed set of validated modules until its context is
// cancelled. Liveness means modules continue attempting work; readiness also
// requires a success and fails after sustained tick failures.
type Runner struct {
	logger          *slog.Logger
	tickTimeout     time.Duration
	shutdownTimeout time.Duration
	states          []*moduleState
}

// NewRunner validates and constructs a Runner. Names must be nonblank,
// already trimmed, and unique; intervals and tickTimeout must be positive.
func NewRunner(logger *slog.Logger, tickTimeout time.Duration, modules ...Module) (*Runner, error) {
	if tickTimeout <= 0 {
		return nil, fmt.Errorf("tick timeout must be > 0")
	}
	states := make([]*moduleState, len(modules))
	names := make(map[string]struct{}, len(modules))
	for i, m := range modules {
		if m == nil || (reflect.ValueOf(m).Kind() == reflect.Pointer && reflect.ValueOf(m).IsNil()) {
			return nil, fmt.Errorf("module %d is nil", i)
		}
		name := m.Name()
		if name == "" || strings.TrimSpace(name) != name {
			return nil, fmt.Errorf("module %d name %q must be nonblank and trimmed", i, name)
		}
		if _, exists := names[name]; exists {
			return nil, fmt.Errorf("duplicate module name %q", name)
		}
		if interval := m.Interval(); interval <= 0 {
			return nil, fmt.Errorf("module %q interval must be > 0", name)
		}
		names[name] = struct{}{}
		states[i] = &moduleState{module: m}
		states[i].status.Store(&ModuleStatus{})
	}
	return &Runner{
		logger:          logger,
		tickTimeout:     tickTimeout,
		shutdownTimeout: defaultRunnerShutdownTimeout,
		states:          states,
	}, nil
}

// Run starts one goroutine per module and blocks until cancellation. Shutdown
// is bounded even if a module ignores its tick context.
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
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(r.shutdownTimeout):
		return ErrShutdownTimeout
	}
}

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

func (r *Runner) runTick(ctx context.Context, st *moduleState) {
	attemptedAt := time.Now()
	previous := *st.status.Load()
	previous.LastAttempt = attemptedAt
	st.status.Store(&previous)

	tickCtx, cancel := context.WithTimeout(ctx, r.tickTimeout)
	defer cancel()

	var tickErr error
	defer func() {
		if rec := recover(); rec != nil {
			tickErr = fmt.Errorf("panic: %v", rec)
			if r.logger != nil {
				r.logger.Error("tick panicked", "module", st.module.Name(), "panic", rec)
			}
		}
		r.recordOutcome(st, attemptedAt, tickErr)
	}()

	tickErr = st.module.Tick(tickCtx, attemptedAt)
	if tickErr != nil && r.logger != nil {
		r.logger.Error("tick failed", "module", st.module.Name(), "err", tickErr)
	}
}

func (r *Runner) recordOutcome(st *moduleState, attemptedAt time.Time, tickErr error) {
	completedAt := time.Now()
	status := *st.status.Load()
	status.LastAttempt = attemptedAt
	status.LastCompleted = completedAt
	if tickErr == nil {
		status.LastSuccess = completedAt
		status.ConsecutiveFailures = 0
	} else {
		status.LastErrorAt = completedAt
		status.LastError = tickErr.Error()
		status.ConsecutiveFailures++
	}
	st.status.Store(&status)
}

// Health returns a point-in-time copy of every module's runtime status. The
// liveness fields use the same per-module deadline as Live, so API consumers do
// not need to approximate the runner's interval-aware policy.
func (r *Runner) Health() map[string]ModuleStatus {
	now := time.Now()
	out := make(map[string]ModuleStatus, len(r.states))
	for _, st := range r.states {
		status := *st.status.Load()
		if !status.LastAttempt.IsZero() {
			status.StaleDeadline = status.LastAttempt.Add(st.module.Interval()*3 + r.tickTimeout)
			status.Live = !now.After(status.StaleDeadline)
		}
		status.Ready = status.Live && !status.LastSuccess.IsZero() && status.ConsecutiveFailures < ReadinessFailureThreshold
		out[st.module.Name()] = status
	}
	return out
}

// Live reports whether every module has attempted a tick and no attempt is
// stale beyond Interval()*3 + tickTimeout. Returned errors do not affect
// liveness; a wedged Tick eventually does.
func (r *Runner) Live() bool {
	for _, status := range r.Health() {
		if !status.Live {
			return false
		}
	}
	return true
}

// Ready reports liveness plus successful initialization of every module.
// Three consecutive errors or panics make a module unready; one success
// immediately recovers it.
func (r *Runner) Ready() bool {
	for _, status := range r.Health() {
		if !status.Ready {
			return false
		}
	}
	return true
}

// Healthy is kept as a compatibility alias for liveness.
func (r *Runner) Healthy() bool { return r.Live() }
