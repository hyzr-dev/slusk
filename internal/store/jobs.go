package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
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
// transfer with no slskd id BEFORE slskd is called. It is idempotent within the
// owning candidate; another job attempting the same remote file must retain its
// own row rather than stealing this transfer's ownership.
func (s *Store) RecordEnqueueIntent(ctx context.Context, candidateID int64, username, filename string, deadline, now time.Time) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO transfers (candidate_id, username, filename, state, deadline, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT(candidate_id, username, filename) DO UPDATE SET
		   state = excluded.state,
		   deadline = excluded.deadline,
		   slskd_id = '',
		   bytes_done = 0,
		   updated_at = excluded.updated_at
		 RETURNING id`,
		candidateID, username, filename, string(core.TransferQueued), deadline, now).Scan(&id); err != nil {
		return 0, fmt.Errorf("upsert transfer intent: %w", err)
	}
	return id, nil
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

// AttachTransferID is step 2 of the write-ahead enqueue: it records the id slskd
// returned so future reconciliation can match on the strong key.
func (s *Store) AttachTransferID(ctx context.Context, transferID int64, slskdID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE transfers SET slskd_id = $1, updated_at = $2 WHERE id = $3`,
		slskdID, now, transferID)
	if err != nil {
		return fmt.Errorf("attach slskd id: %w", err)
	}
	return nil
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

// CancelJob atomically marks every live transfer under every candidate of the
// job and the job itself CANCELLED. It returns false if the job no longer exists.
func (s *Store) CancelJob(ctx context.Context, jobID int64, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("cancel job: begin: %w", err)
	}
	defer tx.Rollback()

	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM album_jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cancel job: select for update: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE transfers SET state = $1, updated_at = $2
		 WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id = $3)
		 AND state IN ($4, $5, $6, $7)`,
		string(core.TransferCancelled), now, jobID,
		string(core.TransferPending), string(core.TransferQueued),
		string(core.TransferInProgress), string(core.TransferStalled)); err != nil {
		return false, fmt.Errorf("cancel job: update transfers: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, updated_at = $2 WHERE id = $3`,
		string(core.StateCancelled), now, jobID); err != nil {
		return false, fmt.Errorf("cancel job: update job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("cancel job: commit: %w", err)
	}
	return true, nil
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

// ActiveTransfersForJob returns all lifecycle-live transfers under every
// candidate owned by a job. Unlike global reconciliation, manual lifecycle
// actions include PENDING transfers that have not yet been sent remotely.
func (s *Store) ActiveTransfersForJob(ctx context.Context, jobID int64) ([]core.Transfer, error) {
	rows, err := s.db.QueryContext(ctx,
		transferSelect+` WHERE candidate_id IN (
			SELECT id FROM candidates WHERE album_job_id = $1
		) AND state IN ($2, $3, $4, $5)
		ORDER BY id`,
		jobID, string(core.TransferPending), string(core.TransferQueued),
		string(core.TransferInProgress), string(core.TransferStalled))
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
			updated_at = $9 WHERE id = $10`,
		string(state), bytesDone, bytesTotal,
		bytesDone, now,
		string(state), string(core.TransferInProgress), now,
		now, transferID)
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
		`UPDATE transfers SET state = $1, retries = retries + 1, slskd_id = '', bytes_done = 0, last_progress_at = NULL, updated_at = $2 WHERE id = $3`,
		string(core.TransferPending), now, transferID)
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
