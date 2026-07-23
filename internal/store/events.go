// Package store: events.go holds the job_events audit trail (see migrations/0001_baseline_schema.sql).
// Writes are best-effort from the engine's perspective — see the callers in
// internal/engine — but the store's own write/read paths use normal error
// semantics like every other store method.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// jobEventRetention bounds how long job_events rows are kept; PruneJobEvents
// deletes anything older. Hardcoded (not configurable) per issue #34's scope:
// a fixed 30-day window is enough for troubleshooting without unbounded growth.
const (
	jobEventRetention       = 30 * 24 * time.Hour
	jobEventPruneBatchSize  = 1000
	jobEventPruneMaxBatches = 100
)

// AddJobEvent appends one row to a job's audit trail.
func (s *Store) AddJobEvent(ctx context.Context, jobID int64, event core.JobEventType, detail string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO job_events (album_job_id, event, detail, created_at) VALUES ($1, $2, $3, $4)`,
		jobID, string(event), detail, now)
	if err != nil {
		return fmt.Errorf("add job event: %w", err)
	}
	return nil
}

const jobEventSelect = `SELECT id, album_job_id, event, detail, created_at FROM job_events`

func scanJobEvents(rows *sql.Rows) ([]core.JobEvent, error) {
	var out []core.JobEvent
	for rows.Next() {
		var e core.JobEvent
		var event string
		if err := rows.Scan(&e.ID, &e.AlbumJobID, &event, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Event = core.JobEventType(event)
		out = append(out, e)
	}
	return out, rows.Err()
}

// JobEvents returns one job's audit trail, newest first.
func (s *Store) JobEvents(ctx context.Context, jobID int64) ([]core.JobEvent, error) {
	rows, err := s.db.QueryContext(ctx, jobEventSelect+` WHERE album_job_id = $1 ORDER BY created_at DESC, id DESC`, jobID)
	if err != nil {
		return nil, fmt.Errorf("job events: %w", err)
	}
	defer rows.Close()
	return scanJobEvents(rows)
}

// RecentEvents returns the most recent events across every job, newest first,
// capped at limit. Backs the dashboard's global event timeline (GET /api/events).
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]core.JobEvent, error) {
	rows, err := s.db.QueryContext(ctx, jobEventSelect+` ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent events: %w", err)
	}
	defer rows.Close()
	return scanJobEvents(rows)
}

// PruneJobEvents deletes expired events in short batches, up to
// jobEventPruneMaxBatches per call. The cap keeps the hourly pipeline tick
// bounded while allowing a retention backlog to drain faster than events are
// written.
func (s *Store) PruneJobEvents(ctx context.Context, now time.Time) error {
	return s.pruneJobEvents(ctx, now.Add(-jobEventRetention), jobEventPruneMaxBatches)
}

func (s *Store) pruneJobEvents(ctx context.Context, cutoff time.Time, maxBatches int) error {
	for batch := 0; batch < maxBatches; batch++ {
		result, err := s.db.ExecContext(ctx, `
			DELETE FROM job_events
			WHERE id IN (
				SELECT id
				FROM job_events
				WHERE created_at < $1
				ORDER BY created_at, id
				LIMIT $2
			)`, cutoff, jobEventPruneBatchSize)
		if err != nil {
			return fmt.Errorf("prune job events batch %d: %w", batch+1, err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("prune job events batch %d rows affected: %w", batch+1, err)
		}
		if deleted < jobEventPruneBatchSize {
			break
		}
	}
	return nil
}
