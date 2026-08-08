// Package store: bulk_retry.go holds the set-at-a-time counterpart of
// RetryFailedJob/RetryManualJob (issue #378). It lives beside them in spirit
// but not in dashboard.go, whose header promises that nothing in it mutates
// state — the filter machinery it borrows (jobViewFrom, dashboardJobsWhere,
// validateDashboardJobsQuery) is read-path code this file reuses rather than
// re-derives.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// BulkRetryResult reports what a bulk retry actually did. A partial outcome is
// the normal case, not a failure: the selection and the write are separate
// steps, the `failed` status deliberately over-matches (see BulkRetryJobs),
// and jobs move under the operation while it runs.
type BulkRetryResult struct {
	// Retried is how many jobs were actually revived.
	Retried int64
	// Skipped is how many jobs matched the filter but were left untouched —
	// not retryable when the write reached them, or a manual job with no
	// candidate row left to revive.
	Skipped int64
}

// BulkRetryJobs revives every retryable job in the filtered set q describes —
// the same status/source/search axes the jobs list renders, with q's paging
// and sort fields ignored, so the operation acts on the whole view rather
// than the page on screen (issue #378).
//
// The two source paths of issue #347 survive into bulk and are applied in one
// transaction:
//
//   - lidarr-sourced jobs get RetryFailedJob's semantics — WANTED, counters
//     zeroed, candidates and their transfers deleted, a clean re-search.
//   - manual jobs get RetryManualJob's — SELECTING, transfers deleted but the
//     user's chosen candidate revived to NEW with fail_reason and
//     import_submitted_at cleared, so the same peer is retried rather than
//     hunted for. RetryManualJob's rollback for a job with no candidate row
//     left is expressed here as an EXISTS predicate on the write instead;
//     such a job is reported skipped, which is the same answer the per-job
//     path gives.
//
// The state guard lives in the write, not in the selection. That is what makes
// a broad filter harmless: q.Filter == "failed" is status-derived and also
// matches a still-DOWNLOADING job whose current candidate's transfers have all
// errored and whose next candidate the pipeline is about to try
// (dashboardJobStatusSQL), so the selection is an upper bound, not a
// retryable set. The worst outcome of a misread filter is 0 retried, N
// skipped.
//
// q is validated with the read path's own validateDashboardJobsQuery rather
// than a private allowlist — the two copies of the filter list that already
// exist have drifted apart before (issue #310), and a third would be a third
// chance at it.
func (s *Store) BulkRetryJobs(ctx context.Context, q DashboardJobsQuery, now time.Time) (BulkRetryResult, error) {
	// Paging and sort are meaningless for a set operation but are part of the
	// validated shape, so they are normalised to valid defaults rather than
	// validated against whatever the caller happened to leave in them.
	q.Page = 0
	q.PageSize = DashboardJobsPageSize
	q.Sort = "st"
	q.Dir = "asc"
	if err := validateDashboardJobsQuery(q); err != nil {
		return BulkRetryResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BulkRetryResult{}, err
	}
	defer tx.Rollback()

	where, args := dashboardJobsWhere(q, true, true)
	rows, err := tx.QueryContext(ctx, `SELECT j.id`+jobViewFrom+where, args...)
	if err != nil {
		return BulkRetryResult{}, fmt.Errorf("bulk retry: select scope: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return BulkRetryResult{}, fmt.Errorf("bulk retry: scan scope: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return BulkRetryResult{}, fmt.Errorf("bulk retry: scope rows: %w", err)
	}
	rows.Close()
	if len(ids) == 0 {
		return BulkRetryResult{}, nil
	}

	// Lidarr path: RetryFailedJob's clean slate. The deleted_transfers CTE is
	// referenced by deleted_candidates purely to order the two — the FK points
	// from transfers to candidates, so the child rows must go first.
	var lidarrRevived int64
	if err := tx.QueryRowContext(ctx, `WITH revived AS (
			UPDATE album_jobs
			SET state = $1, retries = 0, empty_searches = 0, not_before = NULL, failed_at = NULL, updated_at = $2
			WHERE id = ANY($3::bigint[]) AND source = 'lidarr' AND state IN ($4, $5, $6)
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
		SELECT (SELECT count(*) FROM revived)`,
		string(core.StateWanted), now, ids,
		string(core.StateFailed), string(core.StateParked), string(core.StateOrphaned),
	).Scan(&lidarrRevived); err != nil {
		return BulkRetryResult{}, fmt.Errorf("bulk retry: revive lidarr jobs: %w", err)
	}

	// Manual path: RetryManualJob's same-peer retry. The EXISTS is the
	// selection-side form of that function's no-candidate rollback.
	var manualRevived int64
	if err := tx.QueryRowContext(ctx, `WITH revived AS (
			UPDATE album_jobs
			SET state = $1, retries = 0, empty_searches = 0, not_before = NULL, failed_at = NULL, updated_at = $2
			WHERE id = ANY($3::bigint[]) AND source = 'manual' AND state IN ($4, $5, $6)
			  AND EXISTS (SELECT 1 FROM candidates WHERE candidates.album_job_id = album_jobs.id)
			RETURNING id
		), deleted_transfers AS (
			DELETE FROM transfers AS transfers USING candidates, revived
			WHERE transfers.candidate_id = candidates.id AND candidates.album_job_id = revived.id
			RETURNING transfers.id
		), revived_candidates AS (
			UPDATE candidates AS candidates
			SET state = $7, fail_reason = '', import_submitted_at = NULL, updated_at = $2
			FROM revived
			WHERE candidates.album_job_id = revived.id
			  AND (SELECT count(*) FROM deleted_transfers) >= 0
			RETURNING candidates.id
		)
		SELECT (SELECT count(*) FROM revived)`,
		string(core.StateSelecting), now, ids,
		string(core.StateFailed), string(core.StateParked), string(core.StateOrphaned),
		string(core.CandidateNew),
	).Scan(&manualRevived); err != nil {
		return BulkRetryResult{}, fmt.Errorf("bulk retry: revive manual jobs: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return BulkRetryResult{}, fmt.Errorf("bulk retry: commit: %w", err)
	}
	retried := lidarrRevived + manualRevived
	return BulkRetryResult{Retried: retried, Skipped: int64(len(ids)) - retried}, nil
}
