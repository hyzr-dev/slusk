package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

const jobSelect = `SELECT id, lidarr_album_id, state, candidates_tried, next_attempt_at, created_at, updated_at, title, artist_name, release_date, artist_id, retries, not_before, failed_at FROM album_jobs`

func scanJobs(rows *sql.Rows) ([]core.AlbumJob, error) {
	var out []core.AlbumJob
	for rows.Next() {
		var j core.AlbumJob
		var state string
		if err := rows.Scan(&j.ID, &j.LidarrAlbumID, &state, &j.CandidatesTried, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt, &j.Title, &j.ArtistName, &j.ReleaseDate, &j.ArtistID, &j.Retries, &j.NotBefore, &j.FailedAt); err != nil {
			return nil, err
		}
		j.State = core.AlbumJobState(state)
		out = append(out, j)
	}
	return out, rows.Err()
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
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = string(st)
	}
	query := fmt.Sprintf(`SELECT COUNT(*) FROM album_jobs WHERE state IN (%s)`, strings.Join(placeholders, ","))
	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count jobs in states: %w", err)
	}
	return count, nil
}

// CandidatesForJob returns all candidates ever cached for a job (any state),
// oldest first, for the dashboard's per-job detail panel. See dashboard.go's
// JobDetail.
func (s *Store) CandidatesForJob(ctx context.Context, jobID int64) ([]core.Candidate, error) {
	rows, err := s.db.QueryContext(ctx,
		candidateSelect+` WHERE album_job_id = $1 ORDER BY created_at`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RunnableJobsInState is JobsInState plus the not_before filter. Order:
// release_date DESC for WANTED (spec: newest releases first), updated_at ASC
// otherwise (fairness FIFO).
func (s *Store) RunnableJobsInState(ctx context.Context, state core.AlbumJobState, now time.Time, limit int) ([]core.AlbumJob, error) {
	order := "ORDER BY updated_at ASC"
	if state == core.StateWanted {
		order = "ORDER BY release_date DESC, updated_at ASC"
	}
	rows, err := s.db.QueryContext(ctx,
		jobSelect+` WHERE state = $1 AND (not_before IS NULL OR not_before <= $2) `+order+` LIMIT $3`,
		string(state), now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// SetJobBackoff bumps retries and hides the job until notBefore. State unchanged.
func (s *Store) SetJobBackoff(ctx context.Context, jobID int64, retries int, notBefore time.Time, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET retries = $1, not_before = $2, updated_at = $3 WHERE id = $4`,
		retries, notBefore, now, jobID)
	if err != nil {
		return fmt.Errorf("set job backoff: %w", err)
	}
	return nil
}

// MarkJobFailed: state→FAILED, failed_at=now, not_before cleared.
func (s *Store) MarkJobFailed(ctx context.Context, jobID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, failed_at = $2, not_before = NULL, updated_at = $3 WHERE id = $4`,
		string(core.StateFailed), now, now, jobID)
	if err != nil {
		return fmt.Errorf("mark job failed: %w", err)
	}
	return nil
}

// ResetJobToWanted deletes the job's candidates (and their transfers) and
// returns it to WANTED in one tx. retries/notBefore are written as given
// (exhaustion passes bumped values; TTL expiry and manual retry pass
// job.Retries-as-is / 0 and nil). Transfers must go with the candidates:
// transfers has a global UNIQUE(username, filename), so a leftover row from
// the wiped cycle would collide when a later cycle re-attempts the same
// peer+file.
func (s *Store) ResetJobToWanted(ctx context.Context, jobID int64, retries int, notBefore *time.Time, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM transfers WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id = $1)`, jobID); err != nil {
		return fmt.Errorf("delete candidate transfers: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM candidates WHERE album_job_id = $1`, jobID); err != nil {
		return fmt.Errorf("delete candidates: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, retries = $2, not_before = $3, updated_at = $4 WHERE id = $5`,
		string(core.StateWanted), retries, notBefore, now, jobID); err != nil {
		return fmt.Errorf("reset job to wanted: %w", err)
	}
	return tx.Commit()
}

// AdvanceJobStateFrom is the conditional transition every module uses:
// UPDATE ... SET state=$to WHERE id=$id AND state=$from. Returns whether a row
// changed — false means someone (WantedSync cancel) got there first; move on.
func (s *Store) AdvanceJobStateFrom(ctx context.Context, jobID int64, from, to core.AlbumJobState, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, updated_at = $2 WHERE id = $3 AND state = $4`,
		string(to), now, jobID, string(from))
	if err != nil {
		return false, fmt.Errorf("advance job state from: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("advance job state from: rows affected: %w", err)
	}
	return n > 0, nil
}

// CancelJobsNotWanted cancels every non-pipeline-terminal job whose album is
// absent from wantedIDs. Returns count.
func (s *Store) CancelJobsNotWanted(ctx context.Context, wantedIDs []int64, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, updated_at = $2
		 WHERE state NOT IN ($3, $4, $5) AND lidarr_album_id <> ALL($6)`,
		string(core.StateCancelled), now,
		string(core.StateDone), string(core.StateCancelled), string(core.StateFailed),
		wantedIDs)
	if err != nil {
		return 0, fmt.Errorf("cancel jobs not wanted: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cancel jobs not wanted: rows affected: %w", err)
	}
	return int(n), nil
}

// ReviveFailedJobs returns FAILED jobs with failed_at < cutoff AND album still
// in wantedIDs to WANTED with retries=0, not_before=NULL, failed_at=NULL.
func (s *Store) ReviveFailedJobs(ctx context.Context, wantedIDs []int64, cutoff time.Time, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, retries = 0, not_before = NULL, failed_at = NULL, updated_at = $2
		 WHERE state = $3 AND failed_at < $4 AND lidarr_album_id = ANY($5)`,
		string(core.StateWanted), now, string(core.StateFailed), cutoff, wantedIDs)
	if err != nil {
		return 0, fmt.Errorf("revive failed jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("revive failed jobs: rows affected: %w", err)
	}
	return int(n), nil
}

// UpsertWantedJob is UpsertDiscoveredJob but inserting state WANTED.
func (s *Store) UpsertWantedJob(ctx context.Context, lidarrAlbumID int64, now time.Time) (core.AlbumJob, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO album_jobs (lidarr_album_id, state, created_at, updated_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT(lidarr_album_id) DO NOTHING`,
		lidarrAlbumID, string(core.StateWanted), now, now)
	if err != nil {
		return core.AlbumJob{}, fmt.Errorf("insert job: %w", err)
	}
	var j core.AlbumJob
	var state string
	err = s.db.QueryRowContext(ctx,
		`SELECT id, lidarr_album_id, state, candidates_tried, next_attempt_at, created_at, updated_at, artist_id, retries, not_before, failed_at
		 FROM album_jobs WHERE lidarr_album_id = $1`, lidarrAlbumID).
		Scan(&j.ID, &j.LidarrAlbumID, &state, &j.CandidatesTried, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt, &j.ArtistID, &j.Retries, &j.NotBefore, &j.FailedAt)
	if err != nil {
		return core.AlbumJob{}, fmt.Errorf("read job: %w", err)
	}
	j.State = core.AlbumJobState(state)
	return j, nil
}
