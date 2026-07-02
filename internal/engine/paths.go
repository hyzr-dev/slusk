package engine

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
	if len(filenames) == 0 {
		return completeDir
	}
	dir := func(f string) string {
		f = strings.ReplaceAll(f, `\`, "/")
		return path.Dir(f)
	}
	common := dir(filenames[0])
	for _, f := range filenames[1:] {
		if dir(f) != common {
			return completeDir // no single album folder -> scan the root
		}
	}
	if common == "." || common == "/" || common == "" {
		return completeDir
	}
	return path.Join(completeDir, path.Base(common))
}
