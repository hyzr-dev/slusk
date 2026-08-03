package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/soulseek"
)

type fakeShareRescanner struct {
	calls  atomic.Int32
	active atomic.Int32
	max    atomic.Int32
}

func (f *fakeShareRescanner) RescanShares(context.Context) (soulseek.ShareStats, error) {
	active := f.active.Add(1)
	for {
		old := f.max.Load()
		if active <= old || f.max.CompareAndSwap(old, active) {
			break
		}
	}
	f.calls.Add(1)
	time.Sleep(5 * time.Millisecond)
	f.active.Add(-1)
	return soulseek.ShareStats{Files: 1}, nil
}

func TestShareRescanLoopSerializesSignalsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 3)
	fake := &fakeShareRescanner{}
	done := make(chan struct{})
	go func() {
		runShareRescanLoop(ctx, signals, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	signals <- syscall.SIGHUP
	signals <- syscall.SIGHUP
	deadline := time.Now().Add(time.Second)
	for fake.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rescan loop did not stop")
	}
	if fake.calls.Load() != 2 || fake.max.Load() != 1 {
		t.Fatalf("calls/max concurrency = %d/%d", fake.calls.Load(), fake.max.Load())
	}
}

// fakeThroughputSource is a throughputSource whose TakeThroughputMinutes
// return value is configurable, tracking every call's includePartial flag.
type fakeThroughputSource struct {
	mu              sync.Mutex
	regularMinutes  []core.ThroughputMinute // returned once, on the first non-partial call
	partialMinutes  []core.ThroughputMinute // returned once, on the first includePartial call
	regularConsumed bool
	partialConsumed bool
	calls           []bool // includePartial value of every call, in order
}

func (f *fakeThroughputSource) TakeThroughputMinutes(includePartial bool) []core.ThroughputMinute {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, includePartial)
	if includePartial {
		if f.partialConsumed {
			return nil
		}
		f.partialConsumed = true
		return f.partialMinutes
	}
	if f.regularConsumed {
		return nil
	}
	f.regularConsumed = true
	return f.regularMinutes
}

func (f *fakeThroughputSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeThroughputSink is a throughputSink recording every RecordThroughputMinute
// call, including whether ctx was already Done when it was invoked — the
// mechanism TestRunThroughputRecorderFlushesPartialMinuteOnCancelWithFreshContext
// uses to prove the shutdown flush uses a fresh, non-cancelled context rather
// than the recorder's own (by-then-cancelled) ctx.
type fakeThroughputSink struct {
	mu            sync.Mutex
	recorded      []core.ThroughputMinute
	ctxDoneAtCall []bool
	failNext      int // number of remaining calls to fail with an error
	// delay, if nonzero, is slept before recording — used to simulate a slow
	// store write so tests can assert callers actually wait for it rather
	// than racing ahead (issue #157 F1).
	delay time.Duration
}

func (f *fakeThroughputSink) RecordThroughputMinute(ctx context.Context, m core.ThroughputMinute) error {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxDoneAtCall = append(f.ctxDoneAtCall, ctx.Err() != nil)
	if f.failNext > 0 {
		f.failNext--
		return errors.New("sink write failed")
	}
	f.recorded = append(f.recorded, m)
	return nil
}

func (f *fakeThroughputSink) snapshot() []core.ThroughputMinute {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.ThroughputMinute(nil), f.recorded...)
}

func waitFor(t *testing.T, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not met within deadline")
	}
}

// TestRunThroughputRecorderWritesDrainedMinutes asserts a regular tick drains
// src's pending minutes and writes each one to sink.
func TestRunThroughputRecorderWritesDrainedMinutes(t *testing.T) {
	minute := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	src := &fakeThroughputSource{
		regularMinutes: []core.ThroughputMinute{{Minute: minute, AvgBytesPerSecond: 100, Samples: 30}},
	}
	sink := &fakeThroughputSink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runThroughputRecorder(ctx, src, sink, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	waitFor(t, time.Second, func() bool { return len(sink.snapshot()) >= 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runThroughputRecorder did not stop after cancel")
	}

	got := sink.snapshot()
	if len(got) < 1 || !got[0].Minute.Equal(minute) {
		t.Fatalf("recorded minutes = %+v, want at least one starting with %v", got, minute)
	}
}

// TestRunThroughputRecorderFlushesPartialMinuteOnCancelWithFreshContext
// asserts the shutdown path performs exactly one final includePartial=true
// drain, and that the sink observes a NOT-yet-done context for that call —
// proving runThroughputRecorder built a fresh context for it rather than
// reusing its own, by-then-cancelled ctx (which would make every ctx.Err()
// check non-nil).
func TestRunThroughputRecorderFlushesPartialMinuteOnCancelWithFreshContext(t *testing.T) {
	minute := time.Date(2026, 7, 25, 12, 5, 0, 0, time.UTC)
	src := &fakeThroughputSource{
		partialMinutes: []core.ThroughputMinute{{Minute: minute, AvgBytesPerSecond: 50, Samples: 12}},
	}
	sink := &fakeThroughputSink{}
	ctx, cancel := context.WithCancel(context.Background())
	// A long interval so no regular tick fires before we cancel — only the
	// shutdown drain should ever call TakeThroughputMinutes.
	done := make(chan struct{})
	go func() {
		runThroughputRecorder(ctx, src, sink, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runThroughputRecorder did not stop after cancel")
	}

	got := sink.snapshot()
	if len(got) != 1 || !got[0].Minute.Equal(minute) {
		t.Fatalf("recorded minutes on shutdown = %+v, want exactly the partial minute %v", got, minute)
	}
	sink.mu.Lock()
	ctxDone := append([]bool(nil), sink.ctxDoneAtCall...)
	sink.mu.Unlock()
	if len(ctxDone) != 1 || ctxDone[0] {
		t.Fatalf("sink observed ctx.Err() != nil on the shutdown flush call = %v, want a fresh (not-done) context", ctxDone)
	}
}

// TestShutdownSoulseekJoinsThroughputRecorderFlushBeforeReturning asserts
// shutdownSoulseek does not return until the throughput recorder's shutdown
// flush has actually written its partial minute to the sink. Before the #157
// F1 fix nothing waited on the recorder, so a slow shutdown flush let the
// caller proceed while the write was still in flight, racing st.Close(); this
// test fails if shutdownSoulseek returns before that write lands.
//
// Scope: this exercises shutdownSoulseek directly. That main() calls it
// before closeStoreAfterRuntime — the other half of the guarantee — is not
// covered here, since main() is not testable; it is enforced by the ordering
// comment at that call site.
func TestShutdownSoulseekJoinsThroughputRecorderFlushBeforeReturning(t *testing.T) {
	minute := time.Date(2026, 7, 25, 12, 5, 0, 0, time.UTC)
	src := &fakeThroughputSource{
		partialMinutes: []core.ThroughputMinute{{Minute: minute, AvgBytesPerSecond: 50, Samples: 12}},
	}
	// A deliberately slow sink write simulates a real store flush taking
	// noticeable time; if shutdownSoulseek raced ahead instead of joining
	// throughputDone, the assertion below would observe zero recorded
	// minutes.
	sink := &fakeThroughputSink{delay: 50 * time.Millisecond}

	soulCtx, soulCancel := context.WithCancel(context.Background())
	throughputDone := make(chan struct{})
	go func() {
		// A long interval so only the shutdown drain ever fires.
		runThroughputRecorder(soulCtx, src, sink, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(throughputDone)
	}()

	shutdownSoulseek(slog.New(slog.NewTextHandler(io.Discard, nil)), soulCancel, nil, throughputDone, time.Second)

	got := sink.snapshot()
	if len(got) != 1 || !got[0].Minute.Equal(minute) {
		t.Fatalf("shutdownSoulseek returned before the throughput recorder's shutdown flush completed: recorded = %+v, want exactly the partial minute %v", got, minute)
	}
}

// TestRunThroughputRecorderContinuesAfterSinkError asserts one failing sink
// write does not kill the recorder loop: a later tick still drains and
// writes successfully.
func TestRunThroughputRecorderContinuesAfterSinkError(t *testing.T) {
	minuteA := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	src := &fakeThroughputSource{
		regularMinutes: []core.ThroughputMinute{{Minute: minuteA, Samples: 30}},
	}
	sink := &fakeThroughputSink{failNext: 1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runThroughputRecorder(ctx, src, sink, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	// The first drain's write fails; the loop must keep running and calling
	// TakeThroughputMinutes on subsequent ticks rather than exiting.
	waitFor(t, time.Second, func() bool { return src.callCount() >= 3 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runThroughputRecorder did not stop after cancel")
	}
}
