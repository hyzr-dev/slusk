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

// RejectedCandidates returns every candidate this job has already failed on.
// Discovery calls it once per search cycle; the result is normally a handful of
// rows (bounded by MaxCandidates × MaxRetries). Deliberately unordered - the
// only consumer collapses it into a set, and any ORDER BY here would cost a
// sort the primary key cannot serve.
func (s *Store) RejectedCandidates(ctx context.Context, jobID int64) ([]CandidateRejection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT username, release_dir, reason, created_at FROM candidate_rejections
		 WHERE album_job_id = $1`, jobID)
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
// ON CONFLICT keeps the newest reason and timestamp. The same pair can fail
// twice - a job whose rejections were cleared by an explicit retry, or (before
// this table existed) a cycle that already re-cached it.
func recordRejectionTx(ctx context.Context, tx *sql.Tx, candidateID, jobID int64, reason string, now time.Time) error {
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

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO candidate_rejections (album_job_id, username, release_dir, reason, created_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (album_job_id, username, release_dir)
		 DO UPDATE SET reason = EXCLUDED.reason, created_at = EXCLUDED.created_at`,
		jobID, username, dir, reason, now); err != nil {
		return fmt.Errorf("record rejection: %w", err)
	}
	return nil
}
