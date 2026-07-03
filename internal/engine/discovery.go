package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
)

// errAlbumNoLongerWanted signals that a job's album has left Lidarr's wanted
// list, so the job should be cancelled rather than retried indefinitely.
var errAlbumNoLongerWanted = errors.New("album no longer wanted")

// DiscovererParams configures a Discoverer.
type DiscovererParams struct {
	Music                  MusicSource
	Peers                  PeerSearcher
	Store                  DiscoveryStore
	Ranker                 Ranker
	CompleteDir            string
	SearchTimeout          time.Duration
	TransferDeadline       time.Duration
	CandidateBackoff       time.Duration
	FailedCandidateBackoff time.Duration
	FailedRetryAfter       time.Duration
	ImportConfirmTimeout   time.Duration
	MaxCandidates          int
	Batch                  int
	MaxActive              int
	MaxInflightPerPeer     int
	// MaxCandidateFileRatio rejects a candidate whose file count exceeds the
	// album's known Lidarr track count by more than this multiple (e.g. 2 means
	// a candidate offering more than 2x the expected tracks is skipped). Guards
	// against a Soulseek share that dumps an artist's whole discography into one
	// flat folder being mistaken for a single album. Ignored when the album's
	// expected track count is unknown (0), since Lidarr is the final arbiter of
	// import correctness downstream.
	MaxCandidateFileRatio float64
	// MaxTransferRetries bounds how many times a transfer can be returned to
	// PENDING after a failed enqueue attempt before it is marked terminally
	// ERRORED. Mirrors the retry budget the Reconciler enforces for transfers
	// that reached slskd but then failed there.
	MaxTransferRetries int
	Logger             *slog.Logger
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
	wanted, err := d.SyncWanted(ctx, now)
	if err != nil {
		return err
	}
	return d.Advance(ctx, wanted, now)
}

// SyncWanted fetches the current wanted-missing list from Lidarr, upserts each
// as a DISCOVERED job (idempotent), and returns the albums keyed by Lidarr
// album ID for use by Advance. It is intended to run on Lidarr's own poll
// interval, independent of how often Advance ticks the state machine.
func (d *Discoverer) SyncWanted(ctx context.Context, now time.Time) (map[int64]lidarr.WantedAlbum, error) {
	albums, err := d.p.Music.WantedMissing(ctx)
	if err != nil {
		return nil, fmt.Errorf("wanted missing: %w", err)
	}
	wanted := make(map[int64]lidarr.WantedAlbum, len(albums))
	for _, a := range albums {
		wanted[a.ID] = a
	}
	if err := d.syncWanted(ctx, albums, now); err != nil {
		return nil, err
	}
	return wanted, nil
}

// Advance takes every job one transition forward using the most recently
// synced wanted map. Every stage is bounded and idempotent, so a crash
// mid-tick loses nothing.
func (d *Discoverer) Advance(ctx context.Context, wanted map[int64]lidarr.WantedAlbum, now time.Time) error {
	if err := d.retryFailedJobs(ctx, now); err != nil {
		return err
	}
	if err := d.startNewJobs(ctx, wanted, now); err != nil {
		return err
	}
	// advanceDownloading must run before topUpDownloads: it settles any attempt
	// that already has a failed transfer (e.g. from an earlier reconcile pass)
	// and cleans up its folder. Topping up first would release that same
	// attempt's still-PENDING siblings to slskd, into a folder advanceDownloading
	// is about to delete moments later in this same tick.
	if err := d.advanceDownloading(ctx, now); err != nil {
		return err
	}
	if err := d.topUpDownloads(ctx, now); err != nil {
		return err
	}
	if err := d.advanceImporting(ctx, now); err != nil {
		return err
	}
	return d.confirmImports(ctx, now)
}

// syncWanted upserts every wanted Lidarr album as a DISCOVERED job (idempotent),
// and refreshes the job's cached title/artist metadata every pass while it sits
// in DISCOVERED, so a rename in Lidarr before the job is picked up still shows
// up. Jobs already past DISCOVERED only get a targeted backfill (title/artist
// set only if currently empty, updated_at untouched): UpdateJobMetadata bumps
// updated_at, and stores like DueFailedJobs key off updated_at to time state
// transitions (e.g. the failed-retry cooldown), so a full refresh on every
// pass for a still-wanted FAILED/DOWNLOADING/etc. job would keep resetting
// that clock and starve retries. The backfill exists so jobs whose metadata
// was never cached (e.g. created before this caching existed) self-heal.
func (d *Discoverer) syncWanted(ctx context.Context, albums []lidarr.WantedAlbum, now time.Time) error {
	for _, a := range albums {
		job, err := d.p.Store.UpsertDiscoveredJob(ctx, a.ID, now)
		if err != nil {
			return err
		}
		if job.State != core.StateDiscovered {
			if err := d.p.Store.BackfillJobMetadataIfEmpty(ctx, job.ID, a.Title, a.ArtistName, a.ReleaseDate); err != nil {
				return err
			}
			continue
		}
		if err := d.p.Store.UpdateJobMetadata(ctx, job.ID, a.Title, a.ArtistName, a.ReleaseDate, now); err != nil {
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
	query := album.ArtistName + " " + album.Title
	results, err := d.p.Peers.Search(ctx, query, d.p.SearchTimeout)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		// The primary query returned no raw results at all (not just no
		// candidates after ranking/filtering - that's the results' fault, not
		// the query's). Try once with a looser, normalized query: peers'
		// shared folder names rarely carry suffixes like "(Deluxe Edition)" or
		// characters like "&" verbatim, so stripping them can turn a zero-hit
		// search into a match. Skipped entirely when normalizing is a no-op
		// (to avoid doubling search traffic for nothing) or reduces the query
		// to nothing (searching for "" is meaningless).
		if fallback := normalizeQuery(query); fallback != "" && fallback != query {
			d.log().Info("primary search empty, trying normalized query",
				"album_job", job.ID, "query", fallback)
			results, err = d.p.Peers.Search(ctx, fallback, d.p.SearchTimeout)
			if err != nil {
				return err
			}
			query = fallback
		}
	}
	candidates := d.p.Ranker.Rank(results)
	d.log().Info("searched album",
		"album_job", job.ID, "query", query,
		"results", len(results), "candidates", len(candidates))
	// Fetch the album's expected track count once per startJob call (not per
	// candidate) to size-sanity-check candidates below. total == 0 means Lidarr
	// has no reliable count for this album right now, so the check is skipped
	// entirely rather than risk rejecting a legitimate candidate on bad data.
	_, total, err := d.p.Music.AlbumStatus(ctx, job.LidarrAlbumID)
	if err != nil {
		return err
	}
	for _, cand := range candidates {
		if tried[cand.Username] {
			continue
		}
		if total > 0 && float64(len(cand.Files)) > float64(total)*d.p.MaxCandidateFileRatio {
			// A share this oversized for the expected album is almost certainly not
			// a single release (e.g. an artist's whole discography dumped into one
			// flat folder) - skip it like an already-tried candidate rather than
			// attempt it. Not counted toward CandidatesTried individually: if no
			// other candidate exists this tick, the budget is still spent exactly
			// once via the "no untried candidate available" path below, the same as
			// if this candidate had never appeared at all.
			d.log().Info("candidate file count implausible for album, skipping",
				"album_job", job.ID, "user", cand.Username, "files", len(cand.Files), "expected", total)
			continue
		}
		if total > 0 && len(cand.Files) < total {
			// A candidate that can't even cover the expected track count is
			// guaranteed to be rejected by the VERIFYING completeness gate
			// (coverage(importable) < total) after burning a full download cycle,
			// a candidate slot, and a cooldown - so downloading it is guaranteed
			// wasted work. Skip it like an already-tried candidate rather than
			// attempt it. Not counted toward CandidatesTried individually: if no
			// other candidate exists this tick, the budget is still spent exactly
			// once via the "no untried candidate available" path below, the same as
			// if this candidate had never appeared at all.
			d.log().Info("candidate has fewer files than expected tracks, skipping",
				"album_job", job.ID, "user", cand.Username, "files", len(cand.Files), "expected", total)
			continue
		}
		attemptID, err := d.p.Store.CreateAttempt(ctx, job.ID, cand.Username, cand.Score, now)
		if err != nil {
			return err
		}
		// Write-ahead every file as PENDING (survives a restart), then hand only a
		// bounded number to slskd now; the rest are promoted as in-flight
		// transfers finish (topUpAttempt / topUpDownloads). Enqueuing a whole album
		// at once trips Soulseek peers' per-user queued-megabyte limit, which they
		// answer with "Too many megabytes" rejections on the overflow files.
		for _, f := range cand.Files {
			if err := d.p.Store.RecordPendingTransfer(ctx, attemptID, cand.Username, f.Filename, f.Size, now); err != nil {
				return err
			}
		}
		if err := d.p.Store.IncrementCandidatesTried(ctx, job.ID, now); err != nil {
			return err
		}
		sent, err := d.topUpAttempt(ctx, attemptID, now)
		if err != nil {
			return err
		}
		d.log().Info("enqueued candidate, downloading",
			"album_job", job.ID, "lidarr_album", job.LidarrAlbumID,
			"user", cand.Username, "files", len(cand.Files),
			"sent", sent, "deferred", len(cand.Files)-sent)
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

// topUpAttempt hands an attempt's PENDING files to slskd until MaxInflightPerPeer
// transfers are in flight, sending more only as earlier ones finish. Keeping the
// queued count bounded stops the peer's per-user queued-megabyte limit from
// rejecting a burst. PENDING files carry their size in bytes_total (set at
// RecordPendingTransfer time) so they can be enqueued here without the search
// result in hand. Files are sent in filename order for deterministic progress.
func (d *Discoverer) topUpAttempt(ctx context.Context, attemptID int64, now time.Time) (int, error) {
	transfers, err := d.p.Store.TransfersForAttempt(ctx, attemptID)
	if err != nil {
		return 0, err
	}
	inflight := 0
	var pending []core.Transfer
	for _, tr := range transfers {
		switch tr.State {
		case core.TransferQueued, core.TransferInProgress, core.TransferStalled:
			inflight++
		case core.TransferPending:
			pending = append(pending, tr)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Filename < pending[j].Filename })

	deadline := now.Add(d.p.TransferDeadline)
	sent := 0
	for _, p := range pending {
		if inflight >= d.p.MaxInflightPerPeer {
			break
		}
		// Promote PENDING -> QUEUED with a real deadline, then hand it to slskd.
		tid, err := d.p.Store.RecordEnqueueIntent(ctx, attemptID, p.Username, p.Filename, deadline, now)
		if err != nil {
			return sent, err
		}
		slskdID, err := d.p.Peers.Enqueue(ctx, p.Username, p.Filename, p.BytesTotal)
		if err != nil {
			if p.Retries < d.p.MaxTransferRetries {
				d.log().Info("enqueue failed, returning to pending", "user", p.Username, "file", p.Filename, "retries", p.Retries, "err", err)
				if uerr := d.p.Store.RetryTransfer(ctx, tid, now); uerr != nil {
					d.log().Error("retry transfer failed", "transfer", tid, "err", uerr)
				}
			} else {
				d.log().Error("enqueue failed", "user", p.Username, "file", p.Filename, "err", err)
				if uerr := d.p.Store.UpdateTransferProgress(ctx, tid, core.TransferErrored, 0, 0, now); uerr != nil {
					d.log().Error("mark transfer errored failed", "transfer", tid, "err", uerr)
				}
			}
			continue
		}
		_ = d.p.Store.AttachTransferID(ctx, tid, slskdID, now)
		inflight++
		sent++
	}
	return sent, nil
}

// topUpDownloads promotes more PENDING files for every DOWNLOADING job whose
// in-flight count has dropped below MaxInflightPerPeer, so a throttled candidate
// keeps making progress across ticks as its earlier files complete.
func (d *Discoverer) topUpDownloads(ctx context.Context, now time.Time) error {
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
		sent, err := d.topUpAttempt(ctx, active.ID, now)
		if err != nil {
			d.log().Error("top up downloads failed", "album_job", job.ID, "err", err)
		}
		if sent > 0 {
			d.log().Info("released deferred downloads",
				"album_job", job.ID, "user", active.Username, "sent", sent)
		}
	}
	return nil
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
//
// The fail path is two-phase: cancel first, clean up once everything is quiet.
// When an attempt fails while some of its siblings are still QUEUED/IN_PROGRESS/
// STALLED in slskd, cleaning up (deleting the attempt's download folder) would
// race those live downloads - slskd keeps writing their bytes back into the
// folder we just deleted, re-creating exactly the cross-peer collision
// cleanupAttempt exists to prevent. So we first cancel the still-active siblings
// in slskd (and mark never-sent PENDING siblings CANCELLED directly, since there
// is nothing in slskd to cancel), then wait: cleanup, FailAttempt and the
// cooldown are deferred to a later tick once every transfer is terminal.
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
		var activeSiblings, pendingSiblings []core.Transfer
		for _, t := range transfers {
			switch t.State {
			case core.TransferCompleted:
			case core.TransferErrored, core.TransferCancelled:
				anyFailed = true
				allDone = false
			case core.TransferPending:
				// Never sent to slskd; nothing to cancel there, but still not terminal.
				pendingSiblings = append(pendingSiblings, t)
				allDone = false
			default: // QUEUED, IN_PROGRESS, STALLED: still live in slskd.
				activeSiblings = append(activeSiblings, t)
				allDone = false
			}
		}
		switch {
		case anyFailed:
			// A candidate failed, but other untried candidates usually remain, so
			// use the short backoff to try the next one soon rather than the long
			// "nothing new to try" backoff.
			//
			// Two-phase fail: never-sent PENDING siblings are cancelled straight in
			// the DB (nothing exists in slskd to cancel, and without this the attempt
			// would never reach "all terminal"); still-active siblings are cancelled
			// in slskd. While any sibling is still live we defer cleanup/FailAttempt/
			// cooldown to a later tick - deleting the folder now would race those
			// downloads. The branch re-runs every tick, so the job converges to
			// "all terminal" within a few ticks.
			for _, t := range pendingSiblings {
				if err := d.p.Store.UpdateTransferProgress(ctx, t.ID, core.TransferCancelled, t.BytesDone, t.BytesTotal, now); err != nil {
					d.log().Error("cancel pending sibling failed", "transfer", t.ID, "err", err)
				}
			}
			if len(activeSiblings) > 0 {
				d.log().Info("candidate failed, cancelling active siblings before cleanup",
					"album_job", job.ID, "attempt", active.ID, "active", len(activeSiblings))
				for _, t := range activeSiblings {
					if t.SlskdID == "" {
						// No slskd id yet (enqueue never returned one); the reconciler's
						// fallback matching will terminate it. Leave it for a later tick.
						continue
					}
					if err := d.p.Peers.Cancel(ctx, t.Username, t.SlskdID); err != nil {
						// Leave it active; the next tick retries the cancel.
						d.log().Error("cancel active sibling failed", "album_job", job.ID, "transfer", t.ID, "err", err)
					}
				}
				continue
			}
			// Every transfer is terminal: safe to clean up and fail the attempt.
			d.log().Info("candidate download failed, cooling down", "album_job", job.ID, "attempt", active.ID)
			names := make([]string, 0, len(transfers))
			for _, t := range transfers {
				names = append(names, t.Filename)
			}
			d.cleanupAttempt(ctx, job.ID, names)
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

// advanceImporting is the VERIFYING gate: it asks Lidarr what it would import
// from the album folder and decides whether to import at all. A candidate with
// any rejection fails outright (COOLDOWN, retried with the next candidate). A
// clean candidate that cannot cover the whole release is also failed, rather
// than importing a partial album. A clean, complete candidate is imported and
// the job moves to IMPORTING for confirmImports to confirm Lidarr accepted it.
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
			d.log().Error("manual import candidates failed", "album_job", job.ID, "folder", folder, "err", err)
			d.escalateIfStuck(ctx, job, active.ID, names, "import candidates failed", now)
			continue
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
		var rejections []string
		for _, it := range items {
			if it.Importable {
				importable = append(importable, it)
			} else {
				rejections = append(rejections, it.Rejections...)
			}
		}
		if len(rejections) > 0 || len(importable) == 0 {
			// Rejected like a failed download: other candidates usually remain, so
			// use the short backoff to try the next one soon.
			d.log().Info("import rejected", "album_job", job.ID, "folder", folder, "reasons", rejections)
			d.cleanupAttempt(ctx, job.ID, names)
			_ = d.p.Store.FailAttempt(ctx, active.ID, "import rejected", now.Add(d.p.FailedCandidateBackoff), now)
			if err := d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.FailedCandidateBackoff), now); err != nil {
				d.log().Error("set cooldown failed", "album_job", job.ID, "err", err)
			}
			continue
		}
		_, total, err := d.p.Music.AlbumStatus(ctx, job.LidarrAlbumID)
		if err != nil {
			d.log().Error("album status failed", "album_job", job.ID, "err", err)
			d.escalateIfStuck(ctx, job, active.ID, names, "album status check failed", now)
			continue
		}
		if coverage(importable) < total {
			// A source that can't complete the release is rejected outright rather
			// than partially imported, to keep the library free of half albums.
			// Other candidates usually remain, so use the short backoff.
			d.log().Info("incomplete download, rejecting", "album_job", job.ID, "folder", folder,
				"covered", coverage(importable), "total", total)
			d.cleanupAttempt(ctx, job.ID, names)
			_ = d.p.Store.FailAttempt(ctx, active.ID, "incomplete download", now.Add(d.p.FailedCandidateBackoff), now)
			if err := d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.FailedCandidateBackoff), now); err != nil {
				d.log().Error("set cooldown failed", "album_job", job.ID, "err", err)
			}
			continue
		}
		if err := d.p.Music.ExecuteManualImport(ctx, importable); err != nil {
			return err
		}
		d.log().Info("import executed, awaiting confirmation", "album_job", job.ID, "files", len(importable))
		if err := d.p.Store.AdvanceJobState(ctx, job.ID, core.StateImporting, now); err != nil {
			d.log().Error("advance to importing failed", "album_job", job.ID, "err", err)
		}
	}
	return nil
}

// escalateIfStuck fails and cools down a VERIFYING job's active attempt once
// it has been stuck (no state change) longer than ImportConfirmTimeout, so a
// job whose Lidarr call (ManualImportCandidates or AlbumStatus) keeps
// erroring every tick forever - e.g. Lidarr repeatedly timing out on a broken
// folder - does not stay stuck in VERIFYING indefinitely, starving every
// other job's discovery tick. Called after logging the triggering error;
// within the timeout it is a no-op, so the job simply gets retried next tick.
func (d *Discoverer) escalateIfStuck(ctx context.Context, job core.AlbumJob, attemptID int64, filenames []string, reason string, now time.Time) {
	if now.Sub(job.UpdatedAt) <= d.p.ImportConfirmTimeout {
		return
	}
	d.log().Info("verifying stuck past timeout, cooling down", "album_job", job.ID, "reason", reason)
	d.cleanupAttempt(ctx, job.ID, filenames)
	if err := d.p.Store.FailAttempt(ctx, attemptID, reason, now.Add(d.p.FailedCandidateBackoff), now); err != nil {
		d.log().Error("fail attempt failed", "attempt", attemptID, "err", err)
	}
	if err := d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.FailedCandidateBackoff), now); err != nil {
		d.log().Error("set cooldown failed", "album_job", job.ID, "err", err)
	}
}

// cleanupAttempt best-effort deletes a failed attempt's leftover files from
// slskd's downloads root, so they don't get mixed into the next candidate's
// local folder (slskd names local subfolders after the remote peer's own leaf
// directory name, so two different peers sharing an identically-named folder
// can otherwise collide, corrupting Lidarr's later import scan). It skips the
// delete entirely when filenames don't share one common remote directory
// (commonLeaf == ""): that's ambiguous, and slskd's API only accepts one
// relative subdirectory name, so guessing wrong risks deleting more than this
// attempt wrote. A delete failure is logged and otherwise ignored — it must
// not block the job from moving on to its next candidate. A 404 means the
// attempt never wrote any bytes (e.g. it failed before any transfer started),
// which is routine, so it's logged quietly rather than as an ERROR.
func (d *Discoverer) cleanupAttempt(ctx context.Context, jobID int64, filenames []string) {
	leaf := commonLeaf(filenames)
	if leaf == "" {
		return
	}
	err := d.p.Peers.DeleteDownloadFolder(ctx, leaf)
	switch {
	case err == nil:
	case slskd.IsNotFound(err):
		d.log().Info("nothing to clean up for failed attempt", "album_job", jobID, "folder", leaf)
	default:
		d.log().Error("cleanup failed attempt's downloaded files failed", "album_job", jobID, "folder", leaf, "err", err)
	}
}

// coverage counts the distinct Lidarr track IDs covered by importable, used to
// judge whether a candidate can complete a release's full track count.
func coverage(importable []lidarr.ManualImportItem) int {
	seen := make(map[int64]struct{})
	for _, it := range importable {
		for _, id := range it.TrackIDs {
			seen[id] = struct{}{}
		}
	}
	return len(seen)
}

// confirmImports handles IMPORTING jobs: Lidarr's ManualImport command is
// asynchronous, so rather than trusting the command's HTTP response, this
// polls the album's completeness directly. A release that becomes complete is
// COMPLETED; one that stays incomplete past ImportConfirmTimeout is failed
// (COOLDOWN, retried with the next candidate) so a stuck or dropped async
// import doesn't leave the job stranded in IMPORTING forever.
func (d *Discoverer) confirmImports(ctx context.Context, now time.Time) error {
	jobs, err := d.p.Store.JobsInState(ctx, core.StateImporting, d.p.Batch)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		attempts, err := d.p.Store.AttemptsForJob(ctx, job.ID)
		if err != nil || len(attempts) == 0 {
			continue
		}
		active := attempts[len(attempts)-1]
		present, total, err := d.p.Music.AlbumStatus(ctx, job.LidarrAlbumID)
		if err != nil {
			d.log().Error("album status failed", "album_job", job.ID, "err", err)
			if now.Sub(job.UpdatedAt) > d.p.ImportConfirmTimeout {
				d.log().Info("import not confirmed in time, cooling down", "album_job", job.ID)
				_ = d.p.Store.FailAttempt(ctx, active.ID, "import not confirmed", now.Add(d.p.FailedCandidateBackoff), now)
				if err := d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.FailedCandidateBackoff), now); err != nil {
					d.log().Error("set cooldown failed", "album_job", job.ID, "err", err)
				}
			}
			continue
		}
		if present >= total {
			d.log().Info("import confirmed, completed", "album_job", job.ID)
			_ = d.p.Store.SucceedAttempt(ctx, active.ID, now)
			if err := d.p.Store.AdvanceJobState(ctx, job.ID, core.StateCompleted, now); err != nil {
				d.log().Error("advance to completed failed", "album_job", job.ID, "err", err)
			}
			continue
		}
		if now.Sub(job.UpdatedAt) > d.p.ImportConfirmTimeout {
			d.log().Info("import not confirmed in time, cooling down", "album_job", job.ID, "present", present, "total", total)
			_ = d.p.Store.FailAttempt(ctx, active.ID, "import not confirmed", now.Add(d.p.FailedCandidateBackoff), now)
			if err := d.p.Store.SetJobCooldown(ctx, job.ID, now.Add(d.p.FailedCandidateBackoff), now); err != nil {
				d.log().Error("set cooldown failed", "album_job", job.ID, "err", err)
			}
		}
	}
	return nil
}
