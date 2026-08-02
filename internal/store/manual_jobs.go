// Package store: manual_jobs.go holds the write path for manually created jobs
// (POST /api/jobs, issue #155) — a user-supplied peer + file list that skips
// straight to DOWNLOADING, bypassing the Discovery/Selecting modules entirely.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// ErrRemoteFileBusy is returned by CreateManualJob when another live
// candidate (any state in idx_transfers_live_remote_owner: PENDING, QUEUED,
// IN_PROGRESS, STALLED) already owns one of the requested (peer, filename)
// pairs.
var ErrRemoteFileBusy = errors.New("remote file already claimed by another live candidate")

// ManualJobFile is one file the user asked to download directly from a known
// peer, as posted to POST /api/jobs.
type ManualJobFile struct {
	Filename string
	Size     int64
}

// CreateManualJob inserts a manual job directly in DOWNLOADING together with
// its ACTIVE candidate and complete PENDING transfer set, in one transaction,
// so the Downloading module picks it up with no SELECTING step. It does NOT
// take the activation advisory lock and does NOT enforce the MaxActive cap:
// a manual job is an explicit user request and is created unconditionally
// (issue #155). Its lidarr_album_id is NULL and source is 'manual'.
// WantedSync's predicates all filter on source = 'lidarr' explicitly (#369),
// so they never touch this row even if lidarr_album_id were ever non-NULL.
//
// albumMBID is the MusicBrainz release-group id the user identified the
// download against, or "" if they chose not to. It is the wire's only album
// identity for a manual job (see core.AlbumJob.AlbumMBID) - lidarr_album_id
// stays NULL for the job's whole life. Importing resolves albumMBID through
// AlbumByForeignID on each tick and never writes the answer back (issue #59).
//
// Returns ErrRemoteFileBusy if another live candidate already owns a (peer,
// filename) pair among files.
//
// Callers are expected to pre-validate files before calling: non-empty,
// unique non-blank filenames, and non-negative sizes. The HTTP handler
// (validateCreateJobRequest in internal/observ) does this.
func (s *Store) CreateManualJob(ctx context.Context, title, artistName, peer, albumMBID string, files []ManualJobFile, now time.Time) (core.AlbumJob, error) {
	candidateFiles := make([]core.CandidateFile, len(files))
	for i, f := range files {
		candidateFiles[i] = core.CandidateFile{Filename: f.Filename, Size: f.Size}
	}
	filesJSON, err := json.Marshal(candidateFiles)
	if err != nil {
		return core.AlbumJob{}, fmt.Errorf("marshal manual job files: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.AlbumJob{}, err
	}
	defer tx.Rollback()

	var jobID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO album_jobs (lidarr_album_id, source, state, created_at, updated_at, title, artist_name, album_mbid)
		 VALUES (NULL, $1, $2, $3, $3, $4, $5, $6)
		 RETURNING id`,
		string(core.SourceManual), string(core.StateDownloading), now, title, artistName, albumMBID).Scan(&jobID); err != nil {
		return core.AlbumJob{}, fmt.Errorf("insert manual job: %w", err)
	}

	var candidateID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO candidates (album_job_id, username, score, files, state, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $6)
		 RETURNING id`,
		jobID, peer, 0.0, filesJSON, string(core.CandidateActive), now).Scan(&candidateID); err != nil {
		return core.AlbumJob{}, fmt.Errorf("insert manual candidate: %w", err)
	}

	// Mirrors the pending-transfer insert in ActivateCandidateWithTransfers
	// (candidates.go) — keep the two in sync.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO transfers
		   (candidate_id, username, filename, state, bytes_total, deadline, updated_at)
		 SELECT $1, $2, f.value->>'filename', $3,
		        (f.value->>'size')::bigint, $4, $4
		   FROM jsonb_array_elements($5::jsonb) WITH ORDINALITY AS f(value, ord)
		  ORDER BY f.ord`,
		candidateID, peer, string(core.TransferPending), now, string(filesJSON)); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idx_transfers_live_remote_owner" {
			return core.AlbumJob{}, ErrRemoteFileBusy
		}
		return core.AlbumJob{}, fmt.Errorf("insert manual transfers: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return core.AlbumJob{}, fmt.Errorf("create manual job: commit: %w", err)
	}

	return core.AlbumJob{
		ID:         jobID,
		Source:     core.SourceManual,
		State:      core.StateDownloading,
		CreatedAt:  now,
		UpdatedAt:  now,
		Title:      title,
		ArtistName: artistName,
		AlbumMBID:  albumMBID,
	}, nil
}
