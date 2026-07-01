package engine

import "testing"

func TestAlbumFolder(t *testing.T) {
	complete := "/music/slskd-downloads"
	files := []string{
		`music\Sia\1000 Forms of Fear (2014)\01 - Chandelier.flac`,
		`music\Sia\1000 Forms of Fear (2014)\02 - Big Girls Cry.flac`,
	}
	got := AlbumFolder(complete, files)
	want := "/music/slskd-downloads/music/Sia/1000 Forms of Fear (2014)"
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
