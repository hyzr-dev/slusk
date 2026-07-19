package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type tickRecorder struct {
	name     string
	interval time.Duration
	ticks    atomic.Int64
	err      error
	panics   bool
	block    chan struct{}
}

func (t *tickRecorder) Name() string            { return t.name }
func (t *tickRecorder) Interval() time.Duration { return t.interval }
func (t *tickRecorder) Tick(_ context.Context, _ time.Time) error {
	if t.panics {
		panic("boom: " + t.name)
	}
	if t.block != nil {
		<-t.block
	}
	t.ticks.Add(1)
	return t.err
}

type sequenceModule struct {
	name        string
	interval    time.Duration
	mu          sync.Mutex
	results     []error
	calls       int
	blockBefore int
	release     <-chan struct{}
}

func (m *sequenceModule) Name() string            { return m.name }
func (m *sequenceModule) Interval() time.Duration { return m.interval }
func (m *sequenceModule) Tick(_ context.Context, _ time.Time) error {
	m.mu.Lock()
	idx := m.calls
	m.calls++
	m.mu.Unlock()
	if m.release != nil && idx == m.blockBefore {
		<-m.release
	}
	if idx >= len(m.results) {
		return nil
	}
	return m.results[idx]
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func newTestRunner(t *testing.T, timeout time.Duration, modules ...Module) *Runner {
	t.Helper()
	r, err := NewRunner(discardLogger(), timeout, modules...)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not met before timeout")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunnerTicksEveryModuleIndependently(t *testing.T) {
	m1 := &tickRecorder{name: "m1", interval: 10 * time.Millisecond}
	m2 := &tickRecorder{name: "m2", interval: 10 * time.Millisecond}
	r := newTestRunner(t, 50*time.Millisecond, m1, m2)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := m1.ticks.Load(); got < 5 {
		t.Errorf("m1 ticks = %d, want >= 5", got)
	}
	if got := m2.ticks.Load(); got < 5 {
		t.Errorf("m2 ticks = %d, want >= 5", got)
	}
}

func TestRunnerTracksRepeatedErrorsAndPanic(t *testing.T) {
	panicker := &tickRecorder{name: "panicker", interval: 5 * time.Millisecond, panics: true}
	erroring := &tickRecorder{name: "erroring", interval: 5 * time.Millisecond, err: errors.New("boom")}
	r := newTestRunner(t, 20*time.Millisecond, panicker, erroring)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitFor(t, time.Second, func() bool {
		h := r.Health()
		return h["panicker"].ConsecutiveFailures >= 3 && h["erroring"].ConsecutiveFailures >= 3
	})

	if !r.Live() {
		t.Error("repeated returned failures must not fail liveness")
	}
	if r.Ready() {
		t.Error("three consecutive failures must fail readiness")
	}
	for _, name := range []string{"panicker", "erroring"} {
		status := r.Health()[name]
		if status.LastAttempt.IsZero() || status.LastErrorAt.IsZero() || status.LastError == "" {
			t.Errorf("%s status missing attempt/error details: %+v", name, status)
		}
		if !status.LastSuccess.IsZero() {
			t.Errorf("%s unexpectedly recorded success: %+v", name, status)
		}
	}
	if got := r.Health()["panicker"].LastError; !strings.Contains(got, "panic") {
		t.Errorf("panic LastError = %q, want panic detail", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestRunnerReadinessRecoversAfterSuccess(t *testing.T) {
	boom := errors.New("temporary")
	releaseRecovery := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRecovery) }) }
	m := &sequenceModule{
		name: "recovering", interval: 10 * time.Millisecond,
		results: []error{nil, boom, boom, boom, nil}, blockBefore: 4, release: releaseRecovery,
	}
	r := newTestRunner(t, 50*time.Millisecond, m)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		release()
		cancel()
	}()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	waitFor(t, time.Second, func() bool { return r.Health()[m.name].ConsecutiveFailures == 3 })
	if r.Ready() {
		t.Fatal("runner remained ready after three consecutive failures")
	}
	release()
	waitFor(t, time.Second, r.Ready)
	status := r.Health()[m.name]
	if status.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 after recovery", status.ConsecutiveFailures)
	}
	if status.LastSuccess.IsZero() {
		t.Error("recovery did not update LastSuccess")
	}
	if status.LastError == "" {
		t.Error("LastError should be retained after recovery for diagnostics")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestRunnerLiveReflectsStaleAttempt(t *testing.T) {
	block := make(chan struct{})
	m := &tickRecorder{name: "slow", interval: 5 * time.Millisecond, block: block}
	r := newTestRunner(t, 5*time.Millisecond, m)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	waitFor(t, time.Second, func() bool { return !r.Health()[m.name].LastAttempt.IsZero() })
	waitFor(t, time.Second, func() bool { return !r.Live() })
	if r.Ready() {
		t.Error("stale module must not be ready")
	}

	close(block)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestRunnerShutdownIsBoundedWhenModuleIgnoresContext(t *testing.T) {
	block := make(chan struct{})
	m := &tickRecorder{name: "blocked", interval: time.Second, block: block}
	r := newTestRunner(t, 5*time.Millisecond, m)
	r.shutdownTimeout = 25 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitFor(t, time.Second, func() bool { return !r.Health()[m.name].LastAttempt.IsZero() })

	started := time.Now()
	cancel()
	if err := <-done; !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run error = %v, want ErrShutdownTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("bounded shutdown took %v", elapsed)
	}
	close(block)
}

func TestNewRunnerRejectsInvalidModules(t *testing.T) {
	valid := func(name string, interval time.Duration) Module {
		return &tickRecorder{name: name, interval: interval}
	}
	var typedNil *tickRecorder
	tests := []struct {
		name        string
		tickTimeout time.Duration
		modules     []Module
	}{
		{name: "nonpositive tick timeout", tickTimeout: 0, modules: []Module{valid("ok", time.Second)}},
		{name: "nil module", tickTimeout: time.Second, modules: []Module{nil}},
		{name: "typed nil module", tickTimeout: time.Second, modules: []Module{typedNil}},
		{name: "blank name", tickTimeout: time.Second, modules: []Module{valid("", time.Second)}},
		{name: "untrimmed name", tickTimeout: time.Second, modules: []Module{valid(" bad ", time.Second)}},
		{name: "duplicate name", tickTimeout: time.Second, modules: []Module{valid("same", time.Second), valid("same", time.Second)}},
		{name: "nonpositive interval", tickTimeout: time.Second, modules: []Module{valid("bad", 0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewRunner(discardLogger(), tt.tickTimeout, tt.modules...); err == nil {
				t.Fatal("NewRunner returned nil error")
			}
		})
	}
}
