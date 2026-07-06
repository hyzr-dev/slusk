package pipeline

import (
	"path"
	"strings"
)

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
