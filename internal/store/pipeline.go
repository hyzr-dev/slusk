package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/matcher"
)

// ErrJobImporting is returned by DeleteJob when the job is currently
// IMPORTING: deleting it out from under an in-flight Lidarr import could
// leave orphaned files or a half-applied import, so the caller must wait for
// it to settle (or fail) first.
var ErrJobImporting = errors.New("job is importing")

const jobSelect = `SELECT id, COALESCE(lidarr_album_id, 0), state, candidates_tried, next_attempt_at, created_at, updated_at, title, artist_name, release_date, artist_id, retries, empty_searches, not_before, failed_at, min_track_count, max_track_count, source, album_mbid FROM album_jobs`

func scanJobs(rows *sql.Rows) ([]core.AlbumJob, error) {
	var out []core.AlbumJob
	for rows.Next() {
		var j core.AlbumJob
		var state, source string
		if err := rows.Scan(&j.ID, &j.LidarrAlbumID, &state, &j.CandidatesTried, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt, &j.Title, &j.ArtistName, &j.ReleaseDate, &j.ArtistID, &j.Retries, &j.EmptySearches, &j.NotBefore, &j.FailedAt, &j.MinTrackCount, &j.MaxTrackCount, &source, &j.AlbumMBID); err != nil {
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

// SetJobBackoff bumps retries and hides the job until notBefore. State
// unchanged. This is only reached once a search returned raw results (see
// discovery.go), so empty_searches resets to 0 - the network answered, the
// job just has no surviving candidate this cycle.
func (s *Store) SetJobBackoff(ctx context.Context, jobID int64, retries int, notBefore time.Time, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET retries = $1, empty_searches = 0, not_before = $2, updated_at = $3 WHERE id = $4`,
		retries, notBefore, now, jobID)
	if err != nil {
		return fmt.Errorf("set job backoff: %w", err)
	}
	return nil
}

// SetJobEmptySearchBackoff records a search cycle where the Soulseek network
// returned no raw results at all: it bumps empty_searches and hides the job
// until notBefore, but deliberately leaves retries and state untouched -
// unlike SetJobBackoff this never counts toward max_retries and never fails
// the job (see discovery.go's searchJob). Modelled on SetJobBackoff.
func (s *Store) SetJobEmptySearchBackoff(ctx context.Context, jobID int64, emptySearches int, notBefore time.Time, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET empty_searches = $1, not_before = $2, updated_at = $3 WHERE id = $4`,
		emptySearches, notBefore, now, jobID)
	if err != nil {
		return fmt.Errorf("set job empty search backoff: %w", err)
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
// job.Retries-as-is / 0 and nil). empty_searches always resets to 0 - a
// caller reaching this point has real candidate data to reset around, so any
// prior empty-search streak is stale. Transfers must go with the candidates
// to satisfy transfer ownership/FK integrity and leave no stale cycle data.
// The job UPDATE is guarded on the caller-supplied `from` state (per the
// single-writer invariant every transition UPDATE must be conditional): every
// caller resets a SELECTING job, so a job WantedSync cancelled underneath us
// must NOT be resurrected to WANTED nor have its candidates/transfers deleted.
// When the guard bounces (0 rows) the whole tx rolls back and nil is returned.
//
// job_download_folders is deliberately left alone (issue #314): this deletion
// is precisely what used to orphan the folders earlier search cycles wrote to,
// since afterwards nothing in the database could name them any more. The
// register's lifetime is the job's, not the cycle's.
func (s *Store) ResetJobToWanted(ctx context.Context, jobID int64, from core.AlbumJobState, retries int, notBefore *time.Time, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The guarded transition runs first: if it bounces, the deferred rollback
	// leaves the candidates/transfers intact for the job that turned CANCELLED.
	res, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, retries = $2, empty_searches = 0, not_before = $3, updated_at = $4 WHERE id = $5 AND state = $6`,
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

// RetryFailedJob manually revives one FAILED, PARKED, or legacy ORPHANED job:
// retries 0, empty_searches 0, not_before/failed_at cleared, candidates
// deleted, state WANTED. Returns false when the job is not retryable (the
// dashboard button raced a state change) or does not exist.
// Candidates/transfers must go with the reset for the same ownership/FK and
// clean-slate reason as ResetJobToWanted.
// A manual job goes through RetryManualJob instead (issue #347); this
// function is deliberately not source-guarded, since the routing between the
// two lives in app.Jobs.Retry, not here.
func (s *Store) RetryFailedJob(ctx context.Context, jobID int64, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, retries = 0, empty_searches = 0, not_before = NULL, failed_at = NULL, updated_at = $2
		 WHERE id = $3 AND state IN ($4, $5, $6)`,
		string(core.StateWanted), now, jobID,
		string(core.StateFailed), string(core.StateParked), string(core.StateOrphaned))
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
	// The rejection history (issue #317) goes with the clean slate, and only
	// here: ResetJobToWanted, the automatic retry, must keep it - outliving that
	// deletion is the entire point of the table. The rule is retries: a path
	// that resets them to 0 is starting the job over and may try the failed
	// peers again (one of them may since have fixed its share); a path that
	// bumps them is continuing the same attempt.
	if _, err := tx.ExecContext(ctx, `DELETE FROM candidate_rejections WHERE album_job_id = $1`, jobID); err != nil {
		return false, fmt.Errorf("retry failed job: delete candidate rejections: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("retry failed job: commit: %w", err)
	}
	return true, nil
}

// RetryManualJob manually revives one FAILED, PARKED, or legacy ORPHANED
// manual job (issue #347) by retrying the same peer, rather than
// RetryFailedJob's re-search: the user picked this candidate for a reason the
// protocol carries nowhere (a FLAC rip, a bitrate, a particular edition), so a
// manual job's retry must try that same peer again, not go hunting. FAILED is
// the routine path (Discovery/Selecting failing a manual job outright, or a
// candidate exhausted with none left to try); PARKED is a live one too - a
// manual job is created straight into DOWNLOADING, and ParkJobForCandidate
// parks any DOWNLOADING job whose transfer settles terminally. Legacy
// ORPHANED is kept for parity with RetryFailedJob's allowlist.
//
// Unlike RetryFailedJob it revives the job's candidates to NEW instead of
// deleting them: the cached files JSON and username are the user's original
// choice, and ActivateCandidateWithTransfers re-INSERTs the PENDING transfer
// set from that same files JSON against that same username once Selecting
// picks the job back up. Old transfers are still deleted - they are
// FAILED/CANCELLED and must go both to avoid duplicates and to clear
// idx_transfers_live_remote_owner.
//
// Returns false when the job is not a retryable manual job (wrong source,
// wrong state, or does not exist) - the dashboard button raced a state
// change, or was pointed at a lidarr-sourced job - and also when it has no
// candidate row left to revive (see the rollback below).
//
// There is deliberately no up-front check for another live candidate already
// owning one of these (peer, filename) pairs, the conflict CreateManualJob
// reports as ErrRemoteFileBusy. Creation gets that for free from the transfer
// INSERT's unique violation; a retry inserts no transfers here, so detecting
// it would cost a query for a case that resolves itself. Selecting's
// ActivateCandidateWithTransfers already handles it: the 23505 on
// idx_transfers_live_remote_owner turns into a DeferSelectingJob, which only
// bumps updated_at - the job stays SELECTING, stays runnable, and re-attempts
// activation every tick until the other owner's transfers settle. That is a
// transient wait, not a livelock, and failing the retry outright would be a
// worse answer than waiting for it.
//
// One deliberate asymmetry from CreateManualJob: creation bypasses the
// MaxActive cap since the user asked for it explicitly, but a retry goes
// through Selecting like any other SELECTING job and therefore respects the
// cap - it queues behind other work instead of activating immediately. That
// is acceptable; it is not a bug to be worked around.
func (s *Store) RetryManualJob(ctx context.Context, jobID int64, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, retries = 0, empty_searches = 0, not_before = NULL, failed_at = NULL, updated_at = $2
		 WHERE id = $3 AND source = $4 AND state IN ($5, $6, $7)`,
		string(core.StateSelecting), now, jobID, string(core.SourceManual),
		string(core.StateFailed), string(core.StateParked), string(core.StateOrphaned))
	if err != nil {
		return false, fmt.Errorf("retry manual job: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("retry manual job: rows affected: %w", err)
	}
	if n == 0 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM transfers WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id = $1)`, jobID); err != nil {
		return false, fmt.Errorf("retry manual job: delete candidate transfers: %w", err)
	}
	// fail_reason and import_submitted_at must be cleared too, not just state:
	// RetryFailedJob gets this for free by deleting the candidate row outright,
	// but a revive keeps it. A leftover fail_reason surfaces the previous
	// attempt's failure on the dashboard while the retry is still in flight
	// (JobWithTransfer, dashboard.go). A leftover import_submitted_at is worse:
	// if the revived candidate reaches IMPORTING again, Importing.Tick keys
	// verify-vs-confirm on it (importing.go) and a non-NULL timestamp skips
	// straight to confirm, whose timeout is measured from the *stale* value and
	// so has already expired - an instant failUnconfirmed.
	res, err = tx.ExecContext(ctx,
		`UPDATE candidates SET state = $1, fail_reason = '', import_submitted_at = NULL, updated_at = $2 WHERE album_job_id = $3`,
		string(core.CandidateNew), now, jobID)
	if err != nil {
		return false, fmt.Errorf("retry manual job: revive candidates: %w", err)
	}
	// A manual job with no candidate row left has nothing to retry, so roll the
	// whole transaction back rather than parking it in SELECTING with an empty
	// cache - Selecting would only fail it again on the next tick, and the user
	// would watch the Retry button appear to do nothing. This is a real
	// population, not a theoretical one: RetryFailedJob and ForceSearchJob both
	// DELETE candidates, so every manual job that went through either of them
	// before this branch shipped is sitting in production with none. Reporting
	// not-retryable is the honest answer - the peer the user chose is gone and
	// no retry can bring it back.
	revived, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("retry manual job: candidate rows affected: %w", err)
	}
	if revived == 0 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("retry manual job: commit: %w", err)
	}
	return true, nil
}

// RetryRefusedJob manually revives one IMPORT_REFUSED job (issue #470): unlike
// RetryFailedJob it does not go back to WANTED, because the files were never
// the problem - Lidarr rejected them after a complete, correct download. It
// goes straight to IMPORTING with the same candidate, so Importing's restore
// step (importing.go) can move the quarantined folder back and re-submit the
// same files.
//
// In one transaction: the job moves IMPORT_REFUSED -> IMPORTING and its
// import_refused_reason is cleared; the job's FAILED candidate (the one
// RejectCandidateAndAdvance left behind) is revived to ACTIVE with
// fail_reason and import_submitted_at cleared, for the same reasons
// RetryManualJob clears them - a stale fail_reason would show the old refusal
// on the dashboard mid-retry, and a stale import_submitted_at would make
// Importing.Tick key off it and time out instantly; and the candidate's row
// in candidate_rejections (#317) is deleted, since RejectCandidateAndAdvance
// is exactly what put it there and leaving it would have Selecting - if the
// retry ever failed back that far - immediately re-reject the same peer.
// Only that one (username, release_dir) pair is deleted, not the whole job's
// history: other peers this job already tried and rejected should stay
// blacklisted.
//
// The candidate to revive is picked by currentCandidateOrder (dashboard.go),
// the same rule the dashboard itself uses for "the job's current candidate":
// a job can carry more than one FAILED candidate (earlier rejections from
// the same search cycle, still sitting there since RejectCandidateAndAdvance
// never deletes them), and a plain WHERE state = 'FAILED' would match all of
// them - reviving stale peers alongside the one Lidarr actually refused.
//
// Returns false when the job is not IMPORT_REFUSED or has no FAILED
// candidate to revive (the dashboard button raced a state change), or does
// not exist.
func (s *Store) RetryRefusedJob(ctx context.Context, jobID int64, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, import_refused_reason = '', updated_at = $2
		 WHERE id = $3 AND state = $4`,
		string(core.StateImporting), now, jobID, string(core.StateImportRefused))
	if err != nil {
		return false, fmt.Errorf("retry refused job: update job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("retry refused job: job rows affected: %w", err)
	}
	if n == 0 {
		return false, nil
	}

	var username string
	var firstFile sql.NullString
	err = tx.QueryRowContext(ctx,
		`UPDATE candidates SET state = $1, fail_reason = '', import_submitted_at = NULL, updated_at = $2
		 WHERE id = (SELECT id FROM candidates WHERE album_job_id = $3 `+currentCandidateOrder+` LIMIT 1)
		   AND state = $4
		 RETURNING username, files->0->>'filename'`,
		string(core.CandidateActive), now, jobID, string(core.CandidateFailed)).Scan(&username, &firstFile)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("retry refused job: revive candidate: %w", err)
	}

	if firstFile.Valid && firstFile.String != "" {
		dir := matcher.ReleaseDir(firstFile.String)
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM candidate_rejections WHERE album_job_id = $1 AND username = $2 AND release_dir = $3`,
			jobID, username, dir); err != nil {
			return false, fmt.Errorf("retry refused job: delete candidate rejection: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("retry refused job: commit: %w", err)
	}
	return true, nil
}

// ParkJobForCandidate atomically records a transfer's terminal state/progress
// and marks its candidate's owning job PARKED (issue #158), but only if the job
// is still DOWNLOADING. If another transition (for example WantedSync
// cancellation) already moved the job, the transfer write still commits and
// false is returned. A stale transfer that already became terminal is a no-op.
func (s *Store) ParkJobForCandidate(ctx context.Context, transferID, candidateID int64, transferState core.TransferState, bytesDone, bytesTotal int64, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("park job for candidate: begin: %w", err)
	}
	defer tx.Rollback()

	// Cancellation locks the job before its transfers. Use the same order here
	// so parking and cancellation cannot deadlock while each holds one row.
	var jobID int64
	var jobState string
	err = tx.QueryRowContext(ctx,
		`SELECT j.id, j.state
		 FROM album_jobs j
		 JOIN candidates c ON c.album_job_id = j.id
		 JOIN transfers t ON t.candidate_id = c.id
		 WHERE c.id = $1 AND t.id = $2
		 FOR UPDATE OF j`, candidateID, transferID).Scan(&jobID, &jobState)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("park job for candidate: lock job: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE transfers SET state = $1, bytes_done = $2, bytes_total = $3,
			last_progress_at = CASE WHEN $4 > bytes_done THEN $5 ELSE last_progress_at END,
			updated_at = $6
		 WHERE id = $7 AND candidate_id = $8 AND state IN ($9, $10, $11, $12)`,
		string(transferState), bytesDone, bytesTotal, bytesDone, now, now,
		transferID, candidateID,
		string(core.TransferPending), string(core.TransferQueued),
		string(core.TransferInProgress), string(core.TransferStalled))
	if err != nil {
		return false, fmt.Errorf("park job for candidate: update transfer: %w", err)
	}
	transferRows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("park job for candidate: transfer rows affected: %w", err)
	}
	if transferRows == 0 {
		return false, nil
	}

	var jobRows int64
	if core.AlbumJobState(jobState) == core.StateDownloading {
		res, err = tx.ExecContext(ctx,
			`UPDATE album_jobs SET state = $1, updated_at = $2
			 WHERE id = $3 AND state = $4`,
			string(core.StateParked), now, jobID, string(core.StateDownloading))
		if err != nil {
			return false, fmt.Errorf("park job for candidate: update job: %w", err)
		}
		jobRows, err = res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("park job for candidate: job rows affected: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("park job for candidate: commit: %w", err)
	}
	return jobRows > 0, nil
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
// predicate is explicit rather than relying on lidarr_album_id being NULL for
// manual jobs (see #369 — that invariant was briefly broken once and a
// NULL-reliant predicate would have matched a manual job).
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
// in wantedIDs to WANTED with retries=0, empty_searches=0, not_before=NULL,
// failed_at=NULL. No production caller today (SyncWantedJobs inlines the
// same revive as a CTE), but it must not diverge from that live copy, so the
// source = 'lidarr' guard is kept here too.
func (s *Store) ReviveFailedJobs(ctx context.Context, wantedIDs []int64, cutoff time.Time, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, retries = 0, empty_searches = 0, not_before = NULL, failed_at = NULL, updated_at = $2
		 WHERE state = $3 AND failed_at < $4 AND lidarr_album_id = ANY($5) AND source = 'lidarr'`,
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
			SET state = $6, retries = 0, empty_searches = 0, not_before = NULL, failed_at = NULL, updated_at = $7
			FROM wanted
			WHERE jobs.lidarr_album_id = wanted.lidarr_album_id AND jobs.state = $8 AND jobs.source = 'lidarr'
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
		), deleted_rejections AS (
			DELETE FROM candidate_rejections AS rejections USING reentered
			WHERE rejections.album_job_id = reentered.id
			RETURNING rejections.album_job_id
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
		WHERE jobs.lidarr_album_id = wanted.lidarr_album_id AND jobs.state = $7 AND jobs.source = 'lidarr'`,
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
		WHERE jobs.lidarr_album_id = wanted.lidarr_album_id AND jobs.state <> $6 AND jobs.source = 'lidarr'
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
			SET state = $1, retries = 0, empty_searches = 0, not_before = NULL, failed_at = NULL, updated_at = $2
			WHERE state = $3 AND failed_at < $4 AND lidarr_album_id = ANY($5::bigint[]) AND source = 'lidarr'
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
		), deleted_rejections AS (
			DELETE FROM candidate_rejections AS rejections USING revived
			WHERE rejections.album_job_id = revived.id
			RETURNING rejections.album_job_id
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
// (retries=0, empty_searches=0, not_before/failed_at cleared, leftover
// candidates+transfers deleted, same as RetryFailedJob), gated strictly on
// state='CANCELLED' so an in-flight job for a still-wanted album is left
// untouched. Without this a re-wanted CANCELLED album would never re-enter,
// since the INSERT is ON CONFLICT DO NOTHING.
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
		`UPDATE album_jobs SET state = $1, retries = 0, empty_searches = 0, not_before = NULL, failed_at = NULL, updated_at = $2
		 WHERE lidarr_album_id = $3 AND state = $4 AND source = 'lidarr'`,
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
			`DELETE FROM transfers WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id IN (SELECT id FROM album_jobs WHERE lidarr_album_id = $1 AND source = 'lidarr'))`,
			lidarrAlbumID); err != nil {
			return core.AlbumJob{}, fmt.Errorf("re-enter cancelled job: delete transfers: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM candidates WHERE album_job_id IN (SELECT id FROM album_jobs WHERE lidarr_album_id = $1 AND source = 'lidarr')`,
			lidarrAlbumID); err != nil {
			return core.AlbumJob{}, fmt.Errorf("re-enter cancelled job: delete candidates: %w", err)
		}
		// The rejection history goes too - re-monitoring an album in Lidarr is
		// a start-over, and this is the singular twin of SyncWantedJobs'
		// reentered CTE, which clears it as well. Leaving it here would make
		// the same user action behave differently depending on which path the
		// album happened to arrive through.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM candidate_rejections WHERE album_job_id IN (SELECT id FROM album_jobs WHERE lidarr_album_id = $1 AND source = 'lidarr')`,
			lidarrAlbumID); err != nil {
			return core.AlbumJob{}, fmt.Errorf("re-enter cancelled job: delete candidate rejections: %w", err)
		}
	}

	var j core.AlbumJob
	var state, source string
	err = tx.QueryRowContext(ctx,
		`SELECT id, COALESCE(lidarr_album_id, 0), state, candidates_tried, next_attempt_at, created_at, updated_at, artist_id, retries, empty_searches, not_before, failed_at, source, album_mbid
		 FROM album_jobs WHERE lidarr_album_id = $1 AND source = 'lidarr'`, lidarrAlbumID).
		Scan(&j.ID, &j.LidarrAlbumID, &state, &j.CandidatesTried, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt, &j.ArtistID, &j.Retries, &j.EmptySearches, &j.NotBefore, &j.FailedAt, &source, &j.AlbumMBID)
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

// ForceSearchJob manually re-queues one job (issue #159) for an immediate
// re-search: retries/empty_searches/not_before/candidates are wiped
// clean-slate, same as RetryFailedJob, and the state is forced to WANTED so
// the next Discovery tick picks it up. Guarded on state NOT IN (DOWNLOADING,
// IMPORTING) instead of RetryFailedJob's retryable-state allowlist, since a
// force-search is valid from almost any non-active state (WANTED, SELECTING,
// FAILED, PARKED, legacy ORPHANED, CANCELLED, ...). Returns false when the
// job is actively transferring (the dashboard button raced a state change)
// or does not exist.
//
// Deliberately not source-guarded: a manual job is rejected upstream, in
// app.Jobs.ForceSearch (issue #347), before this ever runs - resetting one to
// WANTED with its candidate deleted would leave Discovery's Source guard to
// fail it right back out, since a manual job has no lidarr_album_id to search
// for. A caller reaching this function directly bypasses that check.
func (s *Store) ForceSearchJob(ctx context.Context, jobID int64, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, retries = 0, empty_searches = 0, not_before = NULL, failed_at = NULL, updated_at = $2
		 WHERE id = $3 AND state NOT IN ($4, $5)`,
		string(core.StateWanted), now, jobID, string(core.StateDownloading), string(core.StateImporting))
	if err != nil {
		return false, fmt.Errorf("force search job: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("force search job: rows affected: %w", err)
	}
	if n == 0 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM transfers WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id = $1)`, jobID); err != nil {
		return false, fmt.Errorf("force search job: delete candidate transfers: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM candidates WHERE album_job_id = $1`, jobID); err != nil {
		return false, fmt.Errorf("force search job: delete candidates: %w", err)
	}
	// The rejection history deliberately survives a force search, unlike the
	// retry paths: this button is the nudge a user reaches for when a job looks
	// stuck, which is the very symptom issue #317 produces. Clearing here would
	// send the next search straight back to re-downloading the files that have
	// been failing import - the bug, one click away.
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("force search job: commit: %w", err)
	}
	return true, nil
}

// DeleteJob permanently removes one job and its children (issue #159): the
// job row is locked with FOR UPDATE first so a concurrent transition cannot
// race the IMPORTING check, then transfers/candidates/job_events/
// job_download_folders/album_jobs are deleted in FK-safe order. Returns (false, nil) if no such job exists,
// ErrJobImporting if it is currently IMPORTING (deleting mid-import risks
// orphaned files or a half-applied Lidarr import), and (true, nil) on success.
func (s *Store) DeleteJob(ctx context.Context, jobID int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM album_jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("delete job: select for update: %w", err)
	}
	if core.AlbumJobState(state) == core.StateImporting {
		return false, ErrJobImporting
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM transfers WHERE candidate_id IN (SELECT id FROM candidates WHERE album_job_id = $1)`, jobID); err != nil {
		return false, fmt.Errorf("delete job: delete transfers: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM candidates WHERE album_job_id = $1`, jobID); err != nil {
		return false, fmt.Errorf("delete job: delete candidates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_events WHERE album_job_id = $1`, jobID); err != nil {
		return false, fmt.Errorf("delete job: delete job events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_download_folders WHERE album_job_id = $1`, jobID); err != nil {
		return false, fmt.Errorf("delete job: delete download folders: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM album_jobs WHERE id = $1`, jobID); err != nil {
		return false, fmt.Errorf("delete job: delete job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("delete job: commit: %w", err)
	}
	return true, nil
}
