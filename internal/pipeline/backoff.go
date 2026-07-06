package pipeline

import (
	"context"
	"log/slog"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// nextBackoff returns base * 2^retries, capped at maxBackoff. retries is the
// post-increment failure count (first failure -> retries 1). The exponent is
// clamped so 1<<retries never overflows an int on any platform, since callers
// may pass arbitrarily large retry counts (see maxRetries=50 test case).
func nextBackoff(retries int, base, maxBackoff time.Duration) time.Duration {
	const maxExponent = 32 // 1<<32 * any realistic base already exceeds maxBackoff
	exp := retries
	if exp > maxExponent {
		exp = maxExponent
	}
	d := base * time.Duration(1<<exp)
	if d > maxBackoff || d < 0 { // d<0 guards against overflow wrap-around
		return maxBackoff
	}
	return d
}

// BackoffStore is the store slice failOrBackoff needs.
type BackoffStore interface {
	SetJobBackoff(ctx context.Context, jobID int64, retries int, notBefore time.Time, now time.Time) error
	MarkJobFailed(ctx context.Context, jobID int64, now time.Time) error
	ResetJobToWanted(ctx context.Context, jobID int64, retries int, notBefore *time.Time, now time.Time) error
	AddJobEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) error
}

// failOrBackoff records one search-cycle failure: retries+1 >= maxRetries ->
// MarkJobFailed; otherwise back off exponentially. resetToWanted selects
// whether the job also returns to WANTED (Selecting exhaustion) or stays put
// (Discovery empty search - already WANTED). The AddJobEvent write is
// best-effort: a failure is logged at warn level and swallowed rather than
// propagated, since the audit trail must never block the pipeline.
func failOrBackoff(ctx context.Context, st BackoffStore, log *slog.Logger, job core.AlbumJob, maxRetries int, base, maxBackoff time.Duration, resetToWanted bool, now time.Time) error {
	retries := job.Retries + 1

	if retries >= maxRetries {
		if err := st.MarkJobFailed(ctx, job.ID, now); err != nil {
			return err
		}
		recordBackoffEvent(ctx, st, log, job.ID, core.EventJobFailed, now)
		return nil
	}

	notBefore := now.Add(nextBackoff(retries, base, maxBackoff))
	if resetToWanted {
		return st.ResetJobToWanted(ctx, job.ID, retries, &notBefore, now)
	}
	return st.SetJobBackoff(ctx, job.ID, retries, notBefore, now)
}

// recordBackoffEvent best-effort appends one row to a job's audit trail (see
// store.AddJobEvent). A write failure must never block the pipeline, so it is
// logged at warn level and swallowed rather than propagated (same pattern as
// engine.Discoverer.recordEvent).
func recordBackoffEvent(ctx context.Context, st BackoffStore, log *slog.Logger, jobID int64, event core.JobEventType, now time.Time) {
	if err := st.AddJobEvent(ctx, jobID, event, "", now); err != nil {
		if log != nil {
			log.Warn("record job event failed", "album_job", jobID, "event", event, "err", err)
		}
	}
}
