// Package store: candidates.go holds the pipeline rewrite's candidates table
// read/write paths. See migrations/0001_baseline_schema.sql for why candidates replaces
// candidate_attempts for pipeline jobs: a candidate is its own attempt (NEW →
// ACTIVE → SUCCEEDED|FAILED) with its search result's file list cached as
// JSONB.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// NewCandidate is one ranked Soulseek user to cache for a job, as produced by
// a completed search.
type NewCandidate struct {
	Username string
	Score    float64
	Files    []core.CandidateFile
}

// InsertCandidates caches a job's ranked search results as NEW candidates
// and, in the same transaction, resets the job's search cycle (retries=0,
// not_before=NULL): a successful search starts a fresh cycle, since
// retries/backoff track *search* failures (empty results, candidates
// exhausted) rather than individual candidate failures.
func (s *Store) InsertCandidates(ctx context.Context, jobID int64, cands []NewCandidate, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, c := range cands {
		files, err := json.Marshal(c.Files)
		if err != nil {
			return fmt.Errorf("marshal candidate files: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO candidates (album_job_id, username, score, files, state, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			jobID, c.Username, c.Score, files, string(core.CandidateNew), now, now); err != nil {
			return fmt.Errorf("insert candidate: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET retries = 0, not_before = NULL, updated_at = $1 WHERE id = $2`,
		now, jobID); err != nil {
		return fmt.Errorf("reset job search cycle: %w", err)
	}

	return tx.Commit()
}

const candidateSelect = `SELECT id, album_job_id, username, score, files, state, fail_reason, import_submitted_at, created_at, updated_at FROM candidates`

func scanCandidate(r rowScanner) (core.Candidate, error) {
	var c core.Candidate
	var state string
	var files []byte
	if err := r.Scan(&c.ID, &c.AlbumJobID, &c.Username, &c.Score, &files, &state, &c.FailReason, &c.ImportSubmittedAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return core.Candidate{}, err
	}
	c.State = core.CandidateState(state)
	if len(files) > 0 {
		if err := json.Unmarshal(files, &c.Files); err != nil {
			return core.Candidate{}, fmt.Errorf("unmarshal candidate files: %w", err)
		}
	}
	return c, nil
}

// NextNewCandidate returns the highest-scoring NEW candidate for a job (ties
// broken by insertion order, i.e. lowest id), or found=false if none remain.
func (s *Store) NextNewCandidate(ctx context.Context, jobID int64) (core.Candidate, bool, error) {
	c, err := scanCandidate(s.db.QueryRowContext(ctx,
		candidateSelect+` WHERE album_job_id = $1 AND state = $2 ORDER BY score DESC, id ASC LIMIT 1`,
		jobID, string(core.CandidateNew)))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Candidate{}, false, nil
	}
	if err != nil {
		return core.Candidate{}, false, fmt.Errorf("next new candidate: %w", err)
	}
	return c, true, nil
}

// ActiveCandidate returns the job's ACTIVE candidate, if any (a job has at
// most one active candidate at a time by construction of ActivateCandidate).
func (s *Store) ActiveCandidate(ctx context.Context, jobID int64) (core.Candidate, bool, error) {
	c, err := scanCandidate(s.db.QueryRowContext(ctx,
		candidateSelect+` WHERE album_job_id = $1 AND state = $2`,
		jobID, string(core.CandidateActive)))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Candidate{}, false, nil
	}
	if err != nil {
		return core.Candidate{}, false, fmt.Errorf("active candidate: %w", err)
	}
	return c, true, nil
}

// FailCandidate marks a candidate FAILED with a reason.
func (s *Store) FailCandidate(ctx context.Context, candidateID int64, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE candidates SET state = $1, fail_reason = $2, updated_at = $3 WHERE id = $4`,
		string(core.CandidateFailed), reason, now, candidateID)
	if err != nil {
		return fmt.Errorf("fail candidate: %w", err)
	}
	return nil
}

// SucceedCandidate marks a candidate SUCCEEDED.
func (s *Store) SucceedCandidate(ctx context.Context, candidateID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE candidates SET state = $1, updated_at = $2 WHERE id = $3`,
		string(core.CandidateSucceeded), now, candidateID)
	if err != nil {
		return fmt.Errorf("succeed candidate: %w", err)
	}
	return nil
}

// FailCandidateAndAdvance atomically (single tx) marks an ACTIVE candidate
// FAILED and advances its job from->to. Both writes are conditional (candidate
// still ACTIVE, job still in `from`) and commit together or not at all: a job
// must never be left in DOWNLOADING/IMPORTING with no ACTIVE candidate (which
// both modules permanently skip while it holds a MaxActive slot). Returns
// whether the job row transitioned; false (with the tx rolled back, candidate
// left ACTIVE) when the job already left `from` or the candidate is no longer
// ACTIVE.
func (s *Store) FailCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, reason string, from, to core.AlbumJobState, now time.Time) (bool, error) {
	return s.terminalCandidateAndAdvance(ctx, candidateID, jobID,
		`UPDATE candidates SET state = $1, fail_reason = $2, updated_at = $3 WHERE id = $4 AND state = $5`,
		[]any{string(core.CandidateFailed), reason, now, candidateID, string(core.CandidateActive)},
		from, to, now)
}

// SucceedCandidateAndAdvance is FailCandidateAndAdvance's success twin: it marks
// an ACTIVE candidate SUCCEEDED and advances its job from->to in one tx, with
// the same commit-both-or-neither guarantee.
func (s *Store) SucceedCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, from, to core.AlbumJobState, now time.Time) (bool, error) {
	return s.terminalCandidateAndAdvance(ctx, candidateID, jobID,
		`UPDATE candidates SET state = $1, updated_at = $2 WHERE id = $3 AND state = $4`,
		[]any{string(core.CandidateSucceeded), now, candidateID, string(core.CandidateActive)},
		from, to, now)
}

// terminalCandidateAndAdvance runs candSQL (the candidate terminal-state write,
// guarded on state='ACTIVE') and the job's conditional from->to advance in one
// transaction. Either write affecting zero rows rolls back both.
func (s *Store) terminalCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, candSQL string, candArgs []any, from, to core.AlbumJobState, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, candSQL, candArgs...)
	if err != nil {
		return false, fmt.Errorf("terminal candidate: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("terminal candidate: rows affected: %w", err)
	}
	if n == 0 {
		// Candidate already left ACTIVE (double-processed): change nothing.
		return false, nil
	}

	res, err = tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, updated_at = $2 WHERE id = $3 AND state = $4`,
		string(to), now, jobID, string(from))
	if err != nil {
		return false, fmt.Errorf("advance job: %w", err)
	}
	n, err = res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("advance job: rows affected: %w", err)
	}
	if n == 0 {
		// Job left `from` underneath us (e.g. WantedSync cancel): roll back the
		// candidate write too, so we never strand the job with no ACTIVE candidate.
		return false, nil
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// MarkImportSubmitted records that ExecuteManualImport has been called for
// this candidate's transfers, gating Importing's verify-vs-confirm phase.
func (s *Store) MarkImportSubmitted(ctx context.Context, candidateID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE candidates SET import_submitted_at = $1, updated_at = $2 WHERE id = $3`,
		now, now, candidateID)
	if err != nil {
		return fmt.Errorf("mark import submitted: %w", err)
	}
	return nil
}

// ActivateCandidateWithTransfers atomically makes a NEW candidate runnable. It
// serializes cap decisions, locks and re-checks the candidate/job ownership and
// states, validates and creates every cached file as a PENDING transfer, and
// only then marks the candidate ACTIVE and the job DOWNLOADING. Any failure
// rolls all of those writes back, so Downloading can never observe a partially
// prepared job.
//
// capFull distinguishes a shared-cap block from a candidate-specific skip. A
// live remote-file ownership conflict is an expected skip (false, false, nil):
// the candidate remains NEW for a later tick while Selecting continues with
// unrelated jobs.
func (s *Store) ActivateCandidateWithTransfers(ctx context.Context, candidateID, jobID int64, maxActive int, now time.Time) (activated, capFull bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()

	// COUNT followed by UPDATE is not concurrency-safe under READ COMMITTED by
	// itself: concurrent selectors could all observe the same free slot. A
	// transaction-scoped advisory lock serializes only activation/cap decisions
	// without blocking unrelated album_jobs writes.
	const activationLockKey int64 = 0x736c736b64617272 // "slskdarr"
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, activationLockKey); err != nil {
		return false, false, fmt.Errorf("lock candidate activation: %w", err)
	}

	var username string
	var files []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT c.username, c.files
		   FROM candidates c
		   JOIN album_jobs j ON j.id = c.album_job_id
		  WHERE c.id = $1 AND c.album_job_id = $2
		    AND c.state = $3 AND j.state = $4
		  FOR UPDATE OF c, j`,
		candidateID, jobID, string(core.CandidateNew), string(core.StateSelecting)).Scan(&username, &files); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("check candidate activation eligibility: %w", err)
	}

	var candidateFiles []core.CandidateFile
	if err := json.Unmarshal(files, &candidateFiles); err != nil {
		return false, false, fmt.Errorf("validate candidate files: %w", err)
	}
	if len(candidateFiles) == 0 {
		return false, false, errors.New("validate candidate files: empty file set")
	}
	seen := make(map[string]struct{}, len(candidateFiles))
	for i, file := range candidateFiles {
		if file.Filename == "" || file.Size < 0 {
			return false, false, fmt.Errorf("validate candidate files: invalid file at index %d", i)
		}
		if _, duplicate := seen[file.Filename]; duplicate {
			return false, false, fmt.Errorf("validate candidate files: duplicate filename %q", file.Filename)
		}
		seen[file.Filename] = struct{}{}
	}

	var active int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM album_jobs WHERE state IN ($1, $2)`,
		string(core.StateDownloading), string(core.StateImporting)).Scan(&active); err != nil {
		return false, false, fmt.Errorf("count active jobs: %w", err)
	}
	if active >= maxActive {
		return false, true, nil
	}

	// Preserve the cached JSON array's order so database-trigger failure tests
	// can exercise rollback after the first, middle, and final logical insert.
	// More importantly, this single set-based statement creates the complete
	// write-ahead set before either lifecycle row becomes visible as active.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO transfers
		   (candidate_id, username, filename, state, bytes_total, deadline, updated_at)
		 SELECT $1, $2, f.value->>'filename', $3,
		        (f.value->>'size')::bigint, $4, $4
		   FROM jsonb_array_elements($5::jsonb) WITH ORDINALITY AS f(value, ord)
		  ORDER BY f.ord`,
		candidateID, username, string(core.TransferPending), now, string(files)); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idx_transfers_live_remote_owner" {
			return false, false, nil
		}
		return false, false, fmt.Errorf("create pending transfer set: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE candidates SET state = $1, updated_at = $2
		  WHERE id = $3 AND album_job_id = $4 AND state = $5`,
		string(core.CandidateActive), now, candidateID, jobID, string(core.CandidateNew))
	if err != nil {
		return false, false, fmt.Errorf("activate candidate: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, false, fmt.Errorf("activate candidate: rows affected: %w", err)
	}
	if n != 1 {
		return false, false, nil
	}

	res, err = tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, updated_at = $2 WHERE id = $3 AND state = $4`,
		string(core.StateDownloading), now, jobID, string(core.StateSelecting))
	if err != nil {
		return false, false, fmt.Errorf("advance job to downloading: %w", err)
	}
	n, err = res.RowsAffected()
	if err != nil {
		return false, false, fmt.Errorf("advance job to downloading: rows affected: %w", err)
	}
	if n != 1 {
		return false, false, nil
	}

	if err := tx.Commit(); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// DeferSelectingJob moves a candidate-specific activation skip behind its FIFO
// peers without changing lifecycle state. The guard makes it a no-op if the job
// left SELECTING after the activation attempt.
func (s *Store) DeferSelectingJob(ctx context.Context, jobID int64, now time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE album_jobs SET updated_at = $1 WHERE id = $2 AND state = $3`,
		now, jobID, string(core.StateSelecting)); err != nil {
		return fmt.Errorf("defer selecting job: %w", err)
	}
	return nil
}

// TransfersForCandidate returns all transfers belonging to a candidate.
func (s *Store) TransfersForCandidate(ctx context.Context, candidateID int64) ([]core.Transfer, error) {
	rows, err := s.db.QueryContext(ctx, transferSelect+` WHERE candidate_id = $1`, candidateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransfers(rows)
}
