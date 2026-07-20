package soulseek

import (
	"path"
	"path/filepath"
	"strings"
)

// destLeaf returns the local subdirectory name a downloaded file is written
// into: the base name of the file's remote directory, with Soulseek's "\"
// path separators normalized to "/". It returns "" when the file has no
// meaningful parent directory (the file is written directly under the
// downloads root then).
//
// This deliberately mirrors internal/pipeline/paths.go:commonLeaf for a single
// file, so a natively-downloaded file lands in exactly the same place slskd
// would have written it and the Importing module's AlbumFolder scan still
// finds it. It is a copy rather than a shared import because internal/soulseek
// is a low-level protocol provider and importing the high-level pipeline
// package would invert the layering (cf. nextBackoff copied in client.go).
// TestDownloadDestPathMatchesPipelineAlbumFolder locks the two against drift.
func destLeaf(filename string) string {
	dir := path.Dir(strings.ReplaceAll(filename, `\`, "/"))
	if dir == "." || dir == "/" || dir == "" {
		return ""
	}
	return path.Base(dir)
}

// downloadDestPath returns the absolute local path a downloaded file is written
// to under completeDir, matching the completeDir/<leaf>/<base> layout slskd
// produces and pipeline.AlbumFolder expects.
func downloadDestPath(completeDir, filename string) string {
	base := path.Base(strings.ReplaceAll(filename, `\`, "/"))
	if leaf := destLeaf(filename); leaf != "" {
		return filepath.Join(completeDir, leaf, base)
	}
	return filepath.Join(completeDir, base)
}

// permanentUploadFailureReasons are the substrings that mark a peer's upload
// rejection as permanent (not worth re-queueing). Kept identical to
// internal/slskd/client.go:isTransientFailure so the native downloader reports
// failures in the same vocabulary the pipeline's retry logic already
// understands.
var permanentUploadFailureReasons = []string{"file not shared", "not shared", "banned"}

// categorizeUploadFailure classifies a peer's upload rejection or failure
// reason as permanent (retryable == false) or transient (retryable == true),
// mirroring internal/slskd/client.go:isTransientFailure. The reason is echoed
// back unchanged as failure. It expresses no opinion on non-failure reasons
// such as "Queued" (a peer keeping us in its queue): the download orchestration
// decides those before ever calling this.
func categorizeUploadFailure(reason string) (failure string, retryable bool) {
	lower := strings.ToLower(reason)
	for _, permanent := range permanentUploadFailureReasons {
		if strings.Contains(lower, permanent) {
			return reason, false
		}
	}
	return reason, true
}
