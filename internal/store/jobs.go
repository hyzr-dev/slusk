package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// UpsertDiscoveredJob inserts a DISCOVERED job for the Lidarr album, or returns
// the existing job if one already exists (idempotent on lidarr_album_id).
func (s *Store) UpsertDiscoveredJob(ctx context.Context, lidarrAlbumID int64, now time.Time) (core.AlbumJob, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO album_jobs (lidarr_album_id, state, created_at, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(lidarr_album_id) DO NOTHING`,
		lidarrAlbumID, string(core.StateDiscovered), now, now)
	if err != nil {
		return core.AlbumJob{}, fmt.Errorf("insert job: %w", err)
	}
	var j core.AlbumJob
	var state string
	err = s.db.QueryRowContext(ctx,
		`SELECT id, lidarr_album_id, state, candidates_tried, next_attempt_at, created_at, updated_at
		 FROM album_jobs WHERE lidarr_album_id = ?`, lidarrAlbumID).
		Scan(&j.ID, &j.LidarrAlbumID, &state, &j.CandidatesTried, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return core.AlbumJob{}, fmt.Errorf("read job: %w", err)
	}
	j.State = core.AlbumJobState(state)
	return j, nil
}

// UpdateJobMetadata refreshes the cached title/artist_name for a job. It is
// called every discovery pass so display metadata stays current even if
// Lidarr renames an album/artist after the job was first discovered.
func (s *Store) UpdateJobMetadata(ctx context.Context, jobID int64, title, artistName string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET title = ?, artist_name = ?, updated_at = ? WHERE id = ?`,
		title, artistName, now, jobID)
	if err != nil {
		return fmt.Errorf("update job metadata: %w", err)
	}
	return nil
}

// BackfillJobMetadataIfEmpty sets title/artist_name only if either is
// currently empty (e.g. a job created before metadata caching existed, or
// before this job's first DISCOVERED pass). Unlike UpdateJobMetadata, it does
// not touch updated_at, since that column drives retry-cooldown timing for
// jobs already past DISCOVERED (see Discoverer.syncWanted).
func (s *Store) BackfillJobMetadataIfEmpty(ctx context.Context, jobID int64, title, artistName string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET title = ?, artist_name = ? WHERE id = ? AND (title = '' OR artist_name = '')`,
		title, artistName, jobID)
	if err != nil {
		return fmt.Errorf("backfill job metadata: %w", err)
	}
	return nil
}

// CreateAttempt inserts a PENDING candidate attempt and returns its ID.
func (s *Store) CreateAttempt(ctx context.Context, albumJobID int64, username string, score float64, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO candidate_attempts (album_job_id, username, score, state, created_at)
		 VALUES (?, ?, ?, 'PENDING', ?)`,
		albumJobID, username, score, now)
	if err != nil {
		return 0, fmt.Errorf("insert attempt: %w", err)
	}
	return res.LastInsertId()
}

// RecordEnqueueIntent is step 1 of the write-ahead enqueue: it persists a QUEUED
// transfer with no slskd id BEFORE slskd is called. It is idempotent on the
// (username, filename) key: a re-enqueue updates the existing row's attempt and
// deadline and returns that row, rather than violating the UNIQUE constraint.
func (s *Store) RecordEnqueueIntent(ctx context.Context, attemptID int64, username, filename string, deadline, now time.Time) (int64, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO transfers (attempt_id, username, filename, state, deadline, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(username, filename) DO UPDATE SET
		   attempt_id = excluded.attempt_id,
		   state = excluded.state,
		   deadline = excluded.deadline,
		   slskd_id = '',
		   bytes_done = 0,
		   updated_at = excluded.updated_at`,
		attemptID, username, filename, string(core.TransferQueued), deadline, now)
	if err != nil {
		return 0, fmt.Errorf("upsert transfer intent: %w", err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM transfers WHERE username = ? AND filename = ?`, username, filename).Scan(&id); err != nil {
		return 0, fmt.Errorf("read transfer id: %w", err)
	}
	return id, nil
}

// AttachTransferID is step 2 of the write-ahead enqueue: it records the id slskd
// returned so future reconciliation can match on the strong key.
func (s *Store) AttachTransferID(ctx context.Context, transferID int64, slskdID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE transfers SET slskd_id = ?, updated_at = ? WHERE id = ?`,
		slskdID, now, transferID)
	if err != nil {
		return fmt.Errorf("attach slskd id: %w", err)
	}
	return nil
}

// FindTransferByFallback recovers a transfer by its (username, filename) key,
// used after a crash between RecordEnqueueIntent and AttachTransferID.
func (s *Store) FindTransferByFallback(ctx context.Context, username, filename string) (core.Transfer, bool, error) {
	tr, err := scanTransfer(s.db.QueryRowContext(ctx,
		transferSelect+` WHERE username = ? AND filename = ?`, username, filename))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Transfer{}, false, nil
	}
	if err != nil {
		return core.Transfer{}, false, err
	}
	return tr, true, nil
}

// AdvanceJobState transitions a job to the target state.
func (s *Store) AdvanceJobState(ctx context.Context, jobID int64, to core.AlbumJobState, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET state = ?, updated_at = ? WHERE id = ?`,
		string(to), now, jobID)
	if err != nil {
		return fmt.Errorf("advance job: %w", err)
	}
	return nil
}

// TransfersPastDeadline returns non-terminal transfers whose deadline has passed.
func (s *Store) TransfersPastDeadline(ctx context.Context, now time.Time) ([]core.Transfer, error) {
	rows, err := s.db.QueryContext(ctx,
		transferSelect+` WHERE deadline < ? AND state IN (?, ?, ?)`,
		now, string(core.TransferQueued), string(core.TransferInProgress), string(core.TransferStalled))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransfers(rows)
}

// ActiveTransfers returns every transfer not in a terminal state.
func (s *Store) ActiveTransfers(ctx context.Context) ([]core.Transfer, error) {
	rows, err := s.db.QueryContext(ctx,
		transferSelect+` WHERE state IN (?, ?, ?)`,
		string(core.TransferQueued), string(core.TransferInProgress), string(core.TransferStalled))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransfers(rows)
}

// UpdateTransferProgress records new state and byte counts for a transfer.
func (s *Store) UpdateTransferProgress(ctx context.Context, transferID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE transfers SET state = ?, bytes_done = ?, bytes_total = ?, last_progress_at = ?, updated_at = ? WHERE id = ?`,
		string(state), bytesDone, bytesTotal, now, now, transferID)
	if err != nil {
		return fmt.Errorf("update transfer progress: %w", err)
	}
	return nil
}

const transferSelect = `SELECT id, attempt_id, slskd_id, username, filename, state,
	bytes_done, bytes_total, deadline, last_progress_at, updated_at FROM transfers`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTransfer(r rowScanner) (core.Transfer, error) {
	var t core.Transfer
	var state string
	err := r.Scan(&t.ID, &t.AttemptID, &t.SlskdID, &t.Username, &t.Filename, &state,
		&t.BytesDone, &t.BytesTotal, &t.Deadline, &t.LastProgressAt, &t.UpdatedAt)
	t.State = core.TransferState(state)
	return t, err
}

func scanTransfers(rows *sql.Rows) ([]core.Transfer, error) {
	var out []core.Transfer
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
