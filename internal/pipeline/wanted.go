package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// WantedMusicSource is the slice of the Lidarr client WantedSync needs. A
// narrower interface than the full pipeline.MusicSource: WantedSync only
// ever fetches the wanted-missing list.
type WantedMusicSource interface {
	WantedMissing(ctx context.Context) ([]core.WantedRelease, error)
}

// WantedSyncStore is the slice of the store WantedSync needs.
type WantedSyncStore interface {
	UpsertWantedJob(ctx context.Context, lidarrAlbumID int64, now time.Time) (core.AlbumJob, error)
	UpdateJobMetadata(ctx context.Context, jobID int64, title, artistName, releaseDate string, artistID int64, now time.Time) error
	BackfillJobMetadataIfEmpty(ctx context.Context, jobID int64, title, artistName, releaseDate string, artistID int64) error
	CancelJobsNotWanted(ctx context.Context, wantedIDs []int64, now time.Time) (int, error)
	ReviveFailedJobs(ctx context.Context, wantedIDs []int64, cutoff, now time.Time) (int, error)
	PruneJobEvents(ctx context.Context, now time.Time) error
	// PruneSearchPasses deletes expired search_passes rows (see
	// store.PruneSearchPasses), on the same fixed 30-day window as job events.
	PruneSearchPasses(ctx context.Context, now time.Time) error
}

// WantedSyncParams configures a WantedSync.
type WantedSyncParams struct {
	Music             WantedMusicSource
	Store             WantedSyncStore
	Interval          time.Duration
	FailedReviveAfter time.Duration
	Logger            *slog.Logger
}

// WantedSync is the pipeline's entry module: it mirrors Lidarr's
// wanted-missing list into album_jobs (upserting WANTED jobs, cancelling
// jobs whose album left the list, reviving old FAILED jobs whose album is
// still wanted), and holds the most recent list in memory for Discovery to
// consult when building search queries.
type WantedSync struct {
	p WantedSyncParams

	mu     sync.Mutex
	wanted map[int64]core.WantedRelease
}

// NewWantedSync constructs a WantedSync.
func NewWantedSync(p WantedSyncParams) *WantedSync {
	if p.Logger != nil {
		p.Logger = p.Logger.With("module", "wanted_sync")
	}
	return &WantedSync{p: p}
}

// Name identifies this module in logs and Health().
func (w *WantedSync) Name() string { return "wanted_sync" }

// Interval is how often this module ticks.
func (w *WantedSync) Interval() time.Duration { return w.p.Interval }

func (w *WantedSync) log() *slog.Logger {
	if w.p.Logger != nil {
		return w.p.Logger
	}
	return slog.Default()
}

// Wanted returns a snapshot of the most recently successfully synced wanted
// list, keyed by Lidarr album ID. This is the one in-memory hand-off between
// pipeline modules (Discovery consumes it, from another goroutine, for album
// metadata when building its search query): it is read-only advisory data,
// never state that anything downstream persists. A fresh copy is returned
// each call so callers may not mutate WantedSync's own map.
func (w *WantedSync) Wanted() map[int64]core.WantedRelease {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[int64]core.WantedRelease, len(w.wanted))
	for k, v := range w.wanted {
		out[k] = v
	}
	return out
}

// Tick performs one sync pass: fetch Lidarr's wanted-missing list, upsert and
// refresh metadata for every album on it, cancel jobs whose album has left
// it, revive old FAILED jobs whose album is still wanted, prune expired job
// events and search passes, then publish the new snapshot for Wanted().
//
// Ordering is deliberate: cancellation and revival only run once the fetch
// has succeeded, so a Lidarr outage (WantedMissing erroring) leaves every job
// and the published snapshot untouched rather than cancelling every job in
// the pipeline.
func (w *WantedSync) Tick(ctx context.Context, now time.Time) error {
	albums, err := w.p.Music.WantedMissing(ctx)
	if err != nil {
		w.log().Error("wanted missing failed", "err", err)
		return fmt.Errorf("wanted missing: %w", err)
	}

	wantedIDs := make([]int64, 0, len(albums))
	for _, a := range albums {
		wantedIDs = append(wantedIDs, a.ID)

		job, err := w.p.Store.UpsertWantedJob(ctx, a.ID, now)
		if err != nil {
			return err
		}
		// A job still in WANTED gets a full metadata refresh every pass, so a
		// rename in Lidarr before the job is picked up still shows up. Jobs past
		// WANTED only get a targeted backfill (fields set only if currently
		// empty, updated_at untouched): UpdateJobMetadata bumps updated_at, and
		// stores like the failed-retry cooldown key off updated_at to time state
		// transitions, so a full refresh on every pass for a still-wanted
		// job further along the pipeline would keep resetting that clock and
		// starve those transitions. The backfill exists so jobs whose metadata
		// was never cached (e.g. created before this caching existed) self-heal.
		if job.State != core.StateWanted {
			if err := w.p.Store.BackfillJobMetadataIfEmpty(ctx, job.ID, a.Title, a.ArtistName, a.ReleaseDate, a.ArtistID); err != nil {
				return err
			}
			continue
		}
		if err := w.p.Store.UpdateJobMetadata(ctx, job.ID, a.Title, a.ArtistName, a.ReleaseDate, a.ArtistID, now); err != nil {
			return err
		}
	}

	// An empty wanted list from a successful fetch is treated as suspicious: a
	// transient empty response from Lidarr must not cancel every in-flight job
	// (CancelJobsNotWanted with an empty wantedIDs would cancel all non-terminal
	// jobs, since `lidarr_album_id <> ALL('{}')` is vacuously true). Skip
	// cancellation entirely this pass; the next successful non-empty sync
	// reconciles anything that genuinely left the list.
	if len(wantedIDs) == 0 {
		w.log().Info("wanted list empty, skipping cancellation (treating empty list as suspicious)")
	} else {
		cancelled, err := w.p.Store.CancelJobsNotWanted(ctx, wantedIDs, now)
		if err != nil {
			return err
		}
		if cancelled > 0 {
			w.log().Info("cancelled jobs no longer wanted", "count", cancelled)
		}
	}

	revived, err := w.p.Store.ReviveFailedJobs(ctx, wantedIDs, now.Add(-w.p.FailedReviveAfter), now)
	if err != nil {
		return err
	}
	if revived > 0 {
		w.log().Info("revived old failed jobs", "count", revived)
	}

	if err := w.p.Store.PruneJobEvents(ctx, now); err != nil {
		return err
	}
	if err := w.p.Store.PruneSearchPasses(ctx, now); err != nil {
		return err
	}

	snapshot := make(map[int64]core.WantedRelease, len(albums))
	for _, a := range albums {
		snapshot[a.ID] = a
	}
	w.mu.Lock()
	w.wanted = snapshot
	w.mu.Unlock()

	return nil
}
