package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

const jobSelect = `SELECT id, COALESCE(lidarr_album_id, 0), state, candidates_tried, next_attempt_at, created_at, updated_at, title, artist_name, release_date, artist_id, retries, not_before, failed_at, min_track_count, max_track_count, source FROM album_jobs`

func scanJobs(rows *sql.Rows) ([]core.AlbumJob, error) {
	var out []core.AlbumJob
	for rows.Next() {
		var j core.AlbumJob
		var state, source string
		if err := rows.Scan(&j.ID, &j.LidarrAlbumID, &state, &j.CandidatesTried, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt, &j.Title, &j.ArtistName, &j.ReleaseDate, &j.ArtistID, &j.Retries, &j.NotBefore, &j.FailedAt, &j.MinTrackCount, &j.MaxTrackCount, &source); err != nil {
			return nil, err
		}
		j.State = core.AlbumJobState(state)
		j.Source = core.JobSource(source)
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

// SetJobNotBefore hides the job from RunnableJobsInState until notBefore
// without touching retries or updated_at. Unlike SetJobBackoff this is not a
// backoff-cycle write: the importing verify phase uses it to cool down retries
// against a slow-scanning folder, and bumping updated_at here would reset
// escalateIfStuck's StuckAfter clock so the job never escalates.
func (s *Store) SetJobNotBefore(ctx context.Context, jobID int64, notBefore time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET not_before = $1 WHERE id = $2`,
		notBefore, jobID)
	if err != nil {
		return fmt.Errorf("set job not_before: %w", err)
	}
	return nil
}

// MarkJobFailed: state→FAILED, failed_at=now, not_before cleared. The UPDATE is
// guarded (state NOT IN the terminal states) so a job WantedSync cancelled
// underneath a failing search cycle is never resurrected CANCELLED→FAILED (or a
// DONE/FAILED job re-failed); per the single-writer invariant every transition
// UPDATE must be conditional. A guarded-out no-op returns nil (nothing to fail).
func (s *Store) MarkJobFailed(ctx context.Context, jobID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, failed_at = $2, not_before = NULL, updated_at = $3
		 WHERE id = $4 AND state NOT IN ($5, $6, $7)`,
		string(core.StateFailed), now, now, jobID,
		string(core.StateCancelled), string(core.StateDone), string(core.StateFailed))
	if err != nil {
		return fmt.Errorf("mark job failed: %w", err)
	}
	return nil
}

// ResetJobToWanted deletes the job's candidates (and their transfers) and
// returns it to WANTED in one tx. retries/notBefore are written as given
// (exhaustion passes bumped values; TTL expiry and manual retry pass
// job.Retries-as-is / 0 and nil). Transfers must go with the candidates to
// satisfy transfer ownership/FK integrity and leave no stale cycle data.
// The job UPDATE is guarded on the caller-supplied `from` state (per the
// single-writer invariant every transition UPDATE must be conditional): every
// caller resets a SELECTING job, so a job WantedSync cancelled underneath us
// must NOT be resurrected to WANTED nor have its candidates/transfers deleted.
// When the guard bounces (0 rows) the whole tx rolls back and nil is returned.
func (s *Store) ResetJobToWanted(ctx context.Context, jobID int64, from core.AlbumJobState, retries int, notBefore *time.Time, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The guarded transition runs first: if it bounces, the deferred rollback
	// leaves the candidates/transfers intact for the job that turned CANCELLED.
	res, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, retries = $2, not_before = $3, updated_at = $4 WHERE id = $5 AND state = $6`,
		string(core.StateWanted), retries, notBefore, now, jobID, string(from))
	if err != nil {
		return fmt.Errorf("reset job to wanted: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reset job to wanted: rows affected: %w", err)
	}
	if n == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM transfers WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id = $1)`, jobID); err != nil {
		return fmt.Errorf("delete candidate transfers: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM candidates WHERE album_job_id = $1`, jobID); err != nil {
		return fmt.Errorf("delete candidates: %w", err)
	}
	return tx.Commit()
}

// RetryFailedJob manually revives one FAILED or ORPHANED job: retries 0,
// not_before/failed_at cleared, candidates deleted, state WANTED. Returns
// false when the job is not FAILED/ORPHANED (the dashboard button raced a
// state change) or does not exist. Candidates/transfers must go with the
// reset for the same ownership/FK and clean-slate reason as ResetJobToWanted.
func (s *Store) RetryFailedJob(ctx context.Context, jobID int64, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, retries = 0, not_before = NULL, failed_at = NULL, updated_at = $2
		 WHERE id = $3 AND state IN ($4, $5)`,
		string(core.StateWanted), now, jobID, string(core.StateFailed), string(core.StateOrphaned))
	if err != nil {
		return false, fmt.Errorf("retry failed job: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("retry failed job: rows affected: %w", err)
	}
	if n == 0 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM transfers WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id = $1)`, jobID); err != nil {
		return false, fmt.Errorf("retry failed job: delete candidate transfers: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM candidates WHERE album_job_id = $1`, jobID); err != nil {
		return false, fmt.Errorf("retry failed job: delete candidates: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("retry failed job: commit: %w", err)
	}
	return true, nil
}

// OrphanJobForCandidate marks a candidate's owning job ORPHANED (issue #158),
// but only if it is still DOWNLOADING - guarding against a race with another
// transition (e.g. WantedSync cancelling it, or resolve advancing it on a
// concurrent tick) - so the UPDATE never clobbers a job that already moved on.
// Returns whether a row changed.
func (s *Store) OrphanJobForCandidate(ctx context.Context, candidateID int64, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, updated_at = $2
		 WHERE id = (SELECT album_job_id FROM candidates WHERE id = $3) AND state = $4`,
		string(core.StateOrphaned), now, candidateID, string(core.StateDownloading))
	if err != nil {
		return false, fmt.Errorf("orphan job for candidate: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("orphan job for candidate: rows affected: %w", err)
	}
	return n > 0, nil
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

// CancelJobsNotWanted cancels every non-pipeline-terminal Lidarr job whose
// album is absent from wantedIDs. Returns count. The source = 'lidarr'
// predicate is explicit so an empty wantedIDs never cancels manual jobs (whose
// lidarr_album_id is NULL, making `<> ALL($6)` true for any array).
func (s *Store) CancelJobsNotWanted(ctx context.Context, wantedIDs []int64, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, updated_at = $2
		 WHERE state NOT IN ($3, $4, $5) AND source = 'lidarr' AND lidarr_album_id <> ALL($6)`,
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

// SyncWantedJobs atomically reconciles one complete, successfully fetched
// wanted snapshot. Duplicate Lidarr album IDs are collapsed before any SQL is
// run, with the last occurrence providing the metadata. Every database
// operation is set-based and receives fixed-count PostgreSQL arrays, so the
// number of statements does not grow with the snapshot.
//
// An empty snapshot is deliberately non-authoritative: it inserts, cancels,
// and revives nothing. This protects in-flight work from a transient empty
// Lidarr response.
func (s *Store) SyncWantedJobs(ctx context.Context, releases []core.WantedRelease, failedCutoff, now time.Time) (cancelled, revived int, err error) {
	unique := make(map[int64]core.WantedRelease, len(releases))
	order := make([]int64, 0, len(releases))
	for _, release := range releases {
		if _, exists := unique[release.ID]; !exists {
			order = append(order, release.ID)
		}
		unique[release.ID] = release
	}
	if len(unique) == 0 {
		return 0, 0, nil
	}

	ids := make([]int64, 0, len(unique))
	titles := make([]string, 0, len(unique))
	artists := make([]string, 0, len(unique))
	releaseDates := make([]string, 0, len(unique))
	artistIDs := make([]int64, 0, len(unique))
	for _, id := range order {
		release := unique[id]
		ids = append(ids, release.ID)
		titles = append(titles, release.Title)
		artists = append(artists, release.ArtistName)
		releaseDates = append(releaseDates, release.ReleaseDate)
		artistIDs = append(artistIDs, release.ArtistID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("sync wanted jobs: begin: %w", err)
	}
	defer tx.Rollback()

	const wantedInput = `SELECT * FROM unnest($1::bigint[], $2::text[], $3::text[], $4::text[], $5::bigint[])
		AS wanted(lidarr_album_id, title, artist_name, release_date, artist_id)`
	inputArgs := []any{ids, titles, artists, releaseDates, artistIDs}

	if _, err := tx.ExecContext(ctx, `WITH wanted AS (`+wantedInput+`)
		INSERT INTO album_jobs (lidarr_album_id, state, created_at, updated_at, title, artist_name, release_date, artist_id)
		SELECT lidarr_album_id, $6, $7, $7, title, artist_name, release_date, artist_id FROM wanted
		ON CONFLICT (lidarr_album_id) WHERE source = 'lidarr' DO NOTHING`, append(inputArgs, string(core.StateWanted), now)...); err != nil {
		return 0, 0, fmt.Errorf("sync wanted jobs: insert: %w", err)
	}

	// The transition is state-gated, and only IDs returned by that transition
	// have their stale children removed. The dependency on deleted_transfers
	// makes transfer deletion precede candidate deletion for FK integrity.
	var reentered, deletedTransfers, deletedCandidates int
	if err := tx.QueryRowContext(ctx, `WITH wanted AS (`+wantedInput+`),
		reentered AS (
			UPDATE album_jobs AS jobs
			SET state = $6, retries = 0, not_before = NULL, failed_at = NULL, updated_at = $7
			FROM wanted
			WHERE jobs.lidarr_album_id = wanted.lidarr_album_id AND jobs.state = $8
			RETURNING jobs.id
		), deleted_transfers AS (
			DELETE FROM transfers AS transfers USING candidates, reentered
			WHERE transfers.candidate_id = candidates.id AND candidates.album_job_id = reentered.id
			RETURNING transfers.id
		), deleted_candidates AS (
			DELETE FROM candidates AS candidates USING reentered
			WHERE candidates.album_job_id = reentered.id
			  AND (SELECT count(*) FROM deleted_transfers) >= 0
			RETURNING candidates.id
		)
		SELECT (SELECT count(*) FROM reentered), (SELECT count(*) FROM deleted_transfers), (SELECT count(*) FROM deleted_candidates)`,
		append(inputArgs, string(core.StateWanted), now, string(core.StateCancelled))...).Scan(&reentered, &deletedTransfers, &deletedCandidates); err != nil {
		return 0, 0, fmt.Errorf("sync wanted jobs: re-enter cancelled: %w", err)
	}

	// WANTED jobs receive a full refresh and a new updated_at. The state
	// predicate is part of the UPDATE so a concurrent transition cannot be
	// overwritten based on a stale read.
	if _, err := tx.ExecContext(ctx, `WITH wanted AS (`+wantedInput+`)
		UPDATE album_jobs AS jobs
		SET title = wanted.title, artist_name = wanted.artist_name,
			release_date = wanted.release_date, artist_id = wanted.artist_id, updated_at = $6
		FROM wanted
		WHERE jobs.lidarr_album_id = wanted.lidarr_album_id AND jobs.state = $7`,
		append(inputArgs, now, string(core.StateWanted))...); err != nil {
		return 0, 0, fmt.Errorf("sync wanted jobs: refresh metadata: %w", err)
	}

	// Jobs already past WANTED only self-heal missing metadata. As with the old
	// single-job API, a partially empty record is replaced from one consistent
	// snapshot, while updated_at remains untouched.
	if _, err := tx.ExecContext(ctx, `WITH wanted AS (`+wantedInput+`)
		UPDATE album_jobs AS jobs
		SET title = wanted.title, artist_name = wanted.artist_name,
			release_date = wanted.release_date, artist_id = wanted.artist_id
		FROM wanted
		WHERE jobs.lidarr_album_id = wanted.lidarr_album_id AND jobs.state <> $6
		  AND (jobs.title = '' OR jobs.artist_name = '' OR jobs.release_date = '' OR jobs.artist_id = 0)`,
		append(inputArgs, string(core.StateWanted))...); err != nil {
		return 0, 0, fmt.Errorf("sync wanted jobs: backfill metadata: %w", err)
	}

	res, err := tx.ExecContext(ctx, `UPDATE album_jobs
		SET state = $1, updated_at = $2
		WHERE state NOT IN ($3, $4, $5) AND source = 'lidarr' AND lidarr_album_id <> ALL($6::bigint[])`,
		string(core.StateCancelled), now, string(core.StateDone), string(core.StateCancelled), string(core.StateFailed), ids)
	if err != nil {
		return 0, 0, fmt.Errorf("sync wanted jobs: cancel: %w", err)
	}
	cancelledRows, err := res.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("sync wanted jobs: cancel rows affected: %w", err)
	}

	// Failed revival also starts a clean cycle. The strict failed_at < cutoff
	// predicate intentionally does not revive a job exactly on the boundary.
	var revivedRows int
	if err := tx.QueryRowContext(ctx, `WITH revived AS (
			UPDATE album_jobs
			SET state = $1, retries = 0, not_before = NULL, failed_at = NULL, updated_at = $2
			WHERE state = $3 AND failed_at < $4 AND lidarr_album_id = ANY($5::bigint[])
			RETURNING id
		), deleted_transfers AS (
			DELETE FROM transfers AS transfers USING candidates, revived
			WHERE transfers.candidate_id = candidates.id AND candidates.album_job_id = revived.id
			RETURNING transfers.id
		), deleted_candidates AS (
			DELETE FROM candidates AS candidates USING revived
			WHERE candidates.album_job_id = revived.id
			  AND (SELECT count(*) FROM deleted_transfers) >= 0
			RETURNING candidates.id
		)
		SELECT (SELECT count(*) FROM revived), (SELECT count(*) FROM deleted_transfers), (SELECT count(*) FROM deleted_candidates)`,
		string(core.StateWanted), now, string(core.StateFailed), failedCutoff, ids).Scan(&revivedRows, &deletedTransfers, &deletedCandidates); err != nil {
		return 0, 0, fmt.Errorf("sync wanted jobs: revive failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("sync wanted jobs: commit: %w", err)
	}
	return int(cancelledRows), revivedRows, nil
}

// UpsertWantedJob inserts a WANTED job for the album (no-op if one already
// exists) and, in the same transaction, re-enters a previously-CANCELLED job
// whose album is wanted again: it is reset to WANTED with a clean slate
// (retries=0, not_before/failed_at cleared, leftover candidates+transfers
// deleted, same as RetryFailedJob), gated strictly on state='CANCELLED' so an
// in-flight job for a still-wanted album is left untouched. Without this a
// re-wanted CANCELLED album would never re-enter, since the INSERT is ON
// CONFLICT DO NOTHING.
func (s *Store) UpsertWantedJob(ctx context.Context, lidarrAlbumID int64, now time.Time) (core.AlbumJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.AlbumJob{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO album_jobs (lidarr_album_id, state, created_at, updated_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT(lidarr_album_id) WHERE source = 'lidarr' DO NOTHING`,
		lidarrAlbumID, string(core.StateWanted), now, now); err != nil {
		return core.AlbumJob{}, fmt.Errorf("insert job: %w", err)
	}

	// Re-enter a re-wanted CANCELLED job with a clean slate.
	res, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, retries = 0, not_before = NULL, failed_at = NULL, updated_at = $2
		 WHERE lidarr_album_id = $3 AND state = $4`,
		string(core.StateWanted), now, lidarrAlbumID, string(core.StateCancelled))
	if err != nil {
		return core.AlbumJob{}, fmt.Errorf("re-enter cancelled job: %w", err)
	}
	reentered, err := res.RowsAffected()
	if err != nil {
		return core.AlbumJob{}, fmt.Errorf("re-enter cancelled job: rows affected: %w", err)
	}
	if reentered > 0 {
		// Wipe the stale cycle's candidates+transfers for the same ownership/FK
		// and clean-slate reason as RetryFailedJob.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM transfers WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id IN (SELECT id FROM album_jobs WHERE lidarr_album_id = $1))`,
			lidarrAlbumID); err != nil {
			return core.AlbumJob{}, fmt.Errorf("re-enter cancelled job: delete transfers: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM candidates WHERE album_job_id IN (SELECT id FROM album_jobs WHERE lidarr_album_id = $1)`,
			lidarrAlbumID); err != nil {
			return core.AlbumJob{}, fmt.Errorf("re-enter cancelled job: delete candidates: %w", err)
		}
	}

	var j core.AlbumJob
	var state, source string
	err = tx.QueryRowContext(ctx,
		`SELECT id, COALESCE(lidarr_album_id, 0), state, candidates_tried, next_attempt_at, created_at, updated_at, artist_id, retries, not_before, failed_at, source
		 FROM album_jobs WHERE lidarr_album_id = $1`, lidarrAlbumID).
		Scan(&j.ID, &j.LidarrAlbumID, &state, &j.CandidatesTried, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt, &j.ArtistID, &j.Retries, &j.NotBefore, &j.FailedAt, &source)
	if err != nil {
		return core.AlbumJob{}, fmt.Errorf("read job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return core.AlbumJob{}, fmt.Errorf("upsert wanted job: commit: %w", err)
	}
	j.State = core.AlbumJobState(state)
	j.Source = core.JobSource(source)
	return j, nil
}

// SetJobTrackBand caches the album's valid track-count band (min/max across
// all Lidarr releases) on the job, written by Discovery once per search. Like
// BackfillJobMetadataIfEmpty this is a metadata cache write, so updated_at is
// deliberately not bumped — the band must not reset fairness ordering or
// stuck-detection clocks.
func (s *Store) SetJobTrackBand(ctx context.Context, jobID int64, minTracks, maxTracks int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET min_track_count = $1, max_track_count = $2 WHERE id = $3`,
		minTracks, maxTracks, jobID)
	if err != nil {
		return fmt.Errorf("set job track band: %w", err)
	}
	return nil
}
