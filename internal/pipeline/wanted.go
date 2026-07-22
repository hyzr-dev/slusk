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
	SyncWantedJobs(ctx context.Context, releases []core.WantedRelease, failedCutoff, now time.Time) (cancelled, revived int, err error)
	PruneJobEvents(ctx context.Context, now time.Time) error
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
// events, then publish the new snapshot for Wanted().
//
// Ordering is deliberate: cancellation and revival only run once the fetch
// has succeeded, so a Lidarr outage (WantedMissing erroring) leaves every job
// and the published snapshot untouched rather than cancelling every job in
// the pipeline.
func (w *WantedSync) Tick(ctx context.Context, now time.Time) error {
	started := time.Now()
	albums, err := w.p.Music.WantedMissing(ctx)
	if err != nil {
		w.log().Error("wanted missing failed", "err", err)
		return fmt.Errorf("wanted missing: %w", err)
	}

	cancelled, revived, err := w.p.Store.SyncWantedJobs(ctx, albums, now.Add(-w.p.FailedReviveAfter), now)
	if err != nil {
		return err
	}
	if cancelled > 0 {
		w.log().Info("cancelled jobs no longer wanted", "count", cancelled)
	}
	if revived > 0 {
		w.log().Info("revived old failed jobs", "count", revived)
	}

	// Pruning remains a separate operation: issue #125 owns changing its
	// transaction boundaries. Do not publish a snapshot unless both persistence
	// operations have succeeded.
	if err := w.p.Store.PruneJobEvents(ctx, now); err != nil {
		return err
	}

	// Map assignment naturally makes the last duplicate occurrence authoritative,
	// matching Store.SyncWantedJobs' reconciliation semantics.
	snapshot := make(map[int64]core.WantedRelease, len(albums))
	for _, album := range albums {
		snapshot[album.ID] = album
	}
	w.mu.Lock()
	w.wanted = snapshot
	w.mu.Unlock()

	w.log().Info("wanted sync complete", "albums", len(snapshot), "duration", time.Since(started))
	return nil
}
