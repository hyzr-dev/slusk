package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/observ"
	"github.com/hyzr-dev/slusk/internal/store"
)

func TestJobProgressReportDerivesAgesFromNow(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	snapshot := store.JobProgress{
		States: []store.JobStateProgress{
			{State: core.StateDownloading, Count: 2, OldestUpdate: now.Add(-90 * time.Second)},
			{State: core.StateWanted, Count: 5, OldestUpdate: now.Add(-2 * time.Hour)},
		},
		JobsWithoutActiveCandidate: 3,
	}

	got := jobProgressReport(snapshot, now)

	if len(got.States) != 2 {
		t.Fatalf("states = %+v, want two entries", got.States)
	}
	if got.States[0].State != "DOWNLOADING" || got.States[0].Count != 2 || got.States[0].OldestUpdateAge != 90*time.Second {
		t.Errorf("first entry = %+v", got.States[0])
	}
	if got.States[1].State != "WANTED" || got.States[1].OldestUpdateAge != 2*time.Hour {
		t.Errorf("second entry = %+v", got.States[1])
	}
	if got.JobsWithoutActiveCandidate != 3 {
		t.Errorf("jobsWithoutActiveCandidate = %d, want 3", got.JobsWithoutActiveCandidate)
	}
}

// A clock that has gone backwards relative to a row's updated_at must not
// produce a negative age: a gauge reading -4 would silently pass any
// "older than N" alert.
func TestJobProgressReportClampsNegativeAges(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	snapshot := store.JobProgress{States: []store.JobStateProgress{
		{State: core.StateImporting, Count: 1, OldestUpdate: now.Add(4 * time.Second)},
	}}

	got := jobProgressReport(snapshot, now)
	if got.States[0].OldestUpdateAge != 0 {
		t.Errorf("age = %s, want 0", got.States[0].OldestUpdateAge)
	}
}

// fakeProgressReader serves a canned snapshot and counts reads.
type fakeProgressReader struct {
	mu       sync.Mutex
	calls    int
	err      error
	snapshot store.JobProgress
}

func (f *fakeProgressReader) JobProgress(ctx context.Context) (store.JobProgress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.snapshot, f.err
}

func (f *fakeProgressReader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeProgressSink records every published report.
type fakeProgressSink struct {
	mu      sync.Mutex
	reports []observ.JobProgressReport
}

func (f *fakeProgressSink) SetJobProgress(r observ.JobProgressReport) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, r)
}

func (f *fakeProgressSink) published() []observ.JobProgressReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]observ.JobProgressReport(nil), f.reports...)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The publisher must sample once at start rather than waiting a full interval:
// a gauge that is absent for the first interval after a restart is a blind spot
// exactly when a wedged job is most likely to be looked for.
func TestJobProgressPublisherPublishesBeforeFirstTick(t *testing.T) {
	reader := &fakeProgressReader{snapshot: store.JobProgress{
		States: []store.JobStateProgress{{State: core.StateDownloading, Count: 1, OldestUpdate: time.Now()}},
	}}
	sink := &fakeProgressSink{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runJobProgressPublisher(ctx, reader, sink, time.Hour, discardLogger())
		close(done)
	}()

	waitFor(t, 2*time.Second, func() bool { return len(sink.published()) == 1 })
	cancel()
	<-done

	if got := sink.published(); len(got) != 1 || len(got[0].States) != 1 {
		t.Errorf("published = %+v, want one report with one state", got)
	}
}

// A read failure is logged and the loop continues: a transient DB hiccup must
// not permanently stop the measurement, and it must not publish a stale or
// empty report that would read as "no jobs anywhere".
func TestJobProgressPublisherSurvivesReadErrors(t *testing.T) {
	reader := &fakeProgressReader{err: errors.New("connection refused")}
	sink := &fakeProgressSink{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runJobProgressPublisher(ctx, reader, sink, time.Millisecond, discardLogger())
		close(done)
	}()

	waitFor(t, 2*time.Second, func() bool { return reader.callCount() >= 3 })
	cancel()
	<-done

	if got := sink.published(); len(got) != 0 {
		t.Errorf("published %d reports despite every read failing: %+v", len(got), got)
	}
}

func TestJobProgressPublisherStopsOnContextCancel(t *testing.T) {
	reader := &fakeProgressReader{}
	sink := &fakeProgressSink{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		runJobProgressPublisher(ctx, reader, sink, time.Millisecond, discardLogger())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher did not return on a cancelled context")
	}
}
