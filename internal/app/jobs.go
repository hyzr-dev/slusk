// Package app hosts transport-neutral application services that sit between
// the store and the HTTP edge (internal/observ): the pipeline modules own
// automatic state transitions, while Jobs owns the manual, user-triggered
// ones (cancel, retry, and create) so they no longer live as ad hoc closures
// in cmd/slskdarr/main.go. Errors are returned as plain Go errors
// (ErrJobNotFound, ErrJobNotRetryable, ErrRemoteFileBusy, or a wrapped
// store/peer error); mapping them to HTTP status codes is internal/observ's
// job, not this package's.
package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// ErrJobNotFound is returned when a requested job does not exist.
var ErrJobNotFound = errors.New("job not found")

// ErrJobNotRetryable is returned by Retry when the job exists but is not
// currently FAILED, PARKED, or legacy ORPHANED - the caller raced a state
// change (e.g. WantedSync revived it, or a pipeline module already advanced it).
var ErrJobNotRetryable = errors.New("job is not in a retryable state")

// ErrRemoteFileBusy is returned by Create when another live candidate already
// owns one of the requested (peer, filename) pairs. Mirrors
// store.ErrRemoteFileBusy so observ never needs to import internal/store.
var ErrRemoteFileBusy = errors.New("remote file already claimed by another live candidate")

// ErrJobActive is returned by ForceSearch when the job exists but is
// currently DOWNLOADING or IMPORTING - re-queuing it for search would race
// (and discard) an in-flight transfer or import.
var ErrJobActive = errors.New("job is actively transferring")

// ErrJobImporting is returned by Delete when the job exists but is currently
// IMPORTING. Mirrors store.ErrJobImporting so observ never needs to import
// internal/store.
var ErrJobImporting = errors.New("job is importing")

// JobStore is the slice of the store Jobs needs.
type JobStore interface {
	JobWithTransfer(ctx context.Context, jobID int64) (core.JobView, bool, error)
	CancelJob(ctx context.Context, jobID int64, now time.Time) ([]core.Transfer, bool, error)
	PrepareDeleteJob(ctx context.Context, jobID int64, now time.Time) ([]core.Transfer, bool, error)
	RetryFailedJob(ctx context.Context, jobID int64, now time.Time) (bool, error)
	CreateManualJob(ctx context.Context, title, artistName, peer string, files []store.ManualJobFile, now time.Time) (core.AlbumJob, error)
	ForceSearchJob(ctx context.Context, jobID int64, now time.Time) (bool, error)
	DeleteJob(ctx context.Context, jobID int64) (bool, error)
}

// TransferCanceller is the slice of the slskd client Jobs needs to cancel a
// job's live remote transfers.
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

// cancelRemoteTransfers attempts every known remote cancellation. Individual
// failures cannot prevent later attempts or the durable local lifecycle action.
func (j *Jobs) cancelRemoteTransfers(ctx context.Context, jobID int64, transfers []core.Transfer) {
	for _, transfer := range transfers {
		if transfer.SlskdID == "" {
			continue
		}
		if err := j.Peers.Cancel(ctx, transfer.Username, transfer.SlskdID); err != nil {
			j.log().Warn("remote transfer cancel failed",
				"job_id", jobID,
				"transfer_id", transfer.ID,
				"username", transfer.Username,
				"slskd_id", transfer.SlskdID,
				"err", err)
		}
	}
}

// Cancel first commits the local cancellation barrier, atomically capturing
// and cancelling every live child transfer with the job. It then best-effort
// cancels the captured remote identities. Remote failures are logged but do
// not undo the durable local transition. Returns ErrJobNotFound if no such job
// exists.
func (j *Jobs) Cancel(ctx context.Context, jobID int64) error {
	transfers, found, err := j.Store.CancelJob(ctx, jobID, time.Now())
	if err != nil {
		return err
	}
	if !found {
		return ErrJobNotFound
	}
	j.cancelRemoteTransfers(ctx, jobID, transfers)
	return nil
}

// Retry manually revives one FAILED, PARKED, or legacy ORPHANED job by id:
// ErrJobNotFound if no such job exists, ErrJobNotRetryable if it exists but is
// not currently retryable (the caller raced a state change).
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

// Create manually creates a job that downloads a known peer's files directly,
// skipping Discovery/Selecting (issue #155). It deliberately bypasses the
// MaxActive cap: the user explicitly requested this download, so it is
// created unconditionally. Returns ErrRemoteFileBusy if another live
// candidate already owns one of the requested (peer, filename) pairs.
//
// Importing a manual job is out of scope: a completed download auto-advances
// to IMPORTING where Music.AlbumStatus misbehaves for a NULL/0 lidarr_album_id
// (see #59/#60). Downloading still works end-to-end; only the subsequent
// Lidarr import step is affected.
func (j *Jobs) Create(ctx context.Context, title, artistName, peer string, files []store.ManualJobFile) (core.JobView, error) {
	job, err := j.Store.CreateManualJob(ctx, title, artistName, peer, files, time.Now())
	if errors.Is(err, store.ErrRemoteFileBusy) {
		return core.JobView{}, ErrRemoteFileBusy
	}
	if err != nil {
		return core.JobView{}, err
	}
	view, found, err := j.Store.JobWithTransfer(ctx, job.ID)
	if err != nil {
		return core.JobView{}, err
	}
	if !found {
		return core.JobView{}, ErrJobNotFound
	}
	return view, nil
}

// ForceSearch manually re-queues one job (issue #159) for an immediate
// re-search, bypassing any current backoff: ErrJobNotFound if no such job
// exists, ErrJobActive if it is currently DOWNLOADING or IMPORTING (either
// found on the initial lookup, or discovered by the store's guard when the
// caller raced a state change).
func (j *Jobs) ForceSearch(ctx context.Context, jobID int64) error {
	view, found, err := j.Store.JobWithTransfer(ctx, jobID)
	if err != nil {
		return err
	}
	if !found {
		return ErrJobNotFound
	}
	if view.Job.State == core.StateDownloading || view.Job.State == core.StateImporting {
		return ErrJobActive
	}
	ok, err := j.Store.ForceSearchJob(ctx, jobID, time.Now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrJobActive
	}
	return nil
}

// Delete permanently removes one job and its children (issue #159):
// ErrJobNotFound if no such job exists, ErrJobImporting if it is currently
// IMPORTING. It first commits the local cancellation barrier, then best-effort
// cancels every captured remote identity before the hard delete. If the hard
// delete fails, the job safely remains CANCELLED for a later retry.
func (j *Jobs) Delete(ctx context.Context, jobID int64) error {
	transfers, found, err := j.Store.PrepareDeleteJob(ctx, jobID, time.Now())
	if errors.Is(err, store.ErrJobImporting) {
		return ErrJobImporting
	}
	if err != nil {
		return err
	}
	if !found {
		return ErrJobNotFound
	}
	j.cancelRemoteTransfers(ctx, jobID, transfers)

	ok, err := j.Store.DeleteJob(ctx, jobID)
	if errors.Is(err, store.ErrJobImporting) {
		return ErrJobImporting
	}
	if err != nil {
		return err
	}
	if !ok {
		return ErrJobNotFound
	}
	return nil
}
