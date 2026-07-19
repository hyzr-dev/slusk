package pipeline

import (
	"context"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/samuelenocsson/slskdarr/internal/slskd"
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
// candidate wrote. A delete failure is logged and otherwise ignored — it must
// not block the job from moving on to its next candidate. A 404 means the
// candidate never wrote any bytes (e.g. it failed before any transfer started),
// which is routine, so it's logged quietly rather than as an ERROR.
//
// Ported from the legacy engine's Discoverer.cleanupAttempt
// (engine/discovery.go:812-825) as a shared free function so both Downloading
// and Importing reuse it.
func cleanupFolder(ctx context.Context, peers FolderCleaner, log *slog.Logger, jobID int64, filenames []string) {
	leaf := commonLeaf(filenames)
	if leaf == "" {
		return
	}
	err := peers.DeleteDownloadFolder(ctx, leaf)
	switch {
	case err == nil:
	case slskd.IsNotFound(err):
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
	return path.Base(common)
}
