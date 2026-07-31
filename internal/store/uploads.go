// Package store: uploads.go holds the upload_history table (see
// migrations/0010_upload_history.sql), the durable record of what the native
// Soulseek client has uploaded to other peers (issue #325). Everything else
// about an upload is in-memory only and is erased by a restart.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// uploadHistoryRetention bounds how long upload_history rows are kept;
// PruneUploadHistory deletes anything older. Matches throughputRetention and
// jobEventRetention's fixed 30-day window. A busy share appends thousands of
// rows in a daemon that runs for months, so this table cannot be left to grow
// unbounded the way private_messages was (issue #186).
const uploadHistoryRetention = 30 * 24 * time.Hour

// uploadHistorySelect is shared by every read below so the column order can
// never drift from scanUploadHistory.
const uploadHistorySelect = `SELECT id, username, filename, size, bytes_sent, avg_bytes_per_sec, status, detail, started_at, finished_at FROM upload_history`

// RecordUpload appends one finished upload. It is called once an upload's
// outcome is decided, never from the streaming path, and every call inserts a
// new row: two transfers of the same file to the same peer are two facts, not
// an upsert.
//
// Detail is stored as written. The caller must pass a short fixed reason
// string rather than an error's text, since upload errors wrap local
// filesystem paths and this column is served over the API.
func (s *Store) RecordUpload(ctx context.Context, e core.UploadHistoryEntry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO upload_history (username, filename, size, bytes_sent, avg_bytes_per_sec, status, detail, started_at, finished_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.Username, e.Filename, int64(e.Size), int64(e.BytesSent), int64(e.AvgBytesPerSecond),
		string(e.Status), e.Detail, e.StartedAt, e.FinishedAt)
	if err != nil {
		return fmt.Errorf("record upload: %w", err)
	}
	return nil
}

// UploadHistory returns finished uploads newest-first, capped at limit.
// beforeID > 0 pages backwards from that row (keyset, not OFFSET, so an upload
// finishing mid-scroll cannot shift or duplicate a page).
//
// Ordered by id, not finished_at: id is assigned on insert and is therefore
// monotonic with write order, which is what keyset pagination requires.
// finished_at is not — two uploads can finish in the same instant, and a clock
// step would reorder them.
func (s *Store) UploadHistory(ctx context.Context, limit int, beforeID int64) ([]core.UploadHistoryEntry, error) {
	query := uploadHistorySelect
	var args []any
	if beforeID > 0 {
		query += ` WHERE id < $1`
		args = append(args, beforeID)
	}
	query += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query upload history: %w", err)
	}
	defer rows.Close()

	var out []core.UploadHistoryEntry
	for rows.Next() {
		var e core.UploadHistoryEntry
		var status string
		var detail *string
		var size, bytesSent, avgBytesPerSecond int64
		if err := rows.Scan(&e.ID, &e.Username, &e.Filename, &size, &bytesSent, &avgBytesPerSecond,
			&status, &detail, &e.StartedAt, &e.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan upload history: %w", err)
		}
		e.Size, e.BytesSent, e.AvgBytesPerSecond = uint64(size), uint64(bytesSent), uint64(avgBytesPerSecond)
		e.Status = core.UploadStatus(status)
		if detail != nil {
			e.Detail = *detail
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneUploadHistory deletes upload_history rows whose upload finished longer
// ago than uploadHistoryRetention.
func (s *Store) PruneUploadHistory(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM upload_history WHERE finished_at < $1`, now.Add(-uploadHistoryRetention))
	if err != nil {
		return fmt.Errorf("prune upload history: %w", err)
	}
	return nil
}
