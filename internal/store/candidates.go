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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/jackc/pgx/v5/pgconn"
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
// empty_searches=0, not_before=NULL): a successful search starts a fresh
// cycle, since retries/backoff track *search* failures (candidates exhausted
// after filtering) rather than individual candidate failures, and
// empty_searches tracks the separate no-raw-results streak.
//
// The reset is guarded on WANTED, the state Discovery reads the job in and
// advances it out of on the very next call (see discovery.go's searchJob), per
// the single-writer invariant that every UPDATE touching a job's cycle must be
// conditional. Without the guard this can renew updated_at on a job sitting in
// DONE or FAILED without moving it out - and the Overview panel reads that
// stamp as a completion timestamp, so an old job would resurface under
// "recently finished" (issue #294). The reset of retries is the sharper edge:
// zeroing the retry budget of a job that already failed would make it
// unfailable. Neither is reachable today - Discovery only ever gets here with a
// job it read in WANTED - so this closes the hole by construction rather than
// fixing an observed bug.
//
// A bounced guard is not an error: the candidate rows are still committed. A
// job that left WANTED mid-search keeps them as inert rows nothing will read,
// which is what Discovery already documents and accepts.
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
		`UPDATE album_jobs SET retries = 0, empty_searches = 0, not_before = NULL, updated_at = $1 WHERE id = $2 AND state = $3`,
		now, jobID, string(core.StateWanted)); err != nil {
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
// It records no rejection history: use RejectCandidateAndAdvance when the
// candidate's own content is what failed. See that function for the split.
func (s *Store) FailCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, reason string, from, to core.AlbumJobState, now time.Time) (bool, error) {
	return s.failCandidateAndAdvance(ctx, candidateID, jobID, reason, from, to, rejectionRule{}, now)
}

// CooldownCandidateAndAdvance is the middle setting between
// FailCandidateAndAdvance (forget it happened) and RejectCandidateAndAdvance
// (remember forever): the candidate is recorded in the job's rejection history
// with an expiry, so Discovery skips it for a while and reconsiders it after
// (issue #507).
//
// It exists because a failed *download* sits between the two cases
// RejectCandidateAndAdvance's doc comment describes. Forgetting is wrong -
// ResetJobToWanted wipes the candidate cache, the next search returns
// substantially the same peers, and the job re-downloads the peer that just
// failed, which is what #507 observed in production. Remembering forever is
// equally wrong for exactly the reason given there: a single mid-transfer
// dropout would make an album with one seeder permanently unfetchable.
//
// The delay escalates from policy.Base per failure of the same (username,
// release directory) pair, bounded by policy.Cap. Callers own the policy
// because backoff shape is pipeline configuration, not a storage concern.
//
// NOT for a failure that is slusk's own fault. A candidate deferred because
// another job holds its download folder (issue #471) reaches its ceiling
// through FailCandidateAndAdvance, and must keep doing so: the peer did
// nothing wrong and cooling it down would punish it for a local collision.
func (s *Store) CooldownCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, reason string, from, to core.AlbumJobState, policy CooldownPolicy, now time.Time) (bool, error) {
	return s.failCandidateAndAdvance(ctx, candidateID, jobID, reason, from, to,
		rejectionRule{record: true, cooldown: &policy}, now)
}

// RejectCandidateAndAdvance is FailCandidateAndAdvance plus a permanent record
// of this candidate in the job's rejection history, written in the same
// transaction (issue #317): ResetJobToWanted deletes the job's candidates on
// every retry cycle, and Discovery's next - identical - search would otherwise
// re-cache and re-download the peer that just failed.
//
// The split is about *whose fault* the failure was, because the record outlives
// every automatic retry. Content faults belong here: Lidarr rejected the files,
// or they cannot cover any known edition. Environmental ones do not - a peer
// going offline mid-transfer, or Lidarr stalling on an import, says nothing
// about the files, and blacklisting on it would let one timeout make an album
// with a single seeder permanently unfetchable. Those keep
// FailCandidateAndAdvance, whose soft counterpart is the peer-reliability
// weighting recordOutcome already applies.
//
// Sharing the transaction is deliberate for the same reason: a rejection
// recorded against a candidate whose failure write rolled back would blacklist
// a peer that never actually failed.
func (s *Store) RejectCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, reason string, from, to core.AlbumJobState, now time.Time) (bool, error) {
	return s.failCandidateAndAdvance(ctx, candidateID, jobID, reason, from, to, rejectionRule{record: true}, now)
}

// rejectionRule says what a terminal candidate write leaves in the job's
// rejection history. Its zero value records nothing, which is what the success
// and plain-failure paths want; record with a nil cooldown is permanent, and a
// non-nil cooldown expires. The three cases live in one value so "permanent,
// but with an expiry" and "timed, but with no time" cannot be written down.
type rejectionRule struct {
	record   bool
	cooldown *CooldownPolicy
}

func (s *Store) failCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, reason string, from, to core.AlbumJobState, rule rejectionRule, now time.Time) (bool, error) {
	return s.terminalCandidateAndAdvance(ctx, candidateID, jobID,
		`UPDATE candidates SET state = $1, fail_reason = $2, updated_at = $3 WHERE id = $4 AND state = $5`,
		[]any{string(core.CandidateFailed), reason, now, candidateID, string(core.CandidateActive)},
		from, to, reason, rule, now)
}

// SucceedCandidateAndAdvance is FailCandidateAndAdvance's success twin: it marks
// an ACTIVE candidate SUCCEEDED and advances its job from->to in one tx, with
// the same commit-both-or-neither guarantee.
func (s *Store) SucceedCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, from, to core.AlbumJobState, now time.Time) (bool, error) {
	return s.terminalCandidateAndAdvance(ctx, candidateID, jobID,
		`UPDATE candidates SET state = $1, updated_at = $2 WHERE id = $3 AND state = $4`,
		[]any{string(core.CandidateSucceeded), now, candidateID, string(core.CandidateActive)},
		from, to, "", rejectionRule{}, now)
}

// terminalCandidateAndAdvance runs candSQL (the candidate terminal-state write,
// guarded on state='ACTIVE') and the job's conditional from->to advance in one
// transaction. Either write affecting zero rows rolls back both. rule asks for
// a candidate_rejections row (with reason) alongside them, in the same tx - only
// the failure paths want one.
func (s *Store) terminalCandidateAndAdvance(ctx context.Context, candidateID, jobID int64, candSQL string, candArgs []any, from, to core.AlbumJobState, reason string, rule rejectionRule, now time.Time) (bool, error) {
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

	// import_refused_reason is written in the same statement as the state that
	// gives it meaning (issue #470). Keying it on the destination state rather
	// than on a separate flag makes both "IMPORT_REFUSED with no reason" and "a
	// reason on a job that is not refused" unrepresentable, and leaves every
	// other destination's value untouched.
	res, err = tx.ExecContext(ctx,
		`UPDATE album_jobs SET state = $1, updated_at = $2,
		        import_refused_reason = CASE WHEN $1 = $5 THEN $6 ELSE import_refused_reason END
		 WHERE id = $3 AND state = $4`,
		string(to), now, jobID, string(from), string(core.StateImportRefused), reason)
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

	if rule.record {
		if err := recordRejectionTx(ctx, tx, candidateID, jobID, reason, rule.cooldown, now); err != nil {
			return false, err
		}
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
//
// deadline is the wall-clock time the created transfers are considered overdue
// at, normally now + pipeline.transfer_deadline. RecordEnqueueIntent rewrites
// it when a file is actually handed to the peer, so this initial value only
// covers the PENDING window - but it must still be in the future (#441): a row
// created past its own deadline reads as overdue to anything that later widens
// the deadline sweep to include PENDING.
func (s *Store) ActivateCandidateWithTransfers(ctx context.Context, candidateID, jobID int64, maxActive int, deadline, now time.Time) (activated, capFull bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()

	// COUNT followed by UPDATE is not concurrency-safe under READ COMMITTED by
	// itself: concurrent selectors could all observe the same free slot. A
	// transaction-scoped advisory lock serializes only activation/cap decisions
	// without blocking unrelated album_jobs writes.
	const activationLockKey int64 = 0x736c736b64617272 // "slusk"
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, activationLockKey); err != nil {
		return false, false, fmt.Errorf("lock candidate activation: %w", err)
	}

	var username string
	var files []byte
	var releaseDate string
	if err := tx.QueryRowContext(ctx,
		`SELECT c.username, c.files, j.release_date
		   FROM candidates c
		   JOIN album_jobs j ON j.id = c.album_job_id
		  WHERE c.id = $1 AND c.album_job_id = $2
		    AND c.state = $3 AND j.state = $4
		  FOR UPDATE OF c, j`,
		candidateID, jobID, string(core.CandidateNew), string(core.StateSelecting)).Scan(&username, &files, &releaseDate); err != nil {
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

	tracks := len(candidateFiles)
	format := dominantFormat(candidateFiles)
	year := deriveYear(releaseDate)

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
		        (f.value->>'size')::bigint, $4, $5
		   FROM jsonb_array_elements($6::jsonb) WITH ORDINALITY AS f(value, ord)
		  ORDER BY f.ord`,
		candidateID, username, string(core.TransferPending), deadline, now, string(files)); err != nil {
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
		`UPDATE album_jobs SET state = $1, updated_at = $2, year = $3, tracks = $4, format = $5 WHERE id = $6 AND state = $7`,
		string(core.StateDownloading), now, year, tracks, format, jobID, string(core.StateSelecting))
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

// DeferCandidate records that candidateID is waiting for another job to release
// its download folder (issue #471), and returns when the wait began together
// with whether this call is what started it.
//
// The stored timestamp is the FIRST deferral's, not the latest: a candidate
// re-deferred on every tick would otherwise push its own deadline forward
// forever, and the ceiling that breaks an unbounded wait would never fire.
//
// first is true exactly once per wait, because it is true exactly when the
// column goes NULL → set. That is what de-duplicates the candidate_deferred
// event — one per wait rather than one per tick — with no counter to keep.
func (s *Store) DeferCandidate(ctx context.Context, candidateID int64, now time.Time) (since time.Time, first bool, err error) {
	// The old value has to be read in the same statement as the write: reading
	// it separately under READ COMMITTED would let two ticks both see NULL and
	// both report a fresh wait. FOR UPDATE serializes the row against itself.
	err = s.db.QueryRowContext(ctx,
		`WITH prev AS (SELECT id, deferred_since FROM candidates WHERE id = $1 FOR UPDATE)
		 UPDATE candidates c SET deferred_since = COALESCE(prev.deferred_since, $2), updated_at = $2
		 FROM prev WHERE c.id = prev.id
		 RETURNING c.deferred_since, prev.deferred_since IS NULL`,
		candidateID, now).Scan(&since, &first)
	if errors.Is(err, sql.ErrNoRows) {
		// The candidate was deleted under us (a reset or a manual action). Not
		// an error: the caller's next read finds no active candidate and moves
		// on.
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("defer candidate: %w", err)
	}
	return since, first, nil
}

// ClearCandidateDeferral ends a wait recorded by DeferCandidate, so a candidate
// that is deferred again later starts a fresh clock and reports a fresh event.
// Leaving a stale timestamp behind would make the next wait inherit an expired
// deadline and fail the candidate on its first deferred tick.
func (s *Store) ClearCandidateDeferral(ctx context.Context, candidateID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE candidates SET deferred_since = NULL WHERE id = $1 AND deferred_since IS NOT NULL`,
		candidateID); err != nil {
		return fmt.Errorf("clear candidate deferral: %w", err)
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

// dominantFormat returns the most common uppercased file extension across the
// candidate's files (e.g. "FLAC", "MP3"), or nil if none have a recognisable
// extension. Ties break by count then alphabetically for determinism.
func dominantFormat(files []core.CandidateFile) *string {
	counts := make(map[string]int)
	for _, f := range files {
		ext := strings.TrimPrefix(filepath.Ext(f.Filename), ".")
		if ext == "" {
			continue
		}
		counts[strings.ToUpper(ext)]++
	}
	if len(counts) == 0 {
		return nil
	}

	var best string
	for ext, count := range counts {
		switch {
		case best == "":
			best = ext
		case count > counts[best]:
			best = ext
		case count == counts[best] && ext < best:
			best = ext
		}
	}
	return &best
}

// deriveYear extracts a 4-digit leading year from a release_date string
// (handles both "2024-03-15T00:00:00Z" and "2024-01-01" forms), or nil if
// unparseable or implausible.
func deriveYear(releaseDate string) *int {
	if len(releaseDate) < 4 {
		return nil
	}
	year, err := strconv.Atoi(releaseDate[:4])
	if err != nil {
		return nil
	}
	if year < 1000 || year > 9999 {
		return nil
	}
	return &year
}
