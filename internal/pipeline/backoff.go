package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
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
	ResetJobToWanted(ctx context.Context, jobID int64, from core.AlbumJobState, retries int, notBefore *time.Time, now time.Time) error
	AddJobEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) error
}

// failOrBackoff records one search-cycle failure: retries+1 >= maxRetries ->
// MarkJobFailed; otherwise back off exponentially. resetToWanted selects
// whether the job also returns to WANTED (Selecting exhaustion) or stays put
// (Discovery: peers answered but every candidate was rejected by filtering -
// already WANTED). The AddJobEvent write is best-effort: a failure is logged
// at warn level and swallowed rather than propagated, since the audit trail
// must never block the pipeline.
//
// failed reports that this was the terminal transition (MarkJobFailed), so a
// caller that owns post-mortem work - Selecting quarantining the job's
// leftover files - can run it without re-deriving the retry arithmetic. It is
// false for both backoff branches. Note that MarkJobFailed's own from-guard
// makes it a no-op (returning nil) if WantedSync cancelled the job underneath
// this tick, so failed==true does not strictly prove the row flipped.
//
// reason is the caller's own account of why this cycle failed, in the same
// words it logs - Discovery knows the search produced nothing usable,
// Selecting knows the candidate cache ran out. It is only read on the terminal
// branch, where jobFailedDetail folds it together with the attempt count into
// the job_failed event's detail. Before issue #318 that event was always
// written empty, so the one event marking the pipeline giving up said nothing
// about why and the reason had to be reconstructed from earlier events, often
// several retry cycles back. An empty reason is allowed and yields the attempt
// count alone.
func failOrBackoff(ctx context.Context, st BackoffStore, log *slog.Logger, job core.AlbumJob, maxRetries int, base, maxBackoff time.Duration, resetToWanted bool, reason string, now time.Time) (bool, error) {
	retries := job.Retries + 1

	if retries >= maxRetries {
		if err := st.MarkJobFailed(ctx, job.ID, now); err != nil {
			return false, err
		}
		recordBackoffEvent(ctx, st, log, job.ID, core.EventJobFailed, jobFailedDetail(retries, maxRetries, reason), now)
		return true, nil
	}

	notBefore := now.Add(nextBackoff(retries, base, maxBackoff))
	if resetToWanted {
		// resetToWanted is only ever set by Selecting (candidate cache exhausted),
		// so the job is in SELECTING; ResetJobToWanted's from-guard bounces if
		// WantedSync cancelled it underneath us.
		return false, st.ResetJobToWanted(ctx, job.ID, core.StateSelecting, retries, &notBefore, now)
	}
	return false, st.SetJobBackoff(ctx, job.ID, retries, notBefore, now)
}

// jobFailedDetail renders the job_failed event's detail: the attempt count,
// which is what distinguishes giving up from any earlier cycle, plus the
// caller's reason when it has one. It reads as a sentence on its own, since
// the dashboard's FAILED JOBS panel (issue #310) shows it without the
// surrounding history.
//
// maxRetries <= 0 marks a caller that fails on the first hit by policy rather
// than by exhaustion - Discovery's excluded-phrase path (#319), which is
// doomed by construction and never worth retrying. Its attempts value is
// whatever the job accumulated for unrelated reasons, so printing it would
// claim a retry budget that was never in play. The count is dropped there, but
// only when there is a reason to print instead: an empty detail is the very
// thing #318 is about.
func jobFailedDetail(attempts, maxRetries int, reason string) string {
	if maxRetries <= 0 && reason != "" {
		return reason
	}
	noun := "attempts"
	if attempts == 1 {
		noun = "attempt"
	}
	detail := fmt.Sprintf("gave up after %d %s", attempts, noun)
	if reason == "" {
		return detail
	}
	return detail + ": " + reason
}

// recordBackoffEvent best-effort appends one row to a job's audit trail (see
// store.AddJobEvent). A write failure must never block the pipeline, so it is
// logged at warn level and swallowed rather than propagated (same pattern as
// engine.Discoverer.recordEvent).
func recordBackoffEvent(ctx context.Context, st BackoffStore, log *slog.Logger, jobID int64, event core.JobEventType, detail string, now time.Time) {
	if err := st.AddJobEvent(ctx, jobID, event, detail, now); err != nil {
		if log != nil {
			log.Warn("record job event failed", "album_job", jobID, "event", event, "err", err)
		}
	}
}
