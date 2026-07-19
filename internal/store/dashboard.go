// Package store: dashboard.go holds read-only projections used by the web
// dashboard (internal/observ). Nothing here mutates state.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// jobViewSelect joins each album_job with its most recently created candidate
// row and that candidate's most recent transfer (by updated_at), if any. A
// job with no candidates yet still appears, with NULL candidate/transfer
// columns. Callers append their own WHERE clause.
const jobViewSelect = `
	SELECT
		j.id, j.lidarr_album_id, j.state, j.candidates_tried, j.next_attempt_at, j.created_at, j.updated_at, j.title, j.artist_name, j.retries, j.not_before, j.failed_at,
		t.id, t.candidate_id, t.slskd_id, t.username, t.filename, t.state, t.bytes_done, t.bytes_total, t.deadline, t.last_progress_at, t.updated_at,
		a.id, a.album_job_id, a.username, a.score, a.state, a.fail_reason, a.created_at, a.updated_at
	FROM album_jobs j
	LEFT JOIN candidates a ON a.id = (
		SELECT id FROM candidates WHERE album_job_id = j.id ORDER BY created_at DESC LIMIT 1
	)
	LEFT JOIN transfers t ON t.id = (
		SELECT id FROM transfers WHERE candidate_id = a.id ORDER BY updated_at DESC LIMIT 1
	)`

func scanJobView(r rowScanner) (core.JobView, error) {
	var v core.JobView
	var jState string
	var tID sql.NullInt64
	var tCandidateID sql.NullInt64
	var tSlskdID, tUsername, tFilename, tState sql.NullString
	var tBytesDone, tBytesTotal sql.NullInt64
	var tDeadline, tLastProgressAt, tUpdatedAt sql.NullTime
	var aID, aAlbumJobID sql.NullInt64
	var aUsername, aState, aFailReason sql.NullString
	var aScore sql.NullFloat64
	var aCreatedAt, aUpdatedAt sql.NullTime

	err := r.Scan(
		&v.Job.ID, &v.Job.LidarrAlbumID, &jState, &v.Job.CandidatesTried, &v.Job.NextAttemptAt, &v.Job.CreatedAt, &v.Job.UpdatedAt, &v.Job.Title, &v.Job.ArtistName, &v.Job.Retries, &v.Job.NotBefore, &v.Job.FailedAt,
		&tID, &tCandidateID, &tSlskdID, &tUsername, &tFilename, &tState, &tBytesDone, &tBytesTotal, &tDeadline, &tLastProgressAt, &tUpdatedAt,
		&aID, &aAlbumJobID, &aUsername, &aScore, &aState, &aFailReason, &aCreatedAt, &aUpdatedAt,
	)
	if err != nil {
		return core.JobView{}, err
	}
	v.Job.State = core.AlbumJobState(jState)

	if tID.Valid {
		tr := &core.Transfer{
			ID:          tID.Int64,
			CandidateID: tCandidateID.Int64,
			SlskdID:     tSlskdID.String,
			Username:    tUsername.String,
			Filename:    tFilename.String,
			State:       core.TransferState(tState.String),
			BytesDone:   tBytesDone.Int64,
			BytesTotal:  tBytesTotal.Int64,
			Deadline:    tDeadline.Time,
			UpdatedAt:   tUpdatedAt.Time,
		}
		if tLastProgressAt.Valid {
			lp := tLastProgressAt.Time
			tr.LastProgressAt = &lp
		}
		v.Transfer = tr
		v.Peer = tUsername.String
	}

	if aID.Valid {
		v.Attempt = &core.Candidate{
			ID:         aID.Int64,
			AlbumJobID: aAlbumJobID.Int64,
			Username:   aUsername.String,
			Score:      aScore.Float64,
			State:      core.CandidateState(aState.String),
			FailReason: aFailReason.String,
			CreatedAt:  aCreatedAt.Time,
			UpdatedAt:  aUpdatedAt.Time,
		}
	}
	return v, nil
}

// ListJobsWithTransfer returns every non-cancelled album job joined with its
// most recent transfer, newest job first. Used by the dashboard's Queue view.
func (s *Store) ListJobsWithTransfer(ctx context.Context) ([]core.JobView, error) {
	rows, err := s.db.QueryContext(ctx, jobViewSelect+` WHERE j.state != $1 ORDER BY j.updated_at DESC`, string(core.StateCancelled))
	if err != nil {
		return nil, fmt.Errorf("list jobs with transfer: %w", err)
	}
	defer rows.Close()

	var out []core.JobView
	for rows.Next() {
		v, err := scanJobView(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job view: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// JobWithTransfer looks up a single job (regardless of state) with its most
// recent transfer, for the cancel endpoint. found is false if no job has
// that id.
func (s *Store) JobWithTransfer(ctx context.Context, jobID int64) (core.JobView, bool, error) {
	row := s.db.QueryRowContext(ctx, jobViewSelect+` WHERE j.id = $1`, jobID)

	v, err := scanJobView(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.JobView{}, false, nil
	}
	if err != nil {
		return core.JobView{}, false, fmt.Errorf("job with transfer: %w", err)
	}
	return v, true, nil
}

// JobDetail returns a job plus every candidate made for it (newest first) and
// each candidate's per-file transfers, for the dashboard's per-job detail
// panel (GET /api/jobs/{id}/detail). found is false if no job has that id.
// Built from CandidatesForJob/TransfersForCandidate (one query per candidate)
// rather than a single join, since the number of candidates per job is small
// (bounded by MaxCandidatesPerAlbum) and this reuses the existing read paths
// rather than a bespoke wide query.
func (s *Store) JobDetail(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
	var job core.AlbumJob
	var state string
	err := s.db.QueryRowContext(ctx,
		jobSelect+` WHERE id = $1`, jobID).
		Scan(&job.ID, &job.LidarrAlbumID, &state, &job.CandidatesTried, &job.NextAttemptAt, &job.CreatedAt, &job.UpdatedAt, &job.Title, &job.ArtistName, &job.ReleaseDate, &job.ArtistID, &job.Retries, &job.NotBefore, &job.FailedAt, &job.MinTrackCount, &job.MaxTrackCount)
	if errors.Is(err, sql.ErrNoRows) {
		return core.JobDetail{}, false, nil
	}
	if err != nil {
		return core.JobDetail{}, false, fmt.Errorf("job detail: read job: %w", err)
	}
	job.State = core.AlbumJobState(state)

	candidates, err := s.CandidatesForJob(ctx, jobID) // oldest first
	if err != nil {
		return core.JobDetail{}, false, fmt.Errorf("job detail: candidates: %w", err)
	}
	details := make([]core.AttemptDetail, len(candidates))
	for i, c := range candidates {
		transfers, err := s.TransfersForCandidate(ctx, c.ID)
		if err != nil {
			return core.JobDetail{}, false, fmt.Errorf("job detail: transfers for candidate %d: %w", c.ID, err)
		}
		details[i] = core.AttemptDetail{Attempt: c, Transfers: transfers}
	}
	// CandidatesForJob returns oldest first; the detail panel wants newest first.
	for i, j := 0, len(details)-1; i < j; i, j = i+1, j-1 {
		details[i], details[j] = details[j], details[i]
	}
	return core.JobDetail{Job: job, Attempts: details}, true, nil
}

// Peers returns every known Soulseek peer's global reliability plus their
// per-artist rows, for the dashboard's Peers view (GET /api/peers). Ordered by
// username for determinism; the dashboard sorts client-side. Score computation
// (which needs "now" for decay) is left to the caller — see
// matcher.ReliabilityHistoryScore — so this stays a plain read.
func (s *Store) Peers(ctx context.Context) ([]core.PeerRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT username, success_count, fail_count, last_success_at, last_fail_at
		 FROM known_users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("peers: query known_users: %w", err)
	}
	defer rows.Close()

	byUsername := map[string]*core.PeerRow{}
	var order []string
	for rows.Next() {
		var p core.PeerRow
		if err := rows.Scan(&p.Username, &p.Global.SuccessCount, &p.Global.FailCount, &p.Global.LastSuccessAt, &p.Global.LastFailAt); err != nil {
			return nil, fmt.Errorf("peers: scan known_users: %w", err)
		}
		byUsername[p.Username] = &p
		order = append(order, p.Username)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	artistRows, err := s.db.QueryContext(ctx,
		`SELECT ku.username, aur.artist_id, aur.success_count, aur.fail_count, aur.last_success_at, aur.last_fail_at
		 FROM artist_user_reliability aur JOIN known_users ku ON ku.id = aur.user_id`)
	if err != nil {
		return nil, fmt.Errorf("peers: query artist_user_reliability: %w", err)
	}
	defer artistRows.Close()
	for artistRows.Next() {
		var username string
		var artistID int64
		var c core.ReliabilityCounters
		if err := artistRows.Scan(&username, &artistID, &c.SuccessCount, &c.FailCount, &c.LastSuccessAt, &c.LastFailAt); err != nil {
			return nil, fmt.Errorf("peers: scan artist_user_reliability: %w", err)
		}
		p, ok := byUsername[username]
		if !ok {
			continue
		}
		if p.Artists == nil {
			p.Artists = map[int64]core.ReliabilityCounters{}
		}
		p.Artists[artistID] = c
	}
	if err := artistRows.Err(); err != nil {
		return nil, err
	}

	out := make([]core.PeerRow, 0, len(order))
	for _, u := range order {
		out = append(out, *byUsername[u])
	}
	return out, nil
}
