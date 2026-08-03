package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// UpdateJobMetadata refreshes the cached title/artist_name/release_date/artist_id
// for a job. It is called every discovery pass so display metadata and
// release-date ordering stay current even if Lidarr renames an album/artist or
// corrects a release date after the job was first discovered.
func (s *Store) UpdateJobMetadata(ctx context.Context, jobID int64, title, artistName, releaseDate string, artistID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET title = $1, artist_name = $2, release_date = $3, artist_id = $4, updated_at = $5 WHERE id = $6`,
		title, artistName, releaseDate, artistID, now, jobID)
	if err != nil {
		return fmt.Errorf("update job metadata: %w", err)
	}
	return nil
}

// BackfillJobMetadataIfEmpty sets title/artist_name/release_date/artist_id only
// if any of them is currently empty (e.g. a job created before metadata caching
// existed, or before this job's first DISCOVERED pass). Unlike
// UpdateJobMetadata, it does not touch updated_at, since that column drives
// retry-cooldown timing for jobs already past DISCOVERED (see
// Discoverer.syncWanted).
func (s *Store) BackfillJobMetadataIfEmpty(ctx context.Context, jobID int64, title, artistName, releaseDate string, artistID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET title = $1, artist_name = $2, release_date = $3, artist_id = $4
		 WHERE id = $5 AND (title = '' OR artist_name = '' OR release_date = '' OR artist_id = 0)`,
		title, artistName, releaseDate, artistID, jobID)
	if err != nil {
		return fmt.Errorf("backfill job metadata: %w", err)
	}
	return nil
}

// RecordEnqueueIntent is step 1 of the write-ahead enqueue: it persists a QUEUED
// transfer with no remote id BEFORE the peer is called. It locks and re-checks
// the owning job so cancellation cannot commit between this local intent and
// the point at which enqueue is allowed to begin. ok is false when the job has
// crossed its cancellation barrier or the transfer has already become terminal.
func (s *Store) RecordEnqueueIntent(ctx context.Context, candidateID int64, username, filename string, deadline, now time.Time) (id int64, ok bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("upsert transfer intent: begin: %w", err)
	}
	defer tx.Rollback()

	var jobState string
	err = tx.QueryRowContext(ctx,
		`SELECT j.state FROM album_jobs j
		 JOIN candidates c ON c.album_job_id = j.id
		 WHERE c.id = $1 FOR UPDATE OF j`, candidateID).Scan(&jobState)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && core.AlbumJobState(jobState) == core.StateCancelled) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("upsert transfer intent: lock job: %w", err)
	}

	err = tx.QueryRowContext(ctx,
		`INSERT INTO transfers (candidate_id, username, filename, state, deadline, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT(candidate_id, username, filename) DO UPDATE SET
		   state = excluded.state,
		   deadline = excluded.deadline,
		   slskd_id = '',
		   bytes_done = 0,
		   updated_at = excluded.updated_at
		 WHERE transfers.state IN ($7, $8, $9, $10)
		 RETURNING id`,
		candidateID, username, filename, string(core.TransferQueued), deadline, now,
		string(core.TransferPending), string(core.TransferQueued),
		string(core.TransferInProgress), string(core.TransferStalled)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("upsert transfer intent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("upsert transfer intent: commit: %w", err)
	}
	return id, true, nil
}

// RecordPendingTransfer persists a file the pipeline intends to download but
// has not yet handed to slskd, as a PENDING transfer carrying its size in
// bytes_total. The pipeline promotes a bounded number of these to QUEUED per
// peer at a time (via RecordEnqueueIntent) so a burst never trips a peer's
// per-user queued-megabyte limit. Idempotent on the candidate-owned
// (candidate_id, username, filename) key, mirroring RecordEnqueueIntent. The
// deadline is a placeholder; the real one is set when the file is actually
// sent, since PENDING transfers are never
// deadline-reaped.
func (s *Store) RecordPendingTransfer(ctx context.Context, candidateID int64, username, filename string, size int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO transfers (candidate_id, username, filename, state, bytes_total, deadline, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT(candidate_id, username, filename) DO UPDATE SET
		   state = excluded.state,
		   bytes_total = excluded.bytes_total,
		   slskd_id = '',
		   bytes_done = 0,
		   deadline = excluded.deadline,
		   updated_at = excluded.updated_at`,
		candidateID, username, filename, string(core.TransferPending), size, now, now)
	if err != nil {
		return fmt.Errorf("record pending transfer: %w", err)
	}
	return nil
}

// AttachTransferID is step 2 of the write-ahead enqueue. It serializes with
// the owning job's cancellation barrier; ok is false when cancellation or
// deletion won while the remote enqueue was in flight, so the caller can
// immediately compensate by cancelling the returned remote id.
func (s *Store) AttachTransferID(ctx context.Context, transferID int64, remoteID string, now time.Time) (ok bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("attach remote id: begin: %w", err)
	}
	defer tx.Rollback()

	var jobState string
	err = tx.QueryRowContext(ctx,
		`SELECT j.state FROM album_jobs j
		 JOIN candidates c ON c.album_job_id = j.id
		 JOIN transfers t ON t.candidate_id = c.id
		 WHERE t.id = $1 FOR UPDATE OF j`, transferID).Scan(&jobState)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && core.AlbumJobState(jobState) == core.StateCancelled) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("attach remote id: lock job: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE transfers SET slskd_id = $1, updated_at = $2
		 WHERE id = $3 AND state IN ($4, $5, $6, $7)`,
		remoteID, now, transferID,
		string(core.TransferPending), string(core.TransferQueued),
		string(core.TransferInProgress), string(core.TransferStalled))
	if err != nil {
		return false, fmt.Errorf("attach remote id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("attach remote id: rows affected: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("attach remote id: commit: %w", err)
	}
	return true, nil
}

// FindTransferByFallback recovers a candidate-owned transfer by its remote
// (username, filename) key, used after a crash between RecordEnqueueIntent and
// AttachTransferID.
func (s *Store) FindTransferByFallback(ctx context.Context, candidateID int64, username, filename string) (core.Transfer, bool, error) {
	tr, err := scanTransfer(s.db.QueryRowContext(ctx,
		transferSelect+` WHERE candidate_id = $1 AND username = $2 AND filename = $3
		 AND state IN ($4, $5, $6, $7)`,
		candidateID, username, filename, string(core.TransferPending), string(core.TransferQueued),
		string(core.TransferInProgress), string(core.TransferStalled)))
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
		`UPDATE album_jobs SET state = $1, updated_at = $2 WHERE id = $3`,
		string(to), now, jobID)
	if err != nil {
		return fmt.Errorf("advance job: %w", err)
	}
	return nil
}

// CancelJob establishes the local cancellation barrier and returns the
// transfers that were live immediately before it. found is false if the job no
// longer exists. The returned identities are cancelled remotely only after
// this transaction commits.
func (s *Store) CancelJob(ctx context.Context, jobID int64, now time.Time) (transfers []core.Transfer, found bool, err error) {
	return s.prepareJobCancellation(ctx, jobID, now, false)
}

// PrepareDeleteJob establishes the same local cancellation barrier as
// CancelJob, but refuses an IMPORTING job. A later hard-delete failure safely
// leaves the already-prepared job CANCELLED.
func (s *Store) PrepareDeleteJob(ctx context.Context, jobID int64, now time.Time) (transfers []core.Transfer, found bool, err error) {
	return s.prepareJobCancellation(ctx, jobID, now, true)
}

func (s *Store) prepareJobCancellation(ctx context.Context, jobID int64, now time.Time, rejectImporting bool) ([]core.Transfer, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("prepare job cancellation: begin: %w", err)
	}
	defer tx.Rollback()

	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM album_jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("prepare job cancellation: select for update: %w", err)
	}
	if rejectImporting && core.AlbumJobState(state) == core.StateImporting {
		return nil, false, ErrJobImporting
	}

	rows, err := tx.QueryContext(ctx,
		transferSelect+` WHERE candidate_id IN (
			SELECT id FROM candidates WHERE album_job_id = $1
		) AND state IN ($2, $3, $4, $5) ORDER BY id FOR UPDATE`,
		jobID, string(core.TransferPending), string(core.TransferQueued),
		string(core.TransferInProgress), string(core.TransferStalled))
	if err != nil {
		return nil, false, fmt.Errorf("prepare job cancellation: select transfers: %w", err)
	}
	transfers, err := scanTransfers(rows)
	rows.Close()
	if err != nil {
		return nil, false, fmt.Errorf("prepare job cancellation: scan transfers: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE transfers SET state = $1, updated_at = $2
		 WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id = $3)
		 AND state IN ($4, $5, $6, $7)`,
		string(core.TransferCancelled), now, jobID,
		string(core.TransferPending), string(core.TransferQueued),
		string(core.TransferInProgress), string(core.TransferStalled)); err != nil {
		return nil, false, fmt.Errorf("prepare job cancellation: update transfers: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, updated_at = $2 WHERE id = $3`,
		string(core.StateCancelled), now, jobID); err != nil {
		return nil, false, fmt.Errorf("prepare job cancellation: update job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("prepare job cancellation: commit: %w", err)
	}
	return transfers, true, nil
}

// TransfersPastDeadline returns non-terminal transfers whose deadline has passed.
func (s *Store) TransfersPastDeadline(ctx context.Context, now time.Time) ([]core.Transfer, error) {
	rows, err := s.db.QueryContext(ctx,
		transferSelect+` WHERE deadline < $1 AND state IN ($2, $3, $4)`,
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
		transferSelect+` WHERE state IN ($1, $2, $3)`,
		string(core.TransferQueued), string(core.TransferInProgress), string(core.TransferStalled))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransfers(rows)
}

// UpdateTransferProgress records new state and byte counts for a transfer.
// last_progress_at only advances when the byte counter actually increased: the
// reconciler calls this every poll pass, so stamping it unconditionally would
// make a stalled transfer (reconciled repeatedly with unchanged bytes) always
// look freshly progressing and defeat stall detection. It is also stamped on
// the QUEUED→IN_PROGRESS transition (when last_progress_at is still NULL) so the
// stall clock starts when the download actually begins rather than at enqueue.
func (s *Store) UpdateTransferProgress(ctx context.Context, transferID int64, state core.TransferState, bytesDone, bytesTotal int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE transfers SET state = $1, bytes_done = $2, bytes_total = $3,
			last_progress_at = CASE
				WHEN $4 > bytes_done THEN $5
				WHEN $6::text = $7::text AND last_progress_at IS NULL THEN $8
				ELSE last_progress_at END,
			updated_at = $9
		 WHERE id = $10
		 AND state IN ($11, $12, $13, $14)
		 AND EXISTS (
			SELECT 1 FROM candidates c JOIN album_jobs j ON j.id = c.album_job_id
			WHERE c.id = transfers.candidate_id AND j.state != $15
		)`,
		string(state), bytesDone, bytesTotal,
		bytesDone, now,
		string(state), string(core.TransferInProgress), now,
		now, transferID,
		string(core.TransferPending), string(core.TransferQueued),
		string(core.TransferInProgress), string(core.TransferStalled),
		string(core.StateCancelled))
	if err != nil {
		return fmt.Errorf("update transfer progress: %w", err)
	}
	return nil
}

// RetryTransfer returns a transfer to PENDING for a later resend and bumps its
// retry count, clearing the slskd id, byte progress, and stall clock. Used when
// a peer rejected a download for a transient reason (e.g. its queued-megabyte
// limit): the file waits in the pending pool and topUpAttempt sends it again
// once the peer's queue has drained, rather than failing the whole attempt.
// last_progress_at must be cleared here: a stall-retried transfer would
// otherwise carry its already-expired stall clock into the re-attempt and be
// re-cancelled on its first IN_PROGRESS poll, burning the retry budget without
// a genuine retry. UpdateTransferProgress stamps a fresh clock when the
// re-sent transfer actually starts.
func (s *Store) RetryTransfer(ctx context.Context, transferID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE transfers SET state = $1, retries = retries + 1, slskd_id = '', bytes_done = 0, last_progress_at = NULL, updated_at = $2
		 WHERE id = $3
		 AND state IN ($4, $5, $6, $7)
		 AND EXISTS (
			SELECT 1 FROM candidates c JOIN album_jobs j ON j.id = c.album_job_id
			WHERE c.id = transfers.candidate_id AND j.state != $8
		)`,
		string(core.TransferPending), now, transferID,
		string(core.TransferPending), string(core.TransferQueued),
		string(core.TransferInProgress), string(core.TransferStalled),
		string(core.StateCancelled))
	if err != nil {
		return fmt.Errorf("retry transfer: %w", err)
	}
	return nil
}

const transferSelect = `SELECT id, candidate_id, slskd_id, username, filename, state,
	bytes_done, bytes_total, retries, deadline, last_progress_at, updated_at FROM transfers`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTransfer(r rowScanner) (core.Transfer, error) {
	var t core.Transfer
	var state string
	err := r.Scan(&t.ID, &t.CandidateID, &t.SlskdID, &t.Username, &t.Filename, &state,
		&t.BytesDone, &t.BytesTotal, &t.Retries, &t.Deadline, &t.LastProgressAt, &t.UpdatedAt)
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
