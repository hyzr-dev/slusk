// Package store: download_folders.go holds the job_download_folders register
// (issue #314) — the record of which local folder a job's downloads were
// actually written into, written at download time rather than reconstructed
// afterwards from surviving transfer rows.
//
// The register's lifetime is the job's, not the search cycle's:
// ResetJobToWanted and ForceSearchJob delete a job's candidates and transfers,
// which is exactly what used to strand earlier cycles' folders on disk with no
// way left to name them. They deliberately do not touch this table; only
// DeleteJob does. See migrations/0015_job_download_folders.sql.
package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidDownloadLeaf marks a folder name that is not exactly one path
// element below the download root. The table has the same rule as a CHECK
// constraint, but that is the last line of defence, not the first: a constraint
// violation surfacing mid-download is a far worse signal than an early reject,
// and the register is the input to a recursive delete.
var ErrInvalidDownloadLeaf = fmt.Errorf("store: download folder must be a single path element")

// validDownloadLeaf mirrors job_download_folders_leaf_is_one_element. It is
// deliberately a whole-string rule rather than a sanitizer: a leaf that needs
// cleaning is a leaf we do not understand, and guessing is how a delete ends up
// pointed somewhere it was never meant to go.
func validDownloadLeaf(leaf string) error {
	if leaf == "" || leaf == "." || leaf == ".." ||
		strings.ContainsAny(leaf, `/\`) {
		return fmt.Errorf("%w: %q", ErrInvalidDownloadLeaf, leaf)
	}
	return nil
}

// RegisterDownloadFolder records that jobID's downloads are being written into
// the local folder named leaf, directly below the download root. It is
// idempotent per (job, leaf) compared case-insensitively: a candidate writes
// many files into one folder and every one of them registers, and two casings
// of one directory are one folder, not two (issue #479).
//
// The row keeps the casing it was first registered under. A later registration
// of the same folder under a different casing updates nothing but cleaned_at:
// the stored value is an address at the backend — handed verbatim to
// DeleteDownloadFolder, which base64-encodes it for slskd — so rewriting it
// would repoint a recursive delete at a name no one has seen on disk.
//
// A re-registration clears cleaned_at rather than doing nothing. That case is
// real: a candidate fails, its folder is deleted and marked cleaned, and a
// later candidate for the same job downloads from a peer whose remote folder
// happens to carry the same name. Leaving the row marked cleaned would hide
// that folder from every later cleanup — the exact leak this register exists to
// close.
func (s *Store) RegisterDownloadFolder(ctx context.Context, jobID int64, leaf string, now time.Time) error {
	if err := validDownloadLeaf(leaf); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO job_download_folders (album_job_id, leaf, created_at) VALUES ($1, $2, $3)
		 ON CONFLICT (album_job_id, lower(leaf))
		 DO UPDATE SET cleaned_at = NULL, created_at = $3
		 WHERE job_download_folders.cleaned_at IS NOT NULL`,
		jobID, leaf, now)
	if err != nil {
		return fmt.Errorf("register download folder: %w", err)
	}
	return nil
}

// DownloadFoldersForJob returns the folders jobID has written to and that have
// not been cleaned up yet, oldest first. An empty result means the job has
// written nothing anywhere — not that cleanup should be skipped quietly, which
// is how the derived-path version used to lose track of files.
func (s *Store) DownloadFoldersForJob(ctx context.Context, jobID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT leaf FROM job_download_folders
		 WHERE album_job_id = $1 AND cleaned_at IS NULL
		 ORDER BY created_at, id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("download folders for job: %w", err)
	}
	defer rows.Close()

	var leaves []string
	for rows.Next() {
		var leaf string
		if err := rows.Scan(&leaf); err != nil {
			return nil, fmt.Errorf("download folders for job: scan: %w", err)
		}
		leaves = append(leaves, leaf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("download folders for job: %w", err)
	}
	return leaves, nil
}

// MarkDownloadFolderCleaned stamps one registered folder as no longer on disk,
// so later cleanups stop asking about it. The row is kept rather than deleted:
// it is the record that the folder existed and was dealt with.
//
// leaf is matched case-insensitively, the same comparison that decides whether
// two registrations are one folder (issue #479). Matching exactly here while
// registering case-insensitively would leave a caller holding a leaf whose
// casing no longer matches the stored row unable to stamp it at all — an
// uncleaned row for a folder that is already gone, which is the defect this
// issue is about.
func (s *Store) MarkDownloadFolderCleaned(ctx context.Context, jobID int64, leaf string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE job_download_folders SET cleaned_at = $1
		 WHERE album_job_id = $2 AND lower(leaf) = lower($3) AND cleaned_at IS NULL`,
		now, jobID, leaf)
	if err != nil {
		return fmt.Errorf("mark download folder cleaned: %w", err)
	}
	return nil
}
