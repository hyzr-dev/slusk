package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// tickRecorder is a fake Module used across these tests: it counts
// completed ticks and can be told to error, panic, or hang (ignoring ctx)
// on every tick so the runner's fault tolerance can be exercised directly.
type tickRecorder struct {
	name     string
	interval time.Duration
	ticks    atomic.Int64
	err      error
	panics   bool
	block    chan struct{} // non-nil: Tick blocks on this until it is closed, ignoring ctx entirely (hang simulation)
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

// discardLogger returns a slog.Logger that never writes to stderr, so test
// output stays clean even when panics/errors are deliberately triggered.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func TestRunnerTicksEveryModuleIndependently(t *testing.T) {
	m1 := &tickRecorder{name: "m1", interval: 10 * time.Millisecond}
	m2 := &tickRecorder{name: "m2", interval: 10 * time.Millisecond}
	r := NewRunner(discardLogger(), 50*time.Millisecond, m1, m2)

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

func TestRunnerSurvivesPanicAndError(t *testing.T) {
	panicker := &tickRecorder{name: "panicker", interval: 10 * time.Millisecond, panics: true}
	erroring := &tickRecorder{name: "erroring", interval: 10 * time.Millisecond, err: errors.New("boom")}
	healthy := &tickRecorder{name: "healthy", interval: 10 * time.Millisecond}
	r := NewRunner(discardLogger(), 50*time.Millisecond, panicker, erroring, healthy)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// The panicking module's ticks counter never increments (it panics
	// before reaching ticks.Add), but its lastTick must still advance -
	// checked separately below via Health/Healthy in other tests. Here we
	// only assert the process survived and the other modules kept going.
	if got := erroring.ticks.Load(); got < 5 {
		t.Errorf("erroring module ticks = %d, want >= 5", got)
	}
	if got := healthy.ticks.Load(); got < 5 {
		t.Errorf("healthy module ticks = %d, want >= 5", got)
	}

	if lastTick := r.Health()["panicker"]; lastTick.IsZero() {
		t.Error("panicker module never recorded a completed tick despite panicking")
	}
}

func TestRunnerHealthyReflectsStaleModule(t *testing.T) {
	fast := &tickRecorder{name: "fast", interval: 10 * time.Millisecond}
	block := make(chan struct{})
	slow := &tickRecorder{name: "slow", interval: 10 * time.Millisecond, block: block}

	r := NewRunner(discardLogger(), 10*time.Millisecond, fast, slow)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()

	// slow's first tick is blocked on `block`, ignoring ctx entirely, well
	// past the 10ms tickTimeout; fast keeps ticking normally in the
	// meantime, so overall health must still report unhealthy.
	time.Sleep(60 * time.Millisecond)

	if fast.ticks.Load() == 0 {
		t.Fatal("expected fast module to have ticked while slow module was hung")
	}
	if r.Healthy() {
		t.Fatal("expected Healthy() == false while slow module has never completed a tick")
	}

	close(block) // release slow's hung tick
	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of ctx cancel + unblocking the hung tick")
	}
}

func TestRunnerStopsOnContextCancel(t *testing.T) {
	m := &tickRecorder{name: "m", interval: 10 * time.Millisecond}
	r := NewRunner(discardLogger(), 50*time.Millisecond, m)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of ctx cancel")
	}
}
