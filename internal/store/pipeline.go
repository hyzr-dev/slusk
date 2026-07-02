package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

const jobSelect = `SELECT id, lidarr_album_id, state, candidates_tried, next_attempt_at, created_at, updated_at, title, artist_name FROM album_jobs`

func scanJobs(rows *sql.Rows) ([]core.AlbumJob, error) {
	var out []core.AlbumJob
	for rows.Next() {
		var j core.AlbumJob
		var state string
		if err := rows.Scan(&j.ID, &j.LidarrAlbumID, &state, &j.CandidatesTried, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt, &j.Title, &j.ArtistName); err != nil {
			return nil, err
		}
		j.State = core.AlbumJobState(state)
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobsInState returns up to limit jobs currently in the given state.
func (s *Store) JobsInState(ctx context.Context, state core.AlbumJobState, limit int) ([]core.AlbumJob, error) {
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE state = ? ORDER BY updated_at LIMIT ?`, string(state), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// CountJobsInStates returns the total number of jobs currently in any of the
// given states, used to enforce the global cap on concurrently active jobs.
func (s *Store) CountJobsInStates(ctx context.Context, states ...core.AlbumJobState) (int, error) {
	if len(states) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(states))
	args := make([]any, len(states))
	for i, st := range states {
		placeholders[i] = "?"
		args[i] = string(st)
	}
	query := fmt.Sprintf(`SELECT COUNT(*) FROM album_jobs WHERE state IN (%s)`, strings.Join(placeholders, ","))
	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count jobs in states: %w", err)
	}
	return count, nil
}

// DueCooldownJobs returns up to limit COOLDOWN jobs whose next_attempt_at has passed.
func (s *Store) DueCooldownJobs(ctx context.Context, now time.Time, limit int) ([]core.AlbumJob, error) {
	rows, err := s.db.QueryContext(ctx,
		jobSelect+` WHERE state = ? AND next_attempt_at IS NOT NULL AND next_attempt_at <= ? ORDER BY next_attempt_at LIMIT ?`,
		string(core.StateCooldown), now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// AttemptsForJob returns all candidate attempts for a job, oldest first.
func (s *Store) AttemptsForJob(ctx context.Context, jobID int64) ([]core.CandidateAttempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, album_job_id, username, score, state, fail_reason, backoff_until, created_at
		 FROM candidate_attempts WHERE album_job_id = ? ORDER BY created_at`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.CandidateAttempt
	for rows.Next() {
		var a core.CandidateAttempt
		if err := rows.Scan(&a.ID, &a.AlbumJobID, &a.Username, &a.Score, &a.State, &a.FailReason, &a.BackoffUntil, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TransfersForAttempt returns all transfers belonging to a candidate attempt.
func (s *Store) TransfersForAttempt(ctx context.Context, attemptID int64) ([]core.Transfer, error) {
	rows, err := s.db.QueryContext(ctx, transferSelect+` WHERE attempt_id = ?`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransfers(rows)
}

// FailAttempt marks a candidate attempt FAILED with a reason and a backoff time.
func (s *Store) FailAttempt(ctx context.Context, attemptID int64, reason string, backoffUntil, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE candidate_attempts SET state = 'FAILED', fail_reason = ?, backoff_until = ? WHERE id = ?`,
		reason, backoffUntil, attemptID)
	if err != nil {
		return fmt.Errorf("fail attempt: %w", err)
	}
	return nil
}

// SucceedAttempt marks a candidate attempt SUCCEEDED.
func (s *Store) SucceedAttempt(ctx context.Context, attemptID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE candidate_attempts SET state = 'SUCCEEDED' WHERE id = ?`, attemptID)
	if err != nil {
		return fmt.Errorf("succeed attempt: %w", err)
	}
	return nil
}

// SetJobCooldown moves a job to COOLDOWN with the given next-attempt time.
func (s *Store) SetJobCooldown(ctx context.Context, jobID int64, nextAttemptAt, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET state = ?, next_attempt_at = ?, updated_at = ? WHERE id = ?`,
		string(core.StateCooldown), nextAttemptAt, now, jobID)
	if err != nil {
		return fmt.Errorf("set job cooldown: %w", err)
	}
	return nil
}

// IncrementCandidatesTried bumps the count of candidates tried for a job.
func (s *Store) IncrementCandidatesTried(ctx context.Context, jobID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET candidates_tried = candidates_tried + 1, updated_at = ? WHERE id = ?`,
		now, jobID)
	if err != nil {
		return fmt.Errorf("increment candidates tried: %w", err)
	}
	return nil
}

// DueFailedJobs returns up to limit FAILED jobs whose updated_at is at or before
// cutoff — used to retry failed albums after failed_retry_after has elapsed.
func (s *Store) DueFailedJobs(ctx context.Context, cutoff time.Time, limit int) ([]core.AlbumJob, error) {
	rows, err := s.db.QueryContext(ctx,
		jobSelect+` WHERE state = ? AND updated_at <= ? ORDER BY updated_at LIMIT ?`,
		string(core.StateFailed), cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// ResetJobForRetry gives a failed album a fresh start: it deletes the job's prior
// attempts (and their now-terminal transfers) in a transaction and returns the job
// to DISCOVERED with a zero candidate count. Clearing attempts frees the
// (username, filename) transfer keys and lets previously-tried peers be retried.
func (s *Store) ResetJobForRetry(ctx context.Context, jobID int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM transfers WHERE attempt_id IN (SELECT id FROM candidate_attempts WHERE album_job_id = ?)`, jobID); err != nil {
		return fmt.Errorf("delete transfers: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM candidate_attempts WHERE album_job_id = ?`, jobID); err != nil {
		return fmt.Errorf("delete attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = ?, candidates_tried = 0, next_attempt_at = NULL, updated_at = ? WHERE id = ?`,
		string(core.StateDiscovered), now, jobID); err != nil {
		return fmt.Errorf("reset job: %w", err)
	}
	return tx.Commit()
}
