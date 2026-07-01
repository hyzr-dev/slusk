package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
)

// DiscovererParams configures a Discoverer.
type DiscovererParams struct {
	Music            MusicSource
	Peers            PeerSearcher
	Store            DiscoveryStore
	Ranker           Ranker
	CompleteDir      string
	SearchTimeout    time.Duration
	TransferDeadline time.Duration
	CandidateBackoff time.Duration
	FailedRetryAfter time.Duration
	MaxCandidates    int
	Batch            int
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
	if err := d.syncWanted(ctx, now); err != nil {
		return err
	}
	if err := d.startNewJobs(ctx, now); err != nil {
		return err
	}
	if err := d.advanceDownloading(ctx, now); err != nil {
		return err
	}
	return d.advanceImporting(ctx, now)
}

// syncWanted upserts every wanted Lidarr album as a DISCOVERED job (idempotent).
func (d *Discoverer) syncWanted(ctx context.Context, now time.Time) error {
	albums, err := d.p.Music.WantedMissing(ctx)
	if err != nil {
		return fmt.Errorf("wanted missing: %w", err)
	}
	for _, a := range albums {
		if _, err := d.p.Store.UpsertDiscoveredJob(ctx, a.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// startNewJobs searches and enqueues for DISCOVERED jobs and due COOLDOWN jobs.
func (d *Discoverer) startNewJobs(ctx context.Context, now time.Time) error {
	jobs, err := d.p.Store.JobsInState(ctx, core.StateDiscovered, d.p.Batch)
	if err != nil {
		return err
	}
	due, err := d.p.Store.DueCooldownJobs(ctx, now, d.p.Batch)
	if err != nil {
		return err
	}
	for _, job := range append(jobs, due...) {
		if err := d.startJob(ctx, job, now); err != nil {
			d.log().Error("start job failed", "album_job", job.ID, "err", err)
		}
	}
	return nil
}

// startJob searches for one album, picks the best untried candidate that passes
// the quality floor, write-ahead enqueues its files, and moves the job to
// DOWNLOADING. If no candidate remains it goes to COOLDOWN, or FAILED once the
// candidate budget is exhausted.
func (d *Discoverer) startJob(ctx context.Context, job core.AlbumJob, now time.Time) error {
	if job.CandidatesTried >= d.p.MaxCandidates {
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

	album, err := d.albumFor(ctx, job)
	if err != nil {
		return err
	}
	results, err := d.p.Peers.Search(ctx, album.ArtistName+" "+album.Title, d.p.SearchTimeout)
	if err != nil {
		return err
	}
	candidates := d.p.Ranker.Rank(results)
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
			slskdID, err := d.p.Peers.Enqueue(ctx, cand.Username, f.Filename)
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
		return d.p.Store.AdvanceJobState(ctx, job.ID, core.StateDownloading, now)
	}
	// No untried candidate available now: back off.
	return d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.CandidateBackoff), now)
}

// albumFor returns the Lidarr album matching a job (by lidarr_album_id).
func (d *Discoverer) albumFor(ctx context.Context, job core.AlbumJob) (lidarr.WantedAlbum, error) {
	albums, err := d.p.Music.WantedMissing(ctx)
	if err != nil {
		return lidarr.WantedAlbum{}, err
	}
	for _, a := range albums {
		if a.ID == job.LidarrAlbumID {
			return a, nil
		}
	}
	return lidarr.WantedAlbum{}, fmt.Errorf("album %d no longer wanted", job.LidarrAlbumID)
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
			if err := d.p.Store.FailAttempt(ctx, active.ID, "transfer failed", now.Add(d.p.CandidateBackoff), now); err != nil {
				d.log().Error("fail attempt failed", "attempt", active.ID, "err", err)
			}
			if err := d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.CandidateBackoff), now); err != nil {
				d.log().Error("set cooldown failed", "album_job", job.ID, "err", err)
			}
		case allDone:
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
			d.log().Info("import rejected", "album_job", job.ID, "folder", folder)
			_ = d.p.Store.FailAttempt(ctx, active.ID, "import rejected", now.Add(d.p.CandidateBackoff), now)
			if err := d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.CandidateBackoff), now); err != nil {
				d.log().Error("set cooldown failed", "album_job", job.ID, "err", err)
			}
			continue
		}
		if err := d.p.Music.ExecuteManualImport(ctx, importable); err != nil {
			return err
		}
		_ = d.p.Store.SucceedAttempt(ctx, active.ID, now)
		if err := d.p.Store.AdvanceJobState(ctx, job.ID, core.StateCompleted, now); err != nil {
			d.log().Error("advance to completed failed", "album_job", job.ID, "err", err)
		}
	}
	return nil
}
