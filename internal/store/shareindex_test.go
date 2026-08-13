package store

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

func shareIndexEntry(virtual, local string, size int64) core.ShareIndexEntry {
	return core.ShareIndexEntry{
		VirtualPath: virtual,
		LocalPath:   local,
		ShareRoot:   "/music",
		Size:        size,
		Extension:   "flac",
		ModTime:     time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC).Add(123457 * time.Microsecond),
		Bitrate:     320,
		Duration:    200,
	}
}

func sampleShareIndex() core.ShareIndex {
	return core.ShareIndex{
		ScannedAt:    time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC),
		ScanDuration: 90 * time.Second,
		Folders:      []core.SharedFolder{{Name: "Music", Path: "/music"}},
		Directories:  []string{"Music", `Music\Artist`, `Music\Artist\Empty`},
		Files: []core.ShareIndexEntry{
			shareIndexEntry(`Music\Artist\a.flac`, "/music/Artist/a.flac", 111),
			shareIndexEntry(`Music\Artist\b.flac`, "/music/Artist/b.flac", 222),
		},
		TotalBytes: 333,
	}
}

// TestShareIndexRoundTrip asserts everything the load path needs survives the
// database, including the odd (non-round) microsecond mtime the upload path
// compares against and the directory holding no files.
func TestShareIndexRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	want := sampleShareIndex()

	if err := s.ReplaceShareIndex(ctx, want); err != nil {
		t.Fatalf("ReplaceShareIndex: %v", err)
	}
	got, err := s.ShareIndex(ctx)
	if err != nil {
		t.Fatalf("ShareIndex: %v", err)
	}
	if got == nil {
		t.Fatal("ShareIndex returned nil after a save")
	}
	if !got.ScannedAt.Equal(want.ScannedAt) || got.ScanDuration != want.ScanDuration {
		t.Fatalf("scan row = %v/%v, want %v/%v", got.ScannedAt, got.ScanDuration, want.ScannedAt, want.ScanDuration)
	}
	if len(got.Folders) != 1 || got.Folders[0] != want.Folders[0] {
		t.Fatalf("folders = %+v, want %+v", got.Folders, want.Folders)
	}
	if got.FileCount != 2 || got.TotalBytes != 333 {
		t.Fatalf("file count/total bytes = %d/%d, want 2/333", got.FileCount, got.TotalBytes)
	}
	sort.Strings(got.Directories)
	if strings.Join(got.Directories, "|") != `Music|Music\Artist|Music\Artist\Empty` {
		t.Fatalf("directories = %q, want all three including the empty one", got.Directories)
	}
	sort.Slice(got.Files, func(i, j int) bool { return got.Files[i].VirtualPath < got.Files[j].VirtualPath })
	for i, file := range got.Files {
		w := want.Files[i]
		if file.VirtualPath != w.VirtualPath || file.LocalPath != w.LocalPath || file.ShareRoot != w.ShareRoot ||
			file.Size != w.Size || file.Extension != w.Extension ||
			file.Bitrate != w.Bitrate || file.Duration != w.Duration {
			t.Fatalf("file %d = %+v, want %+v", i, file, w)
		}
		if file.ModTime.UnixMicro() != w.ModTime.UnixMicro() {
			t.Fatalf("file %d mtime = %v, want %v", i, file.ModTime, w.ModTime)
		}
	}
}

// TestShareIndexAbsentReturnsNil asserts "nothing stored yet" is not an error:
// the caller distinguishes it from a failed read only by the nil.
func TestShareIndexAbsentReturnsNil(t *testing.T) {
	s := newTestStore(t)
	got, err := s.ShareIndex(context.Background())
	if err != nil {
		t.Fatalf("ShareIndex: %v", err)
	}
	if got != nil {
		t.Fatalf("ShareIndex = %+v, want nil before anything is saved", got)
	}
}

// TestReplaceShareIndexReplacesRatherThanMerges pins the tables' invariant:
// they hold the result of exactly one scan, so a second save must leave no
// trace of the first.
func TestReplaceShareIndexReplacesRatherThanMerges(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.ReplaceShareIndex(ctx, sampleShareIndex()); err != nil {
		t.Fatalf("first ReplaceShareIndex: %v", err)
	}

	second := core.ShareIndex{
		ScannedAt:   time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC),
		Folders:     []core.SharedFolder{{Name: "Bootlegs", Path: "/bootlegs"}},
		Directories: []string{"Bootlegs"},
		Files:       []core.ShareIndexEntry{shareIndexEntry(`Bootlegs\x.flac`, "/bootlegs/x.flac", 5)},
		TotalBytes:  5,
	}
	if err := s.ReplaceShareIndex(ctx, second); err != nil {
		t.Fatalf("second ReplaceShareIndex: %v", err)
	}

	got, err := s.ShareIndex(ctx)
	if err != nil {
		t.Fatalf("ShareIndex: %v", err)
	}
	if got.FileCount != 1 || len(got.Files) != 1 || got.Files[0].VirtualPath != `Bootlegs\x.flac` {
		t.Fatalf("files = %+v, want only the second scan's", got.Files)
	}
	if len(got.Directories) != 1 || got.Directories[0] != "Bootlegs" {
		t.Fatalf("directories = %q, want only the second scan's", got.Directories)
	}
	if len(got.Folders) != 1 || got.Folders[0].Name != "Bootlegs" {
		t.Fatalf("folders = %+v, want only the second scan's", got.Folders)
	}
}

// TestReplaceShareIndexRefusesUnstorablePaths asserts a path Postgres cannot
// hold fails the whole save rather than being skipped. A skipped row is a file
// that quietly stops being offered to peers after the next restart, which is
// worse than falling back to a full scan.
func TestReplaceShareIndexRefusesUnstorablePaths(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	cases := map[string]core.ShareIndexEntry{
		"not valid UTF-8": shareIndexEntry("Music\\"+string([]byte{0xff, 0xfe})+".flac", "/music/x.flac", 1),
		"over-long path":  shareIndexEntry(`Music\`+strings.Repeat("a", maxShareIndexPathBytes), "/music/x.flac", 1),
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			index := sampleShareIndex()
			index.Files = append(index.Files, entry)
			if err := s.ReplaceShareIndex(ctx, index); err == nil {
				t.Fatal("ReplaceShareIndex accepted an unstorable path")
			}
			got, err := s.ShareIndex(ctx)
			if err != nil {
				t.Fatalf("ShareIndex: %v", err)
			}
			if got != nil {
				t.Fatalf("a refused save left rows behind: %+v", got)
			}
		})
	}
}

// TestReplaceShareIndexStoresFoldersInPlainText asserts the validity condition
// is readable in the row, not hashed: rejecting an index has to be able to say
// what differed.
func TestReplaceShareIndexStoresFoldersInPlainText(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.ReplaceShareIndex(ctx, sampleShareIndex()); err != nil {
		t.Fatalf("ReplaceShareIndex: %v", err)
	}

	var folders string
	if err := s.db.QueryRowContext(ctx, `SELECT shared_folders::text FROM share_index_scan`).Scan(&folders); err != nil {
		t.Fatalf("read shared_folders: %v", err)
	}
	if !strings.Contains(folders, `"Music"`) || !strings.Contains(folders, `"/music"`) {
		t.Fatalf("shared_folders = %q, want the name and path readable", folders)
	}
}
