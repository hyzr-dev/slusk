// Package app hosts transport-neutral application services that sit between
// the store and the HTTP edge (internal/observ): the pipeline modules own
// automatic state transitions, while Jobs owns the two manual, user-triggered
// ones (cancel and retry) so they no longer live as ad hoc closures in
// cmd/slskdarr/main.go. Errors are returned as plain Go errors (ErrJobNotFound,
// ErrJobNotRetryable, or a wrapped store/peer error); mapping them to HTTP
// status codes is internal/observ's job, not this package's.
package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// ErrJobNotFound is returned by Cancel/Retry when no job with the given id exists.
var ErrJobNotFound = errors.New("job not found")

// ErrJobNotRetryable is returned by Retry when the job exists but is not
// currently FAILED - the caller raced a state change (e.g. WantedSync revived
// it, or a pipeline module already advanced it).
var ErrJobNotRetryable = errors.New("job is not in a retryable state")

// JobStore is the slice of the store Jobs needs.
type JobStore interface {
	JobWithTransfer(ctx context.Context, jobID int64) (core.JobView, bool, error)
	AdvanceJobState(ctx context.Context, jobID int64, to core.AlbumJobState, now time.Time) error
	RetryFailedJob(ctx context.Context, jobID int64, now time.Time) (bool, error)
}

// TransferCanceller is the slice of the slskd client Jobs needs to cancel a
// job's live remote transfer.
type TransferCanceller interface {
	Cancel(ctx context.Context, username, id string) error
}

// Jobs is the transport-neutral service backing the dashboard's manual
// cancel/retry actions.
type Jobs struct {
	Store  JobStore
	Peers  TransferCanceller
	Logger *slog.Logger
}

func (j *Jobs) log() *slog.Logger {
	if j.Logger != nil {
		return j.Logger
	}
	return slog.Default()
}

// Cancel cancels a job locally even if the remote slskd cancel call fails:
// the job's local state must advance to cancelled regardless, since any
// stale slskd-side entry gets cleaned up by the next reconcile pass. Returns
// ErrJobNotFound if no such job exists.
func (j *Jobs) Cancel(ctx context.Context, jobID int64) error {
	view, found, err := j.Store.JobWithTransfer(ctx, jobID)
	if err != nil {
		return err
	}
	if !found {
		return ErrJobNotFound
	}
	if view.Transfer != nil && view.Transfer.SlskdID != "" {
		if err := j.Peers.Cancel(ctx, view.Transfer.Username, view.Transfer.SlskdID); err != nil {
			j.log().Warn("slskd cancel failed, still advancing job state", "job_id", jobID, "err", err)
		}
	}
	return j.Store.AdvanceJobState(ctx, jobID, core.StateCancelled, time.Now())
}

// Retry manually revives one FAILED job by id: ErrJobNotFound if no such job
// exists, ErrJobNotRetryable if it exists but is not currently FAILED (the
// caller raced a state change).
func (j *Jobs) Retry(ctx context.Context, jobID int64) error {
	_, found, err := j.Store.JobWithTransfer(ctx, jobID)
	if err != nil {
		return err
	}
	if !found {
		return ErrJobNotFound
	}
	ok, err := j.Store.RetryFailedJob(ctx, jobID, time.Now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrJobNotRetryable
	}
	return nil
}
