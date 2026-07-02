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
	AttachTransferID(ctx context.Context, transferID int64, slskdID string, now time.Time) error
}

// MusicSource is the slice of the Lidarr client the discoverer needs.
type MusicSource interface {
	WantedMissing(ctx context.Context) ([]lidarr.WantedAlbum, error)
	ManualImportCandidates(ctx context.Context, folder string) ([]lidarr.ManualImportItem, error)
	ExecuteManualImport(ctx context.Context, items []lidarr.ManualImportItem) error
}

// PeerSearcher is the slice of the slskd client the discoverer needs for search+enqueue.
type PeerSearcher interface {
	Search(ctx context.Context, query string, timeout time.Duration) ([]slskd.Result, error)
	Enqueue(ctx context.Context, username, filename string, size int64) (string, error)
}

// DiscoveryStore is the slice of the store the discoverer needs.
type DiscoveryStore interface {
	UpsertDiscoveredJob(ctx context.Context, lidarrAlbumID int64, now time.Time) (core.AlbumJob, error)
	UpdateJobMetadata(ctx context.Context, jobID int64, title, artistName string, now time.Time) error
	BackfillJobMetadataIfEmpty(ctx context.Context, jobID int64, title, artistName string) error
	JobsInState(ctx context.Context, state core.AlbumJobState, limit int) ([]core.AlbumJob, error)
	CountJobsInStates(ctx context.Context, states ...core.AlbumJobState) (int, error)
	DueCooldownJobs(ctx context.Context, now time.Time, limit int) ([]core.AlbumJob, error)
	AttemptsForJob(ctx context.Context, jobID int64) ([]core.CandidateAttempt, error)
	TransfersForAttempt(ctx context.Context, attemptID int64) ([]core.Transfer, error)
	CreateAttempt(ctx context.Context, jobID int64, username string, score float64, now time.Time) (int64, error)
	RecordEnqueueIntent(ctx context.Context, attemptID int64, username, filename string, deadline, now time.Time) (int64, error)
	AttachTransferID(ctx context.Context, transferID int64, slskdID string, now time.Time) error
	UpdateTransferProgress(ctx context.Context, id int64, state core.TransferState, done, total int64, now time.Time) error
	AdvanceJobState(ctx context.Context, jobID int64, to core.AlbumJobState, now time.Time) error
	FailAttempt(ctx context.Context, attemptID int64, reason string, backoffUntil, now time.Time) error
	SucceedAttempt(ctx context.Context, attemptID int64, now time.Time) error
	SetJobCooldown(ctx context.Context, jobID int64, nextAttemptAt, now time.Time) error
	IncrementCandidatesTried(ctx context.Context, jobID int64, now time.Time) error
	DueFailedJobs(ctx context.Context, cutoff time.Time, limit int) ([]core.AlbumJob, error)
	ResetJobForRetry(ctx context.Context, jobID int64, now time.Time) error
}

// Ranker ranks slskd results into candidates (satisfied by matcher.Scorer).
type Ranker interface {
	Rank(results []slskd.Result) []matcher.Candidate
}
