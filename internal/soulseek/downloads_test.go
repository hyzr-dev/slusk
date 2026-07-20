package soulseek

import (
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/pipeline"
)

// TestDownloadDestPathMatchesPipelineAlbumFolder is the drift lock: the native
// downloader's dest-path logic (destLeaf/downloadDestPath) must agree with the
// pipeline's AlbumFolder for every representative filename, so a
// natively-downloaded file lands where the Importing scan looks for it. If
// pipeline.AlbumFolder's convention ever changes, this fails.
func TestDownloadDestPathMatchesPipelineAlbumFolder(t *testing.T) {
	const completeDir = "/music/dl"
	cases := []string{
		`Music\Artist - Album\01 Track.flac`,
		"Music/Artist - Album/02 Track.flac",
		`@@abcd\Shared\Some Album [2020]\1-01 Intro.mp3`,
		"single-level/file.flac",
		"noleaf.flac",
		`C:\Users\bob\deep\nested\track.flac`,
	}
	for _, f := range cases {
		base := path.Base(strings.ReplaceAll(f, `\`, "/"))
		want := filepath.Join(pipeline.AlbumFolder(completeDir, []string{f}), base)
		if got := downloadDestPath(completeDir, f); got != want {
			t.Errorf("downloadDestPath(%q) = %q, want %q (drift from pipeline.AlbumFolder)", f, got, want)
		}
	}
}

func TestDestLeafEmptyForRootLevelFile(t *testing.T) {
	for _, f := range []string{"file.flac", "", `\`, "/"} {
		if got := destLeaf(f); got != "" {
			t.Errorf("destLeaf(%q) = %q, want \"\"", f, got)
		}
	}
}

func TestCategorizeUploadFailure(t *testing.T) {
	cases := []struct {
		reason    string
		retryable bool
	}{
		{"File not shared", false},
		{"File not shared.", false},
		{"not shared", false},
		{"Banned", false},
		{"You are banned from this user", false},
		{"Too many megabytes", true},
		{"Too many files", true},
		{"Queued", true},
		{"Cancelled", true},
		{"Complete", true},
		{"", true},
	}
	for _, tc := range cases {
		failure, retryable := categorizeUploadFailure(tc.reason)
		if retryable != tc.retryable {
			t.Errorf("categorizeUploadFailure(%q) retryable = %v, want %v", tc.reason, retryable, tc.retryable)
		}
		if failure != tc.reason {
			t.Errorf("categorizeUploadFailure(%q) failure = %q, want %q", tc.reason, failure, tc.reason)
		}
	}
}
