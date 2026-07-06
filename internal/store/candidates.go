// Package store: candidates.go holds the pipeline rewrite's candidates table
// read/write paths. See schema.sql for why candidates replaces
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

// ActivateCandidate atomically (single tx): re-checks the job is still in
// SELECTING, counts jobs in DOWNLOADING+IMPORTING, and if < maxActive sets the
// candidate ACTIVE and the job DOWNLOADING. Returns false when the cap is full
// or the job left SELECTING — the caller just moves on.
func (s *Store) ActivateCandidate(ctx context.Context, candidateID, jobID int64, maxActive int, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var active int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM album_jobs WHERE state IN ($1, $2)`,
		string(core.StateDownloading), string(core.StateImporting)).Scan(&active); err != nil {
		return false, fmt.Errorf("count active jobs: %w", err)
	}
	if active >= maxActive {
		return false, nil
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, updated_at = $2 WHERE id = $3 AND state = $4`,
		string(core.StateDownloading), now, jobID, string(core.StateSelecting))
	if err != nil {
		return false, fmt.Errorf("advance job to downloading: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("advance job to downloading: rows affected: %w", err)
	}
	if n == 0 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE candidates SET state = $1, updated_at = $2 WHERE id = $3 AND state = $4`,
		string(core.CandidateActive), now, candidateID, string(core.CandidateNew)); err != nil {
		return false, fmt.Errorf("activate candidate: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
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
