// Package engine owns the scheduler, workers, and reconciler. It defines the
// narrow port interfaces it consumes here (Go style: the consumer declares the
// interface), so the concrete slskd/store types satisfy them implicitly.
package engine

import (
	"context"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
	"github.com/samuelenocsson/slskdarr/internal/matcher"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
)

// PeerNetwork is the slice of the slskd client the engine needs.
type PeerNetwork interface {
	ListDownloads(ctx context.Context) ([]slskd.Transfer, error)
	Cancel(ctx context.Context, username, id string) error
}

// JobStore is the slice of the store the reconciler needs.
type JobStore interface {
	ActiveTransfers(ctx context.Context) ([]core.Transfer, error)
	TransfersPastDeadline(ctx context.Context, now time.Time) ([]core.Transfer, error)
	UpdateTransferProgress(ctx context.Context, id int64, state core.TransferState, done, total int64, now time.Time) error
	RetryTransfer(ctx context.Context, transferID int64, now time.Time) error
	AttachTransferID(ctx context.Context, transferID int64, slskdID string, now time.Time) error
}

// MusicSource is the slice of the Lidarr client the discoverer needs.
type MusicSource interface {
	WantedMissing(ctx context.Context) ([]lidarr.WantedAlbum, error)
	ManualImportCandidates(ctx context.Context, folder string) ([]lidarr.ManualImportItem, error)
	ExecuteManualImport(ctx context.Context, items []lidarr.ManualImportItem) error
	AlbumStatus(ctx context.Context, albumID int64) (present, total int, err error)
}

// PeerSearcher is the slice of the slskd client the discoverer needs for search+enqueue.
type PeerSearcher interface {
	Search(ctx context.Context, query string, timeout time.Duration) ([]slskd.Result, error)
	Enqueue(ctx context.Context, username, filename string, size int64) (string, error)
	// Cancel cancels and removes a still-active slskd download, so a failed
	// attempt's live sibling transfers stop writing into a folder that is about
	// to be cleaned up. Same call the reconciler uses for deadline-overdue
	// transfers.
	Cancel(ctx context.Context, username, id string) error
	DeleteDownloadFolder(ctx context.Context, name string) error
}

// DiscoveryStore is the slice of the store the discoverer needs.
type DiscoveryStore interface {
	UpsertDiscoveredJob(ctx context.Context, lidarrAlbumID int64, now time.Time) (core.AlbumJob, error)
	UpdateJobMetadata(ctx context.Context, jobID int64, title, artistName, releaseDate string, artistID int64, now time.Time) error
	BackfillJobMetadataIfEmpty(ctx context.Context, jobID int64, title, artistName, releaseDate string, artistID int64) error
	JobsInState(ctx context.Context, state core.AlbumJobState, limit int) ([]core.AlbumJob, error)
	CountJobsInStates(ctx context.Context, states ...core.AlbumJobState) (int, error)
	DueCooldownJobs(ctx context.Context, now time.Time, limit int) ([]core.AlbumJob, error)
	AttemptsForJob(ctx context.Context, jobID int64) ([]core.CandidateAttempt, error)
	TransfersForAttempt(ctx context.Context, attemptID int64) ([]core.Transfer, error)
	CreateAttempt(ctx context.Context, jobID int64, username string, score float64, now time.Time) (int64, error)
	RecordPendingTransfer(ctx context.Context, attemptID int64, username, filename string, size int64, now time.Time) error
	RecordEnqueueIntent(ctx context.Context, attemptID int64, username, filename string, deadline, now time.Time) (int64, error)
	AttachTransferID(ctx context.Context, transferID int64, slskdID string, now time.Time) error
	UpdateTransferProgress(ctx context.Context, id int64, state core.TransferState, done, total int64, now time.Time) error
	RetryTransfer(ctx context.Context, transferID int64, now time.Time) error
	AdvanceJobState(ctx context.Context, jobID int64, to core.AlbumJobState, now time.Time) error
	FailAttempt(ctx context.Context, attemptID int64, reason string, backoffUntil, now time.Time) error
	SucceedAttempt(ctx context.Context, attemptID int64, now time.Time) error
	SetJobCooldown(ctx context.Context, jobID int64, nextAttemptAt, now time.Time) error
	IncrementCandidatesTried(ctx context.Context, jobID int64, now time.Time) error
	DueFailedJobs(ctx context.Context, cutoff time.Time, limit int) ([]core.AlbumJob, error)
	ResetJobForRetry(ctx context.Context, jobID int64, now time.Time) error
	// ReliabilityFor batch-looks-up known peer reliability history for a set of
	// usernames against one artist (see matcher.ReliabilityHistoryScore), for
	// use in Ranker.Rank.
	ReliabilityFor(ctx context.Context, artistID int64, usernames []string) (map[string]core.PeerReliability, error)
	// RecordAttemptOutcome writes a candidate attempt's terminal success/fail
	// outcome to the peer reliability history. Must be called at every attempt
	// completion (not derived from candidate_attempts), since ResetJobForRetry
	// deletes that table's rows on every retry cycle.
	RecordAttemptOutcome(ctx context.Context, artistID int64, username string, success bool, now time.Time) error
	// AddJobEvent appends one row to a job's audit trail (see store.AddJobEvent).
	// Callers must treat write failures as best-effort: log and continue rather
	// than propagate, since the audit trail must never block the pipeline.
	AddJobEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) error
}

// Ranker ranks slskd results into candidates (satisfied by matcher.Scorer).
type Ranker interface {
	Rank(results []slskd.Result, rel map[string]core.PeerReliability, now time.Time) []matcher.Candidate
}

// EventPruner is the slice of the store the engine needs to prune old
// job_events rows (see Store.PruneJobEvents). A nil EventPruner disables
// pruning.
type EventPruner interface {
	PruneJobEvents(ctx context.Context, now time.Time) error
}
