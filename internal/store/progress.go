// Package store: progress.go holds the read-only aggregate that measures
// whether the pipeline's *work* is moving, as opposed to whether its *modules*
// are ticking (issue #442). Every other health signal in slusk reports on a
// module, and a module whose Tick returns nil while advancing nothing looks
// exactly like a healthy one - which is how three DOWNLOADING jobs sat
// untouched for four weeks behind a fully green /status.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// terminalJobStates are the end states excluded from a progress snapshot: the
// union of core.AlbumJobState.Terminal (the legacy engine's notion) and
// PipelineTerminal (the pipeline's). A job in one of these is finished, so its
// updated_at ages forever and says nothing about whether work is moving.
//
// Deliberately an exclusion list rather than an allow-list of live states: a
// state added to core later then shows up in the snapshot as visible noise
// instead of silently going unmeasured, which is the exact failure mode this
// aggregate exists to catch.
var terminalJobStates = []core.AlbumJobState{
	core.StateCompleted,
	core.StateDone,
	core.StateFailed,
	core.StateCancelled,
	core.StateNotImported,
}

// wedgeableJobStates are the states in which a job holds a MaxActive slot and
// is skipped outright when it has no ACTIVE candidate - Downloading's resolve
// and top-up passes and Importing both return early without writing, logging
// or recording an event. See the doc comment on FailCandidateAndAdvance.
var wedgeableJobStates = []core.AlbumJobState{
	core.StateDownloading,
	core.StateImporting,
}

// JobStateProgress is one non-terminal state's contribution to a snapshot.
type JobStateProgress struct {
	// State is the job state these numbers describe.
	State core.AlbumJobState
	// Count is how many jobs currently sit in it.
	Count int
	// OldestUpdate is the least recent updated_at among them. Callers derive
	// an age from it against their own clock; the store deliberately does no
	// time arithmetic so the value is reproducible in tests.
	OldestUpdate time.Time
}

// JobProgress is a snapshot of how recently the pipeline touched the work it
// still owns.
type JobProgress struct {
	// States holds one entry per non-terminal state that currently has jobs,
	// ordered by state name. A state with no jobs is absent rather than present
	// with zeroes: absence is the honest encoding for "nothing here", where a
	// zero age would read as "everything is fresh".
	States []JobStateProgress
	// JobsWithoutActiveCandidate counts jobs in DOWNLOADING or IMPORTING that
	// have no ACTIVE candidate - a direct measurement of the wedge shape in
	// issue #442, in which such a job holds a slot indefinitely and leaves no
	// trace anywhere.
	JobsWithoutActiveCandidate int
}

// JobProgress returns a snapshot of per-state job counts and oldest update
// times over the non-terminal states, plus the count of jobs wedged in
// DOWNLOADING/IMPORTING with no ACTIVE candidate.
//
// Read-only and cheap enough to call on a timer or per request. Keyed on
// album_jobs.updated_at rather than on anything in transfers on purpose: the
// retry path resets a transfer's bytes_done and last_progress_at to clear its
// stall clock, so those columns cannot distinguish "never moved" from
// "retried", and re-entry into SELECTING deletes a job's transfer rows
// outright - erasing the very period worth measuring.
//
// The two queries are not run in one transaction: a snapshot for a gauge does
// not need the counts to agree to the row.
func (s *Store) JobProgress(ctx context.Context) (JobProgress, error) {
	states, err := s.jobProgressByState(ctx)
	if err != nil {
		return JobProgress{}, err
	}
	wedged, err := s.countJobsWithoutActiveCandidate(ctx)
	if err != nil {
		return JobProgress{}, err
	}
	return JobProgress{States: states, JobsWithoutActiveCandidate: wedged}, nil
}

func (s *Store) jobProgressByState(ctx context.Context) ([]JobStateProgress, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT state, COUNT(*) AS job_count, MIN(updated_at) AS oldest_update
		   FROM album_jobs
		  WHERE state <> ALL($1)
		  GROUP BY state
		  ORDER BY state`,
		stateStrings(terminalJobStates))
	if err != nil {
		return nil, fmt.Errorf("job progress by state: %w", err)
	}
	defer rows.Close()

	var out []JobStateProgress
	for rows.Next() {
		var sp JobStateProgress
		var state string
		if err := rows.Scan(&state, &sp.Count, &sp.OldestUpdate); err != nil {
			return nil, fmt.Errorf("scan job progress: %w", err)
		}
		sp.State = core.AlbumJobState(state)
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("job progress by state: %w", err)
	}
	return out, nil
}

func (s *Store) countJobsWithoutActiveCandidate(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM album_jobs j
		  WHERE j.state = ANY($1)
		    AND NOT EXISTS (
		        SELECT 1 FROM candidates c
		         WHERE c.album_job_id = j.id AND c.state = $2)`,
		stateStrings(wedgeableJobStates), string(core.CandidateActive)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count jobs without active candidate: %w", err)
	}
	return count, nil
}

// stateStrings renders job states for a Postgres text-array parameter.
func stateStrings(states []core.AlbumJobState) []string {
	out := make([]string, len(states))
	for i, st := range states {
		out[i] = string(st)
	}
	return out
}
