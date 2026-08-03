// Package store: passes.go holds the search_passes table (see
// migrations/0002_search_passes.sql), the Discovery search-pass history
// backing the Overview charts (GET /api/charts, issue #88), plus the
// completed-downloads-per-hour aggregation drawn from job_events.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// searchPassRetention bounds how long search_passes rows are kept;
// PruneSearchPasses deletes anything older. Matches jobEventRetention's fixed
// 30-day window (see events.go).
const searchPassRetention = 30 * 24 * time.Hour

// RecordSearchPass appends one row recording a completed Discovery search
// cycle.
func (s *Store) RecordSearchPass(ctx context.Context, p core.SearchPass) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO search_passes (started_at, finished_at, searched, matched) VALUES ($1, $2, $3, $4)`,
		p.StartedAt, p.FinishedAt, p.Searched, p.Matched)
	if err != nil {
		return fmt.Errorf("record search pass: %w", err)
	}
	return nil
}

// RecentSearchPasses returns the most recent search passes, newest first,
// capped at limit. Backs the Overview charts (GET /api/charts).
func (s *Store) RecentSearchPasses(ctx context.Context, limit int) ([]core.SearchPass, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, started_at, finished_at, searched, matched
		 FROM search_passes ORDER BY started_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent search passes: %w", err)
	}
	defer rows.Close()

	var out []core.SearchPass
	for rows.Next() {
		var p core.SearchPass
		if err := rows.Scan(&p.ID, &p.StartedAt, &p.FinishedAt, &p.Searched, &p.Matched); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneSearchPasses deletes search_passes rows older than
// searchPassRetention.
func (s *Store) PruneSearchPasses(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM search_passes WHERE started_at < $1`, now.Add(-searchPassRetention))
	if err != nil {
		return fmt.Errorf("prune search passes: %w", err)
	}
	return nil
}

// CompletedByHour returns the count of attempt_succeeded job events per hour
// since the given time, sparse (only hours with at least one event), oldest
// first. Callers zero-fill missing hours. Backs GET /api/charts.
//
// The bucketing truncates in UTC explicitly (AT TIME ZONE 'UTC' twice: once
// to interpret created_at's instant as a UTC timestamp for truncation, once
// to reattach the UTC zone to the resulting timestamp) rather than relying on
// date_trunc's implicit session-timezone truncation. Without this, a session
// timezone with a fractional UTC offset would truncate to bucket boundaries
// like :30 that never match the Go side's UTC time.Truncate(time.Hour) keys
// in toChartsDTO, so every row would silently fail to zero-fill into place.
func (s *Store) CompletedByHour(ctx context.Context, since time.Time) ([]core.HourCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT date_trunc('hour', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', count(*)
		 FROM job_events
		 WHERE event = $1 AND created_at >= $2 GROUP BY 1 ORDER BY 1`,
		string(core.EventAttemptSucceeded), since)
	if err != nil {
		return nil, fmt.Errorf("completed by hour: %w", err)
	}
	defer rows.Close()

	var out []core.HourCount
	for rows.Next() {
		var hc core.HourCount
		if err := rows.Scan(&hc.Hour, &hc.Count); err != nil {
			return nil, err
		}
		hc.Hour = hc.Hour.UTC()
		out = append(out, hc)
	}
	return out, rows.Err()
}
