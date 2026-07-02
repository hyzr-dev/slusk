package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
)

// errAlbumNoLongerWanted signals that a job's album has left Lidarr's wanted
// list, so the job should be cancelled rather than retried indefinitely.
var errAlbumNoLongerWanted = errors.New("album no longer wanted")

// DiscovererParams configures a Discoverer.
type DiscovererParams struct {
	Music            MusicSource
	Peers            PeerSearcher
	Store            DiscoveryStore
	Ranker           Ranker
	CompleteDir      string
	SearchTimeout    time.Duration
	TransferDeadline time.Duration
	CandidateBackoff       time.Duration
	FailedCandidateBackoff time.Duration
	FailedRetryAfter time.Duration
	MaxCandidates    int
	Batch            int
	MaxActive        int
	Logger           *slog.Logger
}

// Discoverer drives AlbumJobs through the pipeline, one transition per tick.
type Discoverer struct {
	p DiscovererParams
}

// NewDiscoverer constructs a Discoverer.
func NewDiscoverer(p DiscovererParams) *Discoverer { return &Discoverer{p: p} }

func (d *Discoverer) log() *slog.Logger {
	if d.p.Logger != nil {
		return d.p.Logger
	}
	return slog.Default()
}

// RunOnce performs one pipeline tick: sync wanted albums from Lidarr, then take
// each job one transition forward. Every stage is bounded and idempotent, so a
// crash mid-tick loses nothing.
func (d *Discoverer) RunOnce(ctx context.Context, now time.Time) error {
	albums, err := d.p.Music.WantedMissing(ctx)
	if err != nil {
		return fmt.Errorf("wanted missing: %w", err)
	}
	wanted := make(map[int64]lidarr.WantedAlbum, len(albums))
	for _, a := range albums {
		wanted[a.ID] = a
	}
	if err := d.syncWanted(ctx, albums, now); err != nil {
		return err
	}
	if err := d.retryFailedJobs(ctx, now); err != nil {
		return err
	}
	if err := d.startNewJobs(ctx, wanted, now); err != nil {
		return err
	}
	if err := d.advanceDownloading(ctx, now); err != nil {
		return err
	}
	return d.advanceImporting(ctx, now)
}

// syncWanted upserts every wanted Lidarr album as a DISCOVERED job (idempotent),
// and refreshes the job's cached title/artist metadata every pass while it sits
// in DISCOVERED, so a rename in Lidarr before the job is picked up still shows
// up. Jobs already past DISCOVERED are left alone: UpdateJobMetadata also bumps
// updated_at, and stores like DueFailedJobs key off updated_at to time state
// transitions (e.g. the failed-retry cooldown), so touching it on every pass
// for a still-wanted FAILED/DOWNLOADING/etc. job would keep resetting that
// clock and starve retries.
func (d *Discoverer) syncWanted(ctx context.Context, albums []lidarr.WantedAlbum, now time.Time) error {
	for _, a := range albums {
		job, err := d.p.Store.UpsertDiscoveredJob(ctx, a.ID, now)
		if err != nil {
			return err
		}
		if job.State != core.StateDiscovered {
			continue
		}
		if err := d.p.Store.UpdateJobMetadata(ctx, job.ID, a.Title, a.ArtistName, now); err != nil {
			return err
		}
	}
	return nil
}

// retryFailedJobs returns FAILED albums to DISCOVERED once failed_retry_after has
// elapsed since they failed, so a still-wanted album gets another chance later.
func (d *Discoverer) retryFailedJobs(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-d.p.FailedRetryAfter)
	jobs, err := d.p.Store.DueFailedJobs(ctx, cutoff, d.p.Batch)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err := d.p.Store.ResetJobForRetry(ctx, job.ID, now); err != nil {
			d.log().Error("reset failed job for retry", "album_job", job.ID, "err", err)
			continue
		}
		d.log().Info("retrying failed album after cooldown", "album_job", job.ID)
	}
	return nil
}

// startNewJobs searches and enqueues for DISCOVERED jobs and due COOLDOWN jobs,
// capped so the number of newly-started jobs never pushes the total count of
// active (searching/downloading/importing) jobs past MaxActive.
func (d *Discoverer) startNewJobs(ctx context.Context, wanted map[int64]lidarr.WantedAlbum, now time.Time) error {
	active, err := d.p.Store.CountJobsInStates(ctx,
		core.StateSearching, core.StateSelecting, core.StateDownloading, core.StateVerifying, core.StateImporting)
	if err != nil {
		return err
	}
	available := d.p.MaxActive - active
	if available <= 0 {
		return nil
	}
	batch := d.p.Batch
	if available < batch {
		batch = available
	}
	jobs, err := d.p.Store.JobsInState(ctx, core.StateDiscovered, batch)
	if err != nil {
		return err
	}
	if remaining := available - len(jobs); remaining > 0 {
		due, err := d.p.Store.DueCooldownJobs(ctx, now, remaining)
		if err != nil {
			return err
		}
		jobs = append(jobs, due...)
	}
	for _, job := range jobs {
		if err := d.startJob(ctx, job, wanted, now); err != nil {
			d.log().Error("start job failed", "album_job", job.ID, "err", err)
		}
	}
	return nil
}

// startJob searches for one album, picks the best untried candidate that passes
// the quality floor, write-ahead enqueues its files, and moves the job to
// DOWNLOADING. If no candidate remains it goes to COOLDOWN, or FAILED once the
// candidate budget is exhausted.
func (d *Discoverer) startJob(ctx context.Context, job core.AlbumJob, wanted map[int64]lidarr.WantedAlbum, now time.Time) error {
	if job.CandidatesTried >= d.p.MaxCandidates {
		d.log().Info("candidates exhausted, marking album failed",
			"album_job", job.ID, "lidarr_album", job.LidarrAlbumID, "candidates_tried", job.CandidatesTried)
		return d.p.Store.AdvanceJobState(ctx, job.ID, core.StateFailed, now)
	}
	// Which usernames have we already tried for this album?
	attempts, err := d.p.Store.AttemptsForJob(ctx, job.ID)
	if err != nil {
		return err
	}
	tried := map[string]bool{}
	for _, a := range attempts {
		tried[a.Username] = true
	}

	album, err := d.albumFor(job, wanted)
	if err != nil {
		// The album left Lidarr's wanted list (already sourced, unmonitored, etc.):
		// cancel the job so it stops being retried every tick.
		if errors.Is(err, errAlbumNoLongerWanted) {
			d.log().Info("album no longer wanted, cancelling job",
				"album_job", job.ID, "lidarr_album", job.LidarrAlbumID)
			return d.p.Store.AdvanceJobState(ctx, job.ID, core.StateCancelled, now)
		}
		return err
	}
	results, err := d.p.Peers.Search(ctx, album.ArtistName+" "+album.Title, d.p.SearchTimeout)
	if err != nil {
		return err
	}
	candidates := d.p.Ranker.Rank(results)
	d.log().Info("searched album",
		"album_job", job.ID, "query", album.ArtistName+" "+album.Title,
		"results", len(results), "candidates", len(candidates))
	for _, cand := range candidates {
		if tried[cand.Username] {
			continue
		}
		// Prefer candidates offering at least the expected track count, but accept
		// any if none match (Lidarr is the final arbiter).
		attemptID, err := d.p.Store.CreateAttempt(ctx, job.ID, cand.Username, cand.Score, now)
		if err != nil {
			return err
		}
		deadline := now.Add(d.p.TransferDeadline)
		for _, f := range cand.Files {
			tid, err := d.p.Store.RecordEnqueueIntent(ctx, attemptID, cand.Username, f.Filename, deadline, now)
			if err != nil {
				return err
			}
			slskdID, err := d.p.Peers.Enqueue(ctx, cand.Username, f.Filename, f.Size)
			if err != nil {
				d.log().Error("enqueue failed", "user", cand.Username, "file", f.Filename, "err", err)
				if uerr := d.p.Store.UpdateTransferProgress(ctx, tid, core.TransferErrored, 0, 0, now); uerr != nil {
					d.log().Error("mark transfer errored failed", "transfer", tid, "err", uerr)
				}
				continue
			}
			_ = d.p.Store.AttachTransferID(ctx, tid, slskdID, now)
		}
		if err := d.p.Store.IncrementCandidatesTried(ctx, job.ID, now); err != nil {
			return err
		}
		d.log().Info("enqueued candidate, downloading",
			"album_job", job.ID, "lidarr_album", job.LidarrAlbumID,
			"user", cand.Username, "files", len(cand.Files))
		return d.p.Store.AdvanceJobState(ctx, job.ID, core.StateDownloading, now)
	}
	// No untried candidate available now: count this exhausted tick toward the
	// candidate budget and back off. Once the budget is spent, the next tick's
	// budget check (top of startJob) marks the album FAILED.
	d.log().Info("no untried candidate available, cooling down",
		"album_job", job.ID, "candidates_tried", job.CandidatesTried+1)
	if err := d.p.Store.IncrementCandidatesTried(ctx, job.ID, now); err != nil {
		return err
	}
	return d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.CandidateBackoff), now)
}

// albumFor returns the Lidarr album matching a job (by lidarr_album_id) from
// the wanted map fetched once per RunOnce tick.
func (d *Discoverer) albumFor(job core.AlbumJob, wanted map[int64]lidarr.WantedAlbum) (lidarr.WantedAlbum, error) {
	if a, ok := wanted[job.LidarrAlbumID]; ok {
		return a, nil
	}
	return lidarr.WantedAlbum{}, fmt.Errorf("album %d: %w", job.LidarrAlbumID, errAlbumNoLongerWanted)
}

// advanceDownloading moves DOWNLOADING jobs to VERIFYING when all their active
// attempt's transfers are COMPLETED, or to COOLDOWN when any transfer failed.
func (d *Discoverer) advanceDownloading(ctx context.Context, now time.Time) error {
	jobs, err := d.p.Store.JobsInState(ctx, core.StateDownloading, d.p.Batch)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		attempts, err := d.p.Store.AttemptsForJob(ctx, job.ID)
		if err != nil {
			return err
		}
		if len(attempts) == 0 {
			continue
		}
		active := attempts[len(attempts)-1] // most recent
		transfers, err := d.p.Store.TransfersForAttempt(ctx, active.ID)
		if err != nil {
			return err
		}
		allDone, anyFailed := len(transfers) > 0, false
		for _, t := range transfers {
			switch t.State {
			case core.TransferCompleted:
			case core.TransferErrored, core.TransferCancelled:
				anyFailed = true
				allDone = false
			default:
				allDone = false
			}
		}
		switch {
		case anyFailed:
			// A candidate failed, but other untried candidates usually remain, so
			// use the short backoff to try the next one soon rather than the long
			// "nothing new to try" backoff.
			d.log().Info("candidate download failed, cooling down", "album_job", job.ID, "attempt", active.ID)
			if err := d.p.Store.FailAttempt(ctx, active.ID, "transfer failed", now.Add(d.p.FailedCandidateBackoff), now); err != nil {
				d.log().Error("fail attempt failed", "attempt", active.ID, "err", err)
			}
			if err := d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.FailedCandidateBackoff), now); err != nil {
				d.log().Error("set cooldown failed", "album_job", job.ID, "err", err)
			}
		case allDone:
			d.log().Info("download complete, verifying", "album_job", job.ID)
			_ = d.p.Store.AdvanceJobState(ctx, job.ID, core.StateVerifying, now)
		}
	}
	return nil
}

// advanceImporting handles VERIFYING and IMPORTING jobs: it asks Lidarr what it
// would import from the album folder; a clean candidate is imported (COMPLETED),
// a rejected one fails the candidate (COOLDOWN, retried with the next candidate).
func (d *Discoverer) advanceImporting(ctx context.Context, now time.Time) error {
	jobs, err := d.p.Store.JobsInState(ctx, core.StateVerifying, d.p.Batch)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		attempts, err := d.p.Store.AttemptsForJob(ctx, job.ID)
		if err != nil || len(attempts) == 0 {
			continue
		}
		active := attempts[len(attempts)-1]
		transfers, err := d.p.Store.TransfersForAttempt(ctx, active.ID)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(transfers))
		for _, t := range transfers {
			names = append(names, t.Filename)
		}
		folder := AlbumFolder(d.p.CompleteDir, names)
		items, err := d.p.Music.ManualImportCandidates(ctx, folder)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			// Empty folder on a job whose transfers all completed means the files
			// were already imported (e.g. a crash between a prior successful
			// ExecuteManualImport and this state write). Treat it as done so
			// advanceImporting is idempotent across restarts.
			d.log().Info("empty folder treated as already imported", "album_job", job.ID, "folder", folder)
			_ = d.p.Store.SucceedAttempt(ctx, active.ID, now)
			if err := d.p.Store.AdvanceJobState(ctx, job.ID, core.StateCompleted, now); err != nil {
				d.log().Error("advance to completed failed", "album_job", job.ID, "err", err)
			}
			continue
		}
		var importable []lidarr.ManualImportItem
		blocked := false
		for _, it := range items {
			if it.Importable {
				importable = append(importable, it)
			} else {
				blocked = true
			}
		}
		if blocked || len(importable) == 0 {
			// Rejected like a failed download: other candidates usually remain, so
			// use the short backoff to try the next one soon.
			d.log().Info("import rejected", "album_job", job.ID, "folder", folder)
			_ = d.p.Store.FailAttempt(ctx, active.ID, "import rejected", now.Add(d.p.FailedCandidateBackoff), now)
			if err := d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.FailedCandidateBackoff), now); err != nil {
				d.log().Error("set cooldown failed", "album_job", job.ID, "err", err)
			}
			continue
		}
		if err := d.p.Music.ExecuteManualImport(ctx, importable); err != nil {
			return err
		}
		d.log().Info("imported album, completed", "album_job", job.ID, "files", len(importable))
		_ = d.p.Store.SucceedAttempt(ctx, active.ID, now)
		if err := d.p.Store.AdvanceJobState(ctx, job.ID, core.StateCompleted, now); err != nil {
			d.log().Error("advance to completed failed", "album_job", job.ID, "err", err)
		}
	}
	return nil
}
