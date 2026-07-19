package pipeline

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func df(path string, size int64, disc, track int, title string, lossless bool) dedupFile {
	return dedupFile{path: path, size: size, disc: disc, track: track, titleKey: normalizeTitle(title), lossless: lossless}
}

func loserPaths(files []dedupFile) []string {
	var out []string
	for _, f := range dedupFiles(files) {
		out = append(out, f.path)
	}
	slices.Sort(out)
	return out
}

func TestDedupFilesGroupsByDiscAndTrack(t *testing.T) {
	files := []dedupFile{
		df("a.flac", 30_000_000, 1, 1, "Song One", true),
		df("a.mp3", 8_000_000, 1, 1, "Song One", false),
		df("b.mp3", 9_000_000, 1, 2, "Song Two", false),
	}
	if got, want := loserPaths(files), []string{"a.mp3"}; !slices.Equal(got, want) {
		t.Errorf("losers = %v, want %v", got, want)
	}
}

func TestDedupFilesLosslessBeatsLossyRegardlessOfSize(t *testing.T) {
	files := []dedupFile{
		df("small.flac", 5_000_000, 1, 1, "Song", true),
		df("big.mp3", 90_000_000, 1, 1, "Song", false),
	}
	if got, want := loserPaths(files), []string{"big.mp3"}; !slices.Equal(got, want) {
		t.Errorf("losers = %v, want %v", got, want)
	}
}

func TestDedupFilesSizeBreaksTiesWithinFormatClass(t *testing.T) {
	files := []dedupFile{
		df("low.mp3", 6_000_000, 1, 1, "Song", false),
		df("high.mp3", 12_000_000, 1, 1, "Song", false),
	}
	if got, want := loserPaths(files), []string{"low.mp3"}; !slices.Equal(got, want) {
		t.Errorf("losers = %v, want %v", got, want)
	}
}

func TestDedupFilesUntrackedFileJoinsNumberedGroupByTitle(t *testing.T) {
	files := []dedupFile{
		df("tagged.flac", 30_000_000, 1, 3, "My Song (feat. Someone)", true),
		df("untagged.mp3", 8_000_000, 0, 0, "my song", false),
	}
	if got, want := loserPaths(files), []string{"untagged.mp3"}; !slices.Equal(got, want) {
		t.Errorf("losers = %v, want %v", got, want)
	}
}

func TestDedupFilesUnidentifiableFilesAreNeverRemoved(t *testing.T) {
	files := []dedupFile{
		df("mystery1.mp3", 8_000_000, 0, 0, "", false),
		df("mystery2.mp3", 8_000_000, 0, 0, "", false),
		df("song.flac", 30_000_000, 1, 1, "Song", true),
	}
	if got := loserPaths(files); len(got) != 0 {
		t.Errorf("losers = %v, want none", got)
	}
}

func TestDedupFilesDistinctTracksUntouched(t *testing.T) {
	files := []dedupFile{
		df("1.flac", 30_000_000, 1, 1, "One", true),
		df("2.flac", 31_000_000, 1, 2, "Two", true),
		df("d2-1.flac", 32_000_000, 2, 1, "One Reprise", true),
	}
	if got := loserPaths(files); len(got) != 0 {
		t.Errorf("losers = %v, want none", got)
	}
}

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"Song One":           "songone",
		"  Song ONE!  ":      "songone",
		"Song (feat. X & Y)": "song",
		"Song ft. Somebody":  "song",
		"01 - Song":          "01song",
	}
	for in, want := range cases {
		if got := normalizeTitle(in); got != want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDedupAlbumFolderRemovesLosersFromDisk exercises the folder-level entry
// point with readFileMeta stubbed (crafting real tagged audio fixtures is the
// tag library's job to parse, not ours to generate).
func TestDedupAlbumFolderRemovesLosersFromDisk(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.flac", "one.mp3", "two.mp3", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := readFileMeta
	t.Cleanup(func() { readFileMeta = orig })
	readFileMeta = func(path string, size int64) dedupFile {
		switch filepath.Base(path) {
		case "one.flac":
			return df(path, 30_000_000, 1, 1, "One", true)
		case "one.mp3":
			return df(path, 8_000_000, 1, 1, "One", false)
		default:
			return df(path, 9_000_000, 1, 2, "Two", false)
		}
	}

	removed, err := dedupAlbumFolder(slog.Default(), dir)
	if err != nil {
		t.Fatalf("dedupAlbumFolder: %v", err)
	}
	if len(removed) != 1 || filepath.Base(removed[0]) != "one.mp3" {
		t.Fatalf("removed = %v, want [one.mp3]", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "one.mp3")); !os.IsNotExist(err) {
		t.Error("one.mp3 still on disk")
	}
	for _, name := range []string{"one.flac", "two.mp3", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s unexpectedly gone: %v", name, err)
		}
	}
}
