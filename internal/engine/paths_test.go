package engine

import "testing"

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
