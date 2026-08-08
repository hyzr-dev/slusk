package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hyzr-dev/slusk/internal/observ"
	"github.com/hyzr-dev/slusk/internal/store"
)

// jobProgressReader is the subset of *store.Store the publisher needs, so this
// file depends on the method's signature rather than the whole store.
type jobProgressReader interface {
	JobProgress(ctx context.Context) (store.JobProgress, error)
}

// jobProgressSink receives each published snapshot; *observ.Metrics satisfies
// it. Declared next to its consumer rather than in observ, matching
// pipeline.MetricsSink's shape.
type jobProgressSink interface {
	SetJobProgress(observ.JobProgressReport)
}

// jobProgressReport converts a store snapshot into observ's transport type,
// deriving each state's age from now. Ages are clamped at zero: if the clock
// moves backwards relative to a row's updated_at, a negative gauge would
// silently satisfy every "older than N" alert rather than firing one.
func jobProgressReport(p store.JobProgress, now time.Time) observ.JobProgressReport {
	states := make([]observ.JobProgressState, 0, len(p.States))
	for _, s := range p.States {
		age := now.Sub(s.OldestUpdate)
		if age < 0 {
			age = 0
		}
		states = append(states, observ.JobProgressState{
			State:           string(s.State),
			Count:           s.Count,
			OldestUpdateAge: age,
		})
	}
	return observ.JobProgressReport{
		States:                     states,
		JobsWithoutActiveCandidate: p.JobsWithoutActiveCandidate,
	}
}

// runJobProgressPublisher periodically republishes the job-progress gauges
// (issue #442), mirroring runSessionPruner's shape. It samples once before
// arming the ticker so the gauges are populated immediately after a restart
// rather than missing for a whole interval — which would be a blind spot at
// exactly the moment someone is most likely to go looking for a wedged job.
//
// Deliberately its own goroutine rather than a side effect of some module's
// tick: a module that is itself wedged must not be able to stop the
// measurement of its own wedge. For the same reason it is not a pipeline
// module — a failed read here must never make the daemon unready.
//
// A read failure is logged and the loop continues, publishing nothing for that
// cycle. Publishing an empty report on error would be worse than a gap: it
// would clear every series and read as "no jobs anywhere". Nothing is buffered,
// so there is nothing to flush and ctx.Done() simply returns.
func runJobProgressPublisher(ctx context.Context, reader jobProgressReader, sink jobProgressSink, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if snapshot, err := reader.JobProgress(ctx); err != nil {
			if ctx.Err() == nil {
				logger.Error("read job progress failed", "err", err)
			}
		} else {
			sink.SetJobProgress(jobProgressReport(snapshot, time.Now()))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
