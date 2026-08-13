// Package store: rejections.go holds the per-job record of candidates that have
// already been tried and failed (issue #317).
//
// It exists because `candidates` cannot carry this: those rows are the current
// search cycle's cache and ResetJobToWanted deletes them wholesale on every
// retry. The Soulseek query is unchanged between cycles and the network answers
// with substantially the same peers, so without a record that outlives the
// cache, a job re-downloads the same failing candidate up to MaxRetries times.
//
// A rejection is identified by (album_job_id, username, release_dir) - the same
// key matcher.Rank groups search results on - and lives exactly as long as the
// job (ON DELETE CASCADE; see migration 0016).
//
// Since issue #507 a rejection is either permanent (retry_after NULL - the
// candidate's own content failed) or a cooldown (retry_after set - the download
// failed, which says nothing about the files). See migration 0021.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hyzr-dev/slusk/internal/matcher"
)

// CandidateRejection is one (username, release directory) pair a job has
// already tried and failed. Discovery filters new search results against these
// before caching them.
type CandidateRejection struct {
	Username   string
	ReleaseDir string
	Reason     string
	CreatedAt  time.Time
}

// RejectedCandidates returns every candidate this job is currently barred from
// re-trying: permanent rejections, plus cooldowns that have not yet elapsed.
// Discovery calls it once per search cycle; the result is normally a handful of
// rows (bounded by MaxCandidates × MaxRetries). Deliberately unordered - the
// only consumer collapses it into a set, and any ORDER BY here would cost a
// sort the primary key cannot serve.
//
// now is the caller's tick time rather than now() in SQL, matching every other
// time-dependent read in the pipeline: a module's whole tick must agree on what
// "now" is, or a job can be filtered against one instant and written against
// another. It also keeps this testable without sleeping.
//
// An elapsed cooldown is filtered out but NOT deleted: the row's attempts count
// is what makes the next failure of the same pair back off further than the
// last, so forgetting it here would flatten the ladder to its first rung
// forever. The rows die with the job (ON DELETE CASCADE).
func (s *Store) RejectedCandidates(ctx context.Context, jobID int64, now time.Time) ([]CandidateRejection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT username, release_dir, reason, created_at FROM candidate_rejections
		 WHERE album_job_id = $1 AND (retry_after IS NULL OR retry_after > $2)`, jobID, now)
	if err != nil {
		return nil, fmt.Errorf("rejected candidates: %w", err)
	}
	defer rows.Close()

	var out []CandidateRejection
	for rows.Next() {
		var r CandidateRejection
		if err := rows.Scan(&r.Username, &r.ReleaseDir, &r.Reason, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("rejected candidates: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rejected candidates: %w", err)
	}
	return out, nil
}

// CountRejectionsByReason returns how many distinct candidates this job has had
// rejected under the given reason. It backs Importing's cap on a job that keeps
// being told the same thing (issue #472).
//
// The count is over distinct (username, release directory) pairs, not over
// attempts: recordRejectionTx upserts on that key, so re-failing the same peer
// and folder does not advance it. It also skips a candidate whose cached files
// carry no usable filename, which under-counts - a cap built on this fires
// late, never early.
//
// The lifetime is the one the caller needs and comes for free: these rows
// survive ResetJobToWanted, so a fresh search cycle does not reset the count.
// They are cleared by RetryFailedJob and both SyncWantedJobs re-entry
// branches.
//
// RetryManualJob is the exception, and the one worth knowing about: it resets
// retries like the others but deliberately keeps the candidate row, and clears
// no rejections. A manual job's count therefore survives every press of Retry.
// Nothing depends on that today - a manual job has one candidate and Selecting
// fails it on the first rejection, so it cannot reach a cap - but a caller
// that assumes "any retry starts the count over" is wrong for that branch.
func (s *Store) CountRejectionsByReason(ctx context.Context, jobID int64, reason string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM candidate_rejections WHERE album_job_id = $1 AND reason = $2`,
		jobID, reason).Scan(&n); err != nil {
		return 0, fmt.Errorf("count rejections by reason: %w", err)
	}
	return n, nil
}

// recordRejectionTx appends the given candidate's (username, release directory)
// to the job's rejection history, inside the caller's transaction.
//
// The release directory is derived from the candidate's cached file list rather
// than passed in by the caller, because that list is what the search produced
// and what the next search will produce again - deriving it here keeps the write
// and Discovery's later comparison reading the same field through the same
// matcher.ReleaseDir, instead of three call sites each deriving their own key.
// Only the first filename is fetched (files->0->>'filename'), so the whole file
// array is not decoded to read one string.
//
// A candidate with no usable first filename has no directory to key on, so it
// records nothing rather than erroring: an empty key would match every future
// candidate whose path is equally unparseable and blacklist them all, and
// failing here would roll back the caller's candidate-fail and job-advance
// writes over a piece of bookkeeping - deterministically, on every retry, which
// would wedge the job.
//
// ON CONFLICT keeps the newest reason and timestamp and advances attempts. The
// same pair can fail twice - a job whose rejections were cleared by an explicit
// retry, a cooldown that elapsed and let the pair be tried again, or (before
// this table existed) a cycle that already re-cached it.
//
// cooldown nil records a permanent rejection (retry_after NULL); non-nil
// escalates from the row's attempts count. A permanent rejection always wins:
// re-failing a cooled-down pair on content clears retry_after, because the
// evidence changed from "this download did not finish" to "these files are
// wrong", and only the latter is worth remembering forever.
func recordRejectionTx(ctx context.Context, tx *sql.Tx, candidateID, jobID int64, reason string, cooldown *CooldownPolicy, now time.Time) error {
	var username string
	var firstFile sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT username, files->0->>'filename' FROM candidates WHERE id = $1`, candidateID).
		Scan(&username, &firstFile); err != nil {
		return fmt.Errorf("record rejection: load candidate: %w", err)
	}
	if !firstFile.Valid || firstFile.String == "" {
		return nil
	}
	dir := matcher.ReleaseDir(firstFile.String)

	// retry_after is written in a second statement rather than computed in SQL
	// so the escalation ladder stays one Go function (see cooldownFor), read by
	// tests directly. attempts has to come back first: it is the ladder's only
	// input and the upsert is what advances it.
	var attempts int
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO candidate_rejections (album_job_id, username, release_dir, reason, created_at, attempts)
		 VALUES ($1, $2, $3, $4, $5, 1)
		 ON CONFLICT (album_job_id, username, release_dir)
		 DO UPDATE SET reason = EXCLUDED.reason, created_at = EXCLUDED.created_at,
		               attempts = candidate_rejections.attempts + 1
		 RETURNING attempts`,
		jobID, username, dir, reason, now).Scan(&attempts); err != nil {
		return fmt.Errorf("record rejection: %w", err)
	}

	var retryAfter any // nil -> SQL NULL -> permanent
	if cooldown != nil {
		retryAfter = now.Add(cooldownFor(attempts, cooldown.Base, cooldown.Cap))
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE candidate_rejections SET retry_after = $1
		 WHERE album_job_id = $2 AND username = $3 AND release_dir = $4`,
		retryAfter, jobID, username, dir); err != nil {
		return fmt.Errorf("record rejection: set retry_after: %w", err)
	}
	return nil
}

// CooldownPolicy is the escalation ladder a timed rejection backs off along.
// Base is the first failure's delay before doubling; Cap bounds the growth.
type CooldownPolicy struct {
	Base time.Duration
	Cap  time.Duration
}

// cooldownFor returns base * 2^(attempts-1), capped at max: the first failure
// waits base, the second twice that, and so on. attempts is the post-increment
// failure count, so attempts <= 1 yields base rather than half of it.
//
// This mirrors pipeline.nextBackoff, which cannot be called here - internal/
// pipeline imports internal/store, so the dependency only runs one way. The
// duplication is four lines and deliberate; the alternative is hoisting the
// arithmetic into internal/core for two callers. Same trade-off the SQL copy of
// ReliabilityHistoryScore already makes (see matcher/reliability_history.go).
//
// The exponent is clamped for the same reason nextBackoff clamps it: callers
// pass a stored counter, and 1<<attempts must not overflow an int no matter how
// large that counter grows.
func cooldownFor(attempts int, base, max time.Duration) time.Duration {
	const maxExponent = 32
	exp := attempts - 1
	if exp < 0 {
		exp = 0
	}
	if exp > maxExponent {
		exp = maxExponent
	}
	d := base * time.Duration(1<<exp)
	if d > max || d < 0 { // d<0 guards against overflow wrap-around
		return max
	}
	return d
}
