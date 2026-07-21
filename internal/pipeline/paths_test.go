package pipeline

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAlbumFolder(t *testing.T) {
	complete := "/music/slskd-downloads"
	files := []string{
		`music\Sia\1000 Forms of Fear (2014)\01 - Chandelier.flac`,
		`music\Sia\1000 Forms of Fear (2014)\02 - Big Girls Cry.flac`,
	}
	got := AlbumFolder(complete, files)
	want := "/music/slskd-downloads/1000 Forms of Fear (2014)"
	if got != want {
		t.Errorf("AlbumFolder = %q, want %q", got, want)
	}
}

func TestAlbumFolderDeeplyNestedRemoteShare(t *testing.T) {
	// slskd only recreates the leaf album folder locally, discarding the
	// remote peer's own alphabetical share structure (Music/<letter>/<artist>/<album>).
	complete := "/data/media/downloads-slskd"
	files := []string{
		`Music\B\Blut Aus Nord\2023 - Disharmonium - Nahab\01 - Track.flac`,
		`Music\B\Blut Aus Nord\2023 - Disharmonium - Nahab\02 - Track.flac`,
	}
	got := AlbumFolder(complete, files)
	want := "/data/media/downloads-slskd/2023 - Disharmonium - Nahab"
	if got != want {
		t.Errorf("AlbumFolder = %q, want %q", got, want)
	}
}

func TestAlbumFolderFallsBackToRoot(t *testing.T) {
	if got := AlbumFolder("/music/dl", nil); got != "/music/dl" {
		t.Errorf("empty filenames should fall back to root, got %q", got)
	}
	// No common directory -> fall back to root.
	files := []string{`a\1.flac`, `b\2.flac`}
	if got := AlbumFolder("/music/dl", files); got != "/music/dl" {
		t.Errorf("no common dir should fall back to root, got %q", got)
	}
}

func TestCommonLeaf(t *testing.T) {
	files := []string{
		`music\Sia\1000 Forms of Fear (2014)\01 - Chandelier.flac`,
		`music\Sia\1000 Forms of Fear (2014)\02 - Big Girls Cry.flac`,
	}
	got := commonLeaf(files)
	want := "1000 Forms of Fear (2014)"
	if got != want {
		t.Errorf("commonLeaf = %q, want %q", got, want)
	}
}

// TestCommonLeafRejectsTraversal locks the upstream half of the path-traversal
// fix: a common remote directory that resolves to ".." must yield "" so it can
// never reach AlbumFolder (scan) or DeleteDownloadFolder (os.RemoveAll).
func TestCommonLeafRejectsTraversal(t *testing.T) {
	for _, files := range [][]string{
		{`..\track1.flac`, `..\track2.flac`},
		{`a\..\..\x.flac`, `a\..\..\y.flac`},
	} {
		if got := commonLeaf(files); got != "" {
			t.Errorf("commonLeaf(%v) = %q, want \"\" (traversal must be rejected)", files, got)
		}
	}
}

func TestCommonLeafEmptyWhenAmbiguous(t *testing.T) {
	if got := commonLeaf(nil); got != "" {
		t.Errorf("empty filenames should yield \"\", got %q", got)
	}
	// No common directory -> ambiguous.
	files := []string{`a\1.flac`, `b\2.flac`}
	if got := commonLeaf(files); got != "" {
		t.Errorf("no common dir should yield \"\", got %q", got)
	}
}

// newTestLogger returns a *slog.Logger writing to buf, so cleanupCompletedFolder
// tests can assert on the specific log level (Info vs Error) emitted.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

func TestCleanupCompletedFolderRemovesEmptyDir(t *testing.T) {
	completeDir := t.TempDir()
	folder := filepath.Join(completeDir, "1000 Forms of Fear (2014)")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	files := []string{
		`music\Sia\1000 Forms of Fear (2014)\01 - Chandelier.flac`,
		`music\Sia\1000 Forms of Fear (2014)\02 - Big Girls Cry.flac`,
	}

	var buf bytes.Buffer
	cleanupCompletedFolder(newTestLogger(&buf), 1, completeDir, files)

	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Errorf("expected empty folder to be removed, stat err = %v", err)
	}
	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected no ERROR log line, got %q", logged)
	}
	if !strings.Contains(logged, "level=INFO") {
		t.Errorf("expected an INFO log line, got %q", logged)
	}
}

func TestCleanupCompletedFolderSkipsNonEmptyDir(t *testing.T) {
	completeDir := t.TempDir()
	folder := filepath.Join(completeDir, "1000 Forms of Fear (2014)")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(folder, "leftover.txt"), []byte("junk"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	files := []string{
		`music\Sia\1000 Forms of Fear (2014)\01 - Chandelier.flac`,
		`music\Sia\1000 Forms of Fear (2014)\02 - Big Girls Cry.flac`,
	}

	var buf bytes.Buffer
	cleanupCompletedFolder(newTestLogger(&buf), 1, completeDir, files)

	if _, err := os.Stat(folder); err != nil {
		t.Errorf("expected non-empty folder to remain, stat err = %v", err)
	}
	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected no ERROR log line for a non-empty folder, got %q", logged)
	}
	if !strings.Contains(logged, "level=INFO") {
		t.Errorf("expected an INFO log line, got %q", logged)
	}
}

func TestCleanupCompletedFolderSkipsAmbiguousLeaf(t *testing.T) {
	completeDir := t.TempDir()
	// Files don't share a common directory -> commonLeaf returns "".
	files := []string{`a\1.flac`, `b\2.flac`}

	var buf bytes.Buffer
	cleanupCompletedFolder(newTestLogger(&buf), 1, completeDir, files)

	if logged := buf.String(); logged != "" {
		t.Errorf("expected no filesystem interaction or logging for an ambiguous leaf, got %q", logged)
	}
}

func TestCleanupCompletedFolderLogsMissingDirQuietly(t *testing.T) {
	completeDir := t.TempDir()
	files := []string{
		`music\Sia\1000 Forms of Fear (2014)\01 - Chandelier.flac`,
		`music\Sia\1000 Forms of Fear (2014)\02 - Big Girls Cry.flac`,
	}

	var buf bytes.Buffer
	cleanupCompletedFolder(newTestLogger(&buf), 1, completeDir, files)

	logged := buf.String()
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected no ERROR log line for a missing folder, got %q", logged)
	}
	if !strings.Contains(logged, "level=INFO") {
		t.Errorf("expected an INFO log line, got %q", logged)
	}
}
