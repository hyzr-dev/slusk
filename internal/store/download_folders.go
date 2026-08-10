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
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
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

// downloadFolderLockNamespace is the first half of the two-int advisory lock
// key RegisterDownloadFolder takes; the second half is a hash of the folder
// name, so two jobs reserving different folders never queue behind each other.
// Kept distinct from activationLockKey's namespace (candidates.go) — sharing
// one would serialize two unrelated decisions.
const downloadFolderLockNamespace int32 = 0x646c6664 // "dlfd"

// RegisterDownloadFolder claims the local folder named leaf, directly below the
// download root, for jobID's downloads. It returns (0, true, nil) on success
// and (ownerJobID, false, nil) when another job already owns that folder.
//
// Neither backend lets slusk choose the local directory: both derive it from
// the peer's share path, so two jobs whose peers happen to share a directory
// name would otherwise write into one directory at the same time, and the
// recursive DeleteDownloadFolder that cleans up after one of them would take
// the other's files with it (issue #471). Ownership is what makes the folder a
// job's to write into and, later, to delete.
//
// An owner is an uncleaned row whose job is in DOWNLOADING or IMPORTING. The
// row answers two different questions and only one of them is state-dependent:
// cleanup asks "may this folder hold my bytes" of every uncleaned row whatever
// the job's state, because ResetJobToWanted deliberately keeps those rows
// (#314) and a stranded folder is exactly what the register exists to name.
// Ownership asks "is someone writing there right now", which only those two
// states can be true of. Reading uncleaned alone would let a job that finished
// months ago hold a common folder name forever.
//
// Registration is idempotent per (job, leaf) compared case-insensitively: a
// candidate writes many files into one folder and every one of them registers,
// and two casings of one directory are one folder, not two (issue #479). A job
// re-registering a folder it already holds is a success, not a conflict.
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
func (s *Store) RegisterDownloadFolder(ctx context.Context, jobID int64, leaf string, now time.Time) (int64, bool, error) {
	if err := validDownloadLeaf(leaf); err != nil {
		return 0, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("register download folder: %w", err)
	}
	defer tx.Rollback()

	// Every transaction in this package runs at READ COMMITTED, so a lookup
	// followed by an INSERT does not serialize on its own: two jobs would both
	// see the folder free and both claim it. The advisory lock is the same
	// answer ActivateCandidateWithTransfers uses for the same problem, and is
	// why the concurrency test can assert exactly one winner.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock($1, hashtext(lower($2)))`,
		downloadFolderLockNamespace, leaf); err != nil {
		return 0, false, fmt.Errorf("lock download folder: %w", err)
	}

	var owner int64
	err = tx.QueryRowContext(ctx,
		`SELECT f.album_job_id FROM job_download_folders f
		 JOIN album_jobs j ON j.id = f.album_job_id
		 WHERE lower(f.leaf) = lower($1)
		   AND f.cleaned_at IS NULL
		   AND f.album_job_id <> $2
		   AND j.state IN ($3, $4)
		 ORDER BY f.album_job_id
		 LIMIT 1`,
		leaf, jobID, string(core.StateDownloading), string(core.StateImporting)).Scan(&owner)
	switch {
	case err == nil:
		return owner, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, fmt.Errorf("register download folder: owner lookup: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_download_folders (album_job_id, leaf, created_at) VALUES ($1, $2, $3)
		 ON CONFLICT (album_job_id, lower(leaf))
		 DO UPDATE SET cleaned_at = NULL, created_at = $3
		 WHERE job_download_folders.cleaned_at IS NOT NULL`,
		jobID, leaf, now); err != nil {
		return 0, false, fmt.Errorf("register download folder: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("register download folder: %w", err)
	}
	return 0, true, nil
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
