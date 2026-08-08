package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// FolderCleaner is the minimal slice of PeerSearcher that cleanupFolder needs:
// the single slskd "delete a completed-download subfolder" call. Kept as its
// own tiny interface (rather than taking a full PeerSearcher) so cleanupFolder
// is a clean, reusable free function both Downloading (task 9) and Importing
// (task 10) can call with only the capability it actually uses.
type FolderCleaner interface {
	DeleteDownloadFolder(ctx context.Context, name string) error
}

// DownloadFolderRegistry is the slice of the store the cleanup helpers need:
// the record of which local folders a job actually downloaded into, written at
// download time (see internal/store/download_folders.go, issue #314). Kept as
// its own small interface, like FolderCleaner, so cleanupFolder and
// cleanupCompletedFolder stay free functions that Downloading, Importing and
// Selecting can each call with only the capability they use.
type DownloadFolderRegistry interface {
	DownloadFoldersForJob(ctx context.Context, jobID int64) ([]string, error)
	MarkDownloadFolderCleaned(ctx context.Context, jobID int64, leaf string, now time.Time) error
}

// cleanupFolder best-effort deletes a job's leftover downloaded files from the
// downloads root, so they don't get mixed into the next candidate's local
// folder (both backends name local subfolders after the remote peer's own leaf
// directory name, so two different peers sharing an identically-named folder
// can otherwise collide, corrupting Lidarr's later import scan).
//
// The folders come from the register rather than being re-derived from the
// candidate's surviving transfer filenames (issue #314). The derivation was
// silent wherever it failed — no filenames left to look at, or filenames not
// sharing one directory — and silence is what let files accumulate on disk with
// nothing left in the database able to name them. The register is also
// per-job, not per-candidate, so a job whose earlier search cycles had their
// transfers deleted by ResetJobToWanted still cleans those cycles up here.
//
// It refuses to act on the quarantine directory itself (see quarantineDirName),
// which a peer's remote folder can legitimately be named: DeleteDownloadFolder
// is recursive on both backends, so one such peer would otherwise wipe every
// album quarantined so far. A delete failure is logged and otherwise ignored —
// it must not block the job from moving on to its next candidate. A 404 means
// the folder is already gone (e.g. the candidate failed before any transfer
// wrote bytes), which is routine, so it's logged quietly rather than as an
// ERROR.
//
// Only a genuine delete stamps cleaned_at — a 404 deliberately does not. The
// slskd adapter maps *every* 404 to core.ErrRemoteNotFound, including one
// caused by a wrong base URL or a moved endpoint, so a 404 is not evidence that
// the folder is gone. Stamping on it would let a misconfigured backend mark
// every job's folders cleaned while the files sit on disk, unreachable by any
// later cleanup — issue #314's own defect, reintroduced by its fix. Leaving the
// row is cheap: the next cleanup asks again, and DeleteJob drops it.
//
// Ported from the legacy engine's Discoverer.cleanupAttempt
// (engine/discovery.go:812-825) as a shared free function so both Downloading
// and Importing reuse it.
func cleanupFolder(ctx context.Context, peers FolderCleaner, reg DownloadFolderRegistry, log *slog.Logger, jobID int64, now time.Time) {
	leaves, err := registeredFolders(ctx, reg, log, jobID, "cleanup")
	if err != nil {
		return
	}
	for _, leaf := range leaves {
		// A peer's remote folder can legitimately be named ".failed"; deleting it
		// would take every already-quarantined album with it, since both backends'
		// DeleteDownloadFolder is recursive. Same guard, same reason, as
		// quarantineFolder's.
		if leaf == quarantineDirName {
			log.Info("skipping cleanup of a folder named like the quarantine dir", "album_job", jobID, "folder", leaf)
			continue
		}
		switch err := peers.DeleteDownloadFolder(ctx, leaf); {
		case err == nil:
			markCleaned(ctx, reg, log, jobID, leaf, now)
		case errors.Is(err, core.ErrRemoteNotFound):
			log.Info("nothing to clean up for failed attempt", "album_job", jobID, "folder", leaf)
		default:
			log.Error("cleanup failed attempt's downloaded files failed", "album_job", jobID, "folder", leaf, "err", err)
		}
	}
}

// registeredFolders reads a job's uncleaned download folders and, crucially,
// says so out loud when there are none. The defect this replaces was not that
// the old derivation sometimes failed but that it failed *silently*: no log
// line, no job_event, and a folder left on disk that nothing could name.
func registeredFolders(ctx context.Context, reg DownloadFolderRegistry, log *slog.Logger, jobID int64, purpose string) ([]string, error) {
	leaves, err := reg.DownloadFoldersForJob(ctx, jobID)
	if err != nil {
		log.Error("list registered download folders failed", "album_job", jobID, "purpose", purpose, "err", err)
		return nil, err
	}
	if len(leaves) == 0 {
		log.Info("no registered download folder", "album_job", jobID, "purpose", purpose)
	}
	return leaves, nil
}

// markCleaned records that a registered folder is gone, best-effort: failing to
// stamp it only means a later cleanup asks about it again, which is harmless.
func markCleaned(ctx context.Context, reg DownloadFolderRegistry, log *slog.Logger, jobID int64, leaf string, now time.Time) {
	if err := reg.MarkDownloadFolderCleaned(ctx, jobID, leaf, now); err != nil {
		log.Error("mark download folder cleaned failed", "album_job", jobID, "folder", leaf, "err", err)
	}
}

// AlbumFolder computes the local folder Lidarr should scan for one album, from
// the downloaded transfers' filenames. filenames are the remote peer's full
// share paths (slskd uses "\" separators and mirrors the peer's own directory
// layout, e.g. "Music\B\Artist\Album\track.flac"); slskd itself only recreates
// the leaf directory locally under completeDir, discarding the remote peer's
// parent folders. So only the common directory's base name is joined under
// completeDir. If there is no single common directory, it falls back to
// completeDir so Lidarr scans the whole download root.
func AlbumFolder(completeDir string, filenames []string) string {
	leaf := commonLeaf(filenames)
	if leaf == "" {
		return completeDir
	}
	return path.Join(completeDir, leaf)
}

// cleanupCompletedFolder best-effort removes the per-album folders the backend
// created under completeDir once Lidarr has confirmed the import, so a
// completed job doesn't leave empty leftover directories behind forever. The
// folders come from the register (issue #314) for the same reason cleanupFolder
// takes them from there.
//
// Unlike cleanupFolder (used for a failed/rejected candidate via the backends'
// recursive DeleteDownloadFolder API), this only ever removes an
// already-verified-empty directory with os.Remove: if anything remains
// (e.g. a partial import), os.Remove fails safely rather than deleting real
// files, and that failure is only logged - it must never block the job's
// own DONE transition, which has already committed by the time this runs.
//
// Every branch that reaches a verdict stamps cleaned_at, including the one that
// deliberately leaves a non-empty folder alone. The import has succeeded, so
// that decision is final: the unmatched extras Importing chose to keep are not
// leftovers, and leaving the row uncleaned would hand them to cleanupFolder's
// recursive delete if the same job were ever revived. Local os.ReadDir evidence
// is trusted here in a way cleanupFolder cannot trust a remote 404.
func cleanupCompletedFolder(ctx context.Context, reg DownloadFolderRegistry, log *slog.Logger, jobID int64, completeDir string, now time.Time) {
	leaves, err := registeredFolders(ctx, reg, log, jobID, "completed")
	if err != nil {
		return
	}
	for _, leaf := range leaves {
		// The same guard cleanupFolder and quarantineFolder carry: a peer's
		// remote folder can legitimately be named ".failed", and the quarantine
		// directory is not this job's to remove even when it happens to be empty.
		if leaf == quarantineDirName {
			log.Info("skipping completed-folder cleanup of a folder named like the quarantine dir", "album_job", jobID, "folder", leaf)
			continue
		}
		folder := filepath.Join(completeDir, leaf)
		entries, err := os.ReadDir(folder)
		switch {
		case err == nil:
			if len(entries) > 0 {
				log.Info("completed album folder not empty, leaving in place", "album_job", jobID, "folder", folder, "entries", len(entries))
				markCleaned(ctx, reg, log, jobID, leaf, now)
				continue
			}
			if err := os.Remove(folder); err != nil {
				log.Error("remove completed album folder failed", "album_job", jobID, "folder", folder, "err", err)
				continue
			}
			log.Info("removed empty completed album folder", "album_job", jobID, "folder", folder)
		case os.IsNotExist(err):
			log.Info("completed album folder already gone", "album_job", jobID, "folder", folder)
		default:
			log.Error("read completed album folder failed", "album_job", jobID, "folder", folder, "err", err)
			continue
		}
		markCleaned(ctx, reg, log, jobID, leaf, now)
	}
}

// quarantineDirName is the fixed subdirectory of the download root that
// terminally FAILED jobs' leftover folders are moved into. It is deliberately
// not configurable: see quarantineFolder for why it has to live inside the
// download root.
const quarantineDirName = ".failed"

// quarantineFolder moves one leftover album folder out of completeDir and into
// completeDir/.failed, so a job that has exhausted its retry budget doesn't
// strand its partial download in the root Lidarr and the next job both scan.
// It returns the destination and whether anything actually moved; the common
// case is that there is nothing left to move, because cleanupFolder already
// removed the folder when the last candidate failed.
//
// The destination is a fixed subdirectory of completeDir rather than a
// configurable path for two reasons: a configurable path would be a new
// required config key (which must exist in production's config.toml before
// this deploys), and keeping source and destination under the same parent
// makes a cross-device os.Rename essentially unreachable, so no copy+delete
// fallback is needed - a half-copied album would be worse than today's
// behaviour of leaving it in place.
//
// Nothing here can block the job's FAILED transition, which has already
// committed by the time this runs: every failure path logs and returns, the
// same contract as cleanupCompletedFolder.
func quarantineFolder(log *slog.Logger, jobID int64, completeDir, leaf string) (string, bool) {
	if completeDir == "" || leaf == "" {
		return "", false
	}
	// A peer's remote folder can legitimately be named ".failed"; moving the
	// quarantine directory into itself is never what was meant.
	if leaf == quarantineDirName {
		log.Info("skipping quarantine of a folder named like the quarantine dir", "album_job", jobID, "folder", leaf)
		return "", false
	}

	src := filepath.Join(completeDir, leaf)
	// Belt and braces over commonLeaf's own ".."/"." rejection: refuse anything
	// that is not exactly one element below the download root (mirrors
	// soulseek.pathWithinRoot).
	rel, err := filepath.Rel(completeDir, src)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.ContainsRune(rel, filepath.Separator) {
		log.Error("refusing to quarantine a folder outside the download root", "album_job", jobID, "folder", src)
		return "", false
	}
	if filepath.Clean(src) == filepath.Clean(completeDir) {
		log.Error("refusing to quarantine the download root itself", "album_job", jobID, "folder", src)
		return "", false
	}

	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			log.Info("nothing to quarantine", "album_job", jobID, "folder", src)
		} else {
			log.Error("stat leftover folder failed", "album_job", jobID, "folder", src, "err", err)
		}
		return "", false
	}

	dstRoot := filepath.Join(completeDir, quarantineDirName)
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		log.Error("create quarantine directory failed", "album_job", jobID, "dir", dstRoot, "err", err)
		return "", false
	}

	// os.Rename's directory semantics are not safe to lean on here: on Linux it
	// silently replaces an EMPTY destination directory and fails with ENOTEMPTY
	// otherwise, so a collision is resolved by picking a free name up front.
	// The Lstat->Rename race is not a concern: Selecting.Tick is the only
	// writer (one goroutine per module, see runner.go).
	dst, ok := freeQuarantinePath(dstRoot, leaf, jobID)
	if !ok {
		log.Error("no free quarantine destination name", "album_job", jobID, "folder", src, "dir", dstRoot)
		return "", false
	}

	if err := os.Rename(src, dst); err != nil {
		var linkErr *os.LinkError
		if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
			// Should be unreachable given dst is a sibling subdirectory of src.
			// If it ever happens, leave the files exactly where they are rather
			// than risk a half-copied album.
			log.Error("quarantine would cross a filesystem boundary, leaving files in place",
				"album_job", jobID, "from", src, "to", dst)
			return "", false
		}
		log.Error("quarantine leftover folder failed", "album_job", jobID, "from", src, "to", dst, "err", err)
		return "", false
	}
	log.Info("quarantined leftover files from failed job", "album_job", jobID, "from", src, "to", dst)
	return dst, true
}

// freeQuarantinePath picks the first unused destination under dstRoot: the
// leaf itself, then the leaf suffixed with the job id, then additionally with
// the current unix second. It gives up rather than looping unboundedly - three
// collisions in a row means something other than this code is writing there.
func freeQuarantinePath(dstRoot, leaf string, jobID int64) (string, bool) {
	candidates := []string{
		leaf,
		fmt.Sprintf("%s.job%d", leaf, jobID),
		fmt.Sprintf("%s.job%d.%d", leaf, jobID, time.Now().Unix()),
	}
	for _, name := range candidates {
		p := filepath.Join(dstRoot, name)
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			return p, true
		}
	}
	return "", false
}

// commonLeaf returns the base name of filenames' single common directory, or
// "" if there isn't one (empty, no filenames, or the files don't share one
// directory) — ambiguous, unsafe to treat as a single album folder for
// either import-scanning or later cleanup.
func commonLeaf(filenames []string) string {
	if len(filenames) == 0 {
		return ""
	}
	dir := func(f string) string {
		f = strings.ReplaceAll(f, `\`, "/")
		return path.Dir(f)
	}
	common := dir(filenames[0])
	for _, f := range filenames[1:] {
		if dir(f) != common {
			return "" // no single album folder
		}
	}
	if common == "." || common == "/" || common == "" {
		return ""
	}
	// A remote share path whose common directory is (or ends in) ".." — e.g. a
	// hostile peer naming every file `..\track.flac` — yields a ".." leaf. That
	// is never a real album folder, and it feeds both AlbumFolder (the local
	// scan path) and the native backend's DeleteDownloadFolder (os.RemoveAll),
	// where a ".." would escape the download root. Reject it as ambiguous.
	leaf := path.Base(common)
	if leaf == ".." || leaf == "." {
		return ""
	}
	return leaf
}
