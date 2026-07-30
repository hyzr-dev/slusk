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

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// FolderCleaner is the minimal slice of PeerSearcher that cleanupFolder needs:
// the single slskd "delete a completed-download subfolder" call. Kept as its
// own tiny interface (rather than taking a full PeerSearcher) so cleanupFolder
// is a clean, reusable free function both Downloading (task 9) and Importing
// (task 10) can call with only the capability it actually uses.
type FolderCleaner interface {
	DeleteDownloadFolder(ctx context.Context, name string) error
}

// cleanupFolder best-effort deletes a failed candidate's leftover files from
// slskd's downloads root, so they don't get mixed into the next candidate's
// local folder (slskd names local subfolders after the remote peer's own leaf
// directory name, so two different peers sharing an identically-named folder
// can otherwise collide, corrupting Lidarr's later import scan). It skips the
// delete entirely when filenames don't share one common remote directory
// (commonLeaf == ""): that's ambiguous, and slskd's API only accepts one
// relative subdirectory name, so guessing wrong risks deleting more than this
// candidate wrote. It also refuses to act on the quarantine directory itself
// (see quarantineDirName), which a peer's remote folder can legitimately be
// named: DeleteDownloadFolder is recursive on both backends, so one such peer
// would otherwise wipe every album quarantined so far. A delete failure is
// logged and otherwise ignored — it must not block the job from moving on to
// its next candidate. A 404 means the candidate never wrote any bytes (e.g. it
// failed before any transfer started), which is routine, so it's logged quietly
// rather than as an ERROR.
//
// Ported from the legacy engine's Discoverer.cleanupAttempt
// (engine/discovery.go:812-825) as a shared free function so both Downloading
// and Importing reuse it.
func cleanupFolder(ctx context.Context, peers FolderCleaner, log *slog.Logger, jobID int64, filenames []string) {
	leaf := commonLeaf(filenames)
	if leaf == "" {
		return
	}
	// A peer's remote folder can legitimately be named ".failed"; deleting it
	// would take every already-quarantined album with it, since both backends'
	// DeleteDownloadFolder is recursive. Same guard, same reason, as
	// quarantineFolder's.
	if leaf == quarantineDirName {
		log.Info("skipping cleanup of a folder named like the quarantine dir", "album_job", jobID, "folder", leaf)
		return
	}
	err := peers.DeleteDownloadFolder(ctx, leaf)
	switch {
	case err == nil:
	case errors.Is(err, core.ErrRemoteNotFound):
		log.Info("nothing to clean up for failed attempt", "album_job", jobID, "folder", leaf)
	default:
		log.Error("cleanup failed attempt's downloaded files failed", "album_job", jobID, "folder", leaf, "err", err)
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

// cleanupCompletedFolder best-effort removes the per-album folder slskd
// created under completeDir once Lidarr has confirmed the import, so a
// completed job doesn't leave an empty leftover directory behind forever.
// Unlike cleanupFolder (used for a failed/rejected candidate via slskd's
// recursive DeleteDownloadFolder API), this only ever removes an
// already-verified-empty directory with os.Remove: if anything remains
// (e.g. a partial import), os.Remove fails safely rather than deleting real
// files, and that failure is only logged - it must never block the job's
// own DONE transition, which has already committed by the time this runs.
func cleanupCompletedFolder(log *slog.Logger, jobID int64, completeDir string, filenames []string) {
	leaf := commonLeaf(filenames)
	if leaf == "" {
		return
	}
	folder := filepath.Join(completeDir, leaf)
	entries, err := os.ReadDir(folder)
	switch {
	case err == nil:
		if len(entries) > 0 {
			log.Info("completed album folder not empty, leaving in place", "album_job", jobID, "folder", folder, "entries", len(entries))
			return
		}
		if err := os.Remove(folder); err != nil {
			log.Error("remove completed album folder failed", "album_job", jobID, "folder", folder, "err", err)
			return
		}
		log.Info("removed empty completed album folder", "album_job", jobID, "folder", folder)
	case os.IsNotExist(err):
		log.Info("completed album folder already gone", "album_job", jobID, "folder", folder)
	default:
		log.Error("read completed album folder failed", "album_job", jobID, "folder", folder, "err", err)
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
