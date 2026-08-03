// Package store: throughput.go holds the throughput_minutes table (see
// migrations/0005_throughput_minutes.sql), the per-minute download-throughput
// history backing the Overview charts (GET /api/charts, issue #157).
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
)

// throughputRetention bounds how long throughput_minutes rows are kept;
// PruneThroughputMinutes deletes anything older. Matches searchPassRetention
// and jobEventRetention's fixed 30-day window.
const throughputRetention = 30 * 24 * time.Hour

// RecordThroughputMinute upserts one completed per-minute download-throughput
// rollup. A shutdown flushes a partial minute (fewer samples) that a later
// run may re-flush more completely after the process restarts mid-minute and
// resumes sampling the same wall-clock minute; the ON CONFLICT clause only
// overwrites when the new row has strictly more samples than the one already
// stored, so applying the two writes in either order converges on the more
// complete row — restart order is irrelevant.
func (s *Store) RecordThroughputMinute(ctx context.Context, m core.ThroughputMinute) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO throughput_minutes (minute, avg_bytes_per_sec, max_bytes_per_sec, max_active, samples)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (minute) DO UPDATE SET
		   avg_bytes_per_sec = excluded.avg_bytes_per_sec,
		   max_bytes_per_sec = excluded.max_bytes_per_sec,
		   max_active = excluded.max_active,
		   samples = excluded.samples
		 WHERE excluded.samples > throughput_minutes.samples`,
		m.Minute, m.AvgBytesPerSecond, m.MaxBytesPerSecond, m.MaxActive, m.Samples)
	if err != nil {
		return fmt.Errorf("record throughput minute: %w", err)
	}
	return nil
}

// ThroughputMinutes returns every throughput_minutes row at or after since,
// oldest first. This is the read side of the persisted per-minute throughput
// history (RecordThroughputMinute is the write side); unlike RecentSearchPasses
// and CompletedByHour, no production caller wires this up yet — GET
// /api/charts serves only the in-memory live sparkline (see
// soulseek.Client.ThroughputSamples), not this table's history. It exists so
// history survives a restart, ready for a future caller to read.
func (s *Store) ThroughputMinutes(ctx context.Context, since time.Time) ([]core.ThroughputMinute, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT minute, avg_bytes_per_sec, max_bytes_per_sec, max_active, samples
		 FROM throughput_minutes WHERE minute >= $1 ORDER BY minute`, since)
	if err != nil {
		return nil, fmt.Errorf("throughput minutes: %w", err)
	}
	defer rows.Close()

	var out []core.ThroughputMinute
	for rows.Next() {
		var m core.ThroughputMinute
		if err := rows.Scan(&m.Minute, &m.AvgBytesPerSecond, &m.MaxBytesPerSecond, &m.MaxActive, &m.Samples); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PruneThroughputMinutes deletes throughput_minutes rows older than
// throughputRetention.
func (s *Store) PruneThroughputMinutes(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM throughput_minutes WHERE minute < $1`, now.Add(-throughputRetention))
	if err != nil {
		return fmt.Errorf("prune throughput minutes: %w", err)
	}
	return nil
}
