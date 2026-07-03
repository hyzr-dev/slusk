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

// jobViewSelect joins each album_job with its most recent candidate_attempts
// row (by created_at) and that attempt's most recent transfer (by
// updated_at), if any. A job with no attempts yet still appears, with NULL
// attempt/transfer columns. Callers append their own WHERE clause.
const jobViewSelect = `
	SELECT
		j.id, j.lidarr_album_id, j.state, j.candidates_tried, j.next_attempt_at, j.created_at, j.updated_at, j.title, j.artist_name,
		t.id, t.attempt_id, t.slskd_id, t.username, t.filename, t.state, t.bytes_done, t.bytes_total, t.deadline, t.last_progress_at, t.updated_at,
		a.id, a.album_job_id, a.username, a.score, a.state, a.fail_reason, a.backoff_until, a.created_at, a.updated_at
	FROM album_jobs j
	LEFT JOIN candidate_attempts a ON a.id = (
		SELECT id FROM candidate_attempts WHERE album_job_id = j.id ORDER BY created_at DESC LIMIT 1
	)
	LEFT JOIN transfers t ON t.id = (
		SELECT id FROM transfers WHERE attempt_id = a.id ORDER BY updated_at DESC LIMIT 1
	)`

func scanJobView(r rowScanner) (core.JobView, error) {
	var v core.JobView
	var jState string
	var tID sql.NullInt64
	var tAttemptID sql.NullInt64
	var tSlskdID, tUsername, tFilename, tState sql.NullString
	var tBytesDone, tBytesTotal sql.NullInt64
	var tDeadline, tLastProgressAt, tUpdatedAt sql.NullTime
	var aID, aAlbumJobID sql.NullInt64
	var aUsername, aState, aFailReason sql.NullString
	var aScore sql.NullFloat64
	var aBackoffUntil, aCreatedAt, aUpdatedAt sql.NullTime

	err := r.Scan(
		&v.Job.ID, &v.Job.LidarrAlbumID, &jState, &v.Job.CandidatesTried, &v.Job.NextAttemptAt, &v.Job.CreatedAt, &v.Job.UpdatedAt, &v.Job.Title, &v.Job.ArtistName,
		&tID, &tAttemptID, &tSlskdID, &tUsername, &tFilename, &tState, &tBytesDone, &tBytesTotal, &tDeadline, &tLastProgressAt, &tUpdatedAt,
		&aID, &aAlbumJobID, &aUsername, &aScore, &aState, &aFailReason, &aBackoffUntil, &aCreatedAt, &aUpdatedAt,
	)
	if err != nil {
		return core.JobView{}, err
	}
	v.Job.State = core.AlbumJobState(jState)

	if tID.Valid {
		tr := &core.Transfer{
			ID:         tID.Int64,
			AttemptID:  tAttemptID.Int64,
			SlskdID:    tSlskdID.String,
			Username:   tUsername.String,
			Filename:   tFilename.String,
			State:      core.TransferState(tState.String),
			BytesDone:  tBytesDone.Int64,
			BytesTotal: tBytesTotal.Int64,
			Deadline:   tDeadline.Time,
			UpdatedAt:  tUpdatedAt.Time,
		}
		if tLastProgressAt.Valid {
			lp := tLastProgressAt.Time
			tr.LastProgressAt = &lp
		}
		v.Transfer = tr
		v.Peer = tUsername.String
	}

	if aID.Valid {
		att := &core.CandidateAttempt{
			ID:         aID.Int64,
			AlbumJobID: aAlbumJobID.Int64,
			Username:   aUsername.String,
			Score:      aScore.Float64,
			State:      aState.String,
			FailReason: aFailReason.String,
			CreatedAt:  aCreatedAt.Time,
			UpdatedAt:  aUpdatedAt.Time,
		}
		if aBackoffUntil.Valid {
			b := aBackoffUntil.Time
			att.BackoffUntil = &b
		}
		v.Attempt = att
	}
	return v, nil
}

// ListJobsWithTransfer returns every non-cancelled album job joined with its
// most recent transfer, newest job first. Used by the dashboard's Queue view.
func (s *Store) ListJobsWithTransfer(ctx context.Context) ([]core.JobView, error) {
	rows, err := s.db.QueryContext(ctx, jobViewSelect+` WHERE j.state != ? ORDER BY j.updated_at DESC`, string(core.StateCancelled))
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
	row := s.db.QueryRowContext(ctx, jobViewSelect+` WHERE j.id = ?`, jobID)

	v, err := scanJobView(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.JobView{}, false, nil
	}
	if err != nil {
		return core.JobView{}, false, fmt.Errorf("job with transfer: %w", err)
	}
	return v, true, nil
}
