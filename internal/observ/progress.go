// Package observ: progress.go carries the job-progress reading (issue #442)
// from the store to /metrics and /status. It measures the *work*, where every
// other health signal here measures a *module*: a module whose tick returns
// nil while advancing nothing is indistinguishable from a healthy one, which is
// how three DOWNLOADING jobs sat untouched for four weeks behind a green
// /status.
//
// observ declares its own transport types rather than importing store, as it
// does for the soulseek client; cmd/slusk adapts between them.
package observ

import (
	"context"
	"time"
)

// JobProgressState is one non-terminal job state's staleness reading.
type JobProgressState struct {
	// State is the job state, carried as a plain string because it becomes a
	// Prometheus label and a JSON field, not a domain decision.
	State string
	// Count is how many jobs sit in it.
	Count int
	// OldestUpdateAge is how long ago the least recently updated of them was
	// touched. Exported raw rather than as an "is stale" boolean: the threshold
	// belongs to whoever alerts on it, and baking one in here would mean a new
	// required config key, which stops other people's containers from starting
	// on the next deploy.
	OldestUpdateAge time.Duration
}

// JobProgressReport is a whole snapshot of how recently the pipeline touched
// the work it still owns.
type JobProgressReport struct {
	// States holds one entry per non-terminal state that currently has jobs. A
	// state with no jobs is absent rather than zero: absence reads as "nothing
	// here", where a zero age would read as "everything is fresh".
	States []JobProgressState
	// JobsWithoutActiveCandidate counts jobs in DOWNLOADING or IMPORTING with
	// no ACTIVE candidate - the wedge shape from issue #442, in which such a
	// job holds a slot indefinitely while both modules skip it silently.
	JobsWithoutActiveCandidate int
}

// JobProgressFunc produces a current JobProgressReport (typically backed by the
// store).
type JobProgressFunc func(ctx context.Context) (JobProgressReport, error)

// SetJobProgress republishes the job-progress gauges.
//
// The two labelled vectors are reset before every publish rather than updated
// in place. A label set whose jobs have all left must stop being reported
// altogether: a gauge still claiming a month-old age for an empty state is
// worse than no gauge, because it reads as an unattended wedge that no longer
// exists. Reset is safe here because this is the only writer and it always
// supplies the complete set.
func (m *Metrics) SetJobProgress(r JobProgressReport) {
	m.JobsInState.Reset()
	m.JobOldestUpdateAge.Reset()
	for _, s := range r.States {
		m.JobsInState.WithLabelValues(s.State).Set(float64(s.Count))
		m.JobOldestUpdateAge.WithLabelValues(s.State).Set(s.OldestUpdateAge.Seconds())
	}
	m.JobsWithoutActiveCandidate.Set(float64(r.JobsWithoutActiveCandidate))
}

// jobProgressStateDTO is one row of the /status job-progress block.
type jobProgressStateDTO struct {
	State                  string `json:"state"`
	Count                  int    `json:"count"`
	OldestUpdateAgeSeconds int    `json:"oldestUpdateAgeSeconds"`
}

// jobProgressDTO is the /status job-progress block.
type jobProgressDTO struct {
	States                     []jobProgressStateDTO `json:"states"`
	JobsWithoutActiveCandidate int                   `json:"jobsWithoutActiveCandidate"`
}

// newJobProgressDTO renders a report for /status. States is always a non-nil
// slice so the field serializes as [] rather than null.
func newJobProgressDTO(r JobProgressReport) jobProgressDTO {
	states := make([]jobProgressStateDTO, 0, len(r.States))
	for _, s := range r.States {
		states = append(states, jobProgressStateDTO{
			State:                  s.State,
			Count:                  s.Count,
			OldestUpdateAgeSeconds: int(s.OldestUpdateAge.Seconds()),
		})
	}
	return jobProgressDTO{States: states, JobsWithoutActiveCandidate: r.JobsWithoutActiveCandidate}
}
