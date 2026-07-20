package soulseek

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

func TestRescanSharesPublishesVirtualIndexAndKeepsLastGood(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Album", "track.flac"), []byte("not really flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.mp3")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.mp3")); err != nil {
		t.Fatal(err)
	}

	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	stats, err := c.RescanShares(context.Background())
	if err != nil {
		t.Fatalf("RescanShares: %v", err)
	}
	if stats.Files != 2 || stats.Directories != 2 {
		t.Fatalf("stats = %+v, want 2 files/2 directories", stats)
	}
	snapshot := c.shareSnapshot()
	if snapshot.files[`Music\Album\track.flac`] == nil || snapshot.files[`Music\README`] == nil {
		t.Fatalf("virtual files missing: %#v", snapshot.files)
	}
	for public, indexed := range snapshot.files {
		if strings.Contains(public, root) || strings.Contains(indexed.wire.Name, root) {
			t.Fatalf("local root leaked in public data: %q / %q", public, indexed.wire.Name)
		}
	}
	if snapshot.files[`Music\escape.mp3`] != nil {
		t.Fatal("symlink entry was indexed")
	}

	var browse peer.SharedFileListResponse
	if err := browse.Deserialize(bytes.NewReader(snapshot.sharedFrame)); err != nil {
		t.Fatalf("deserialize shared frame: %v", err)
	}
	if len(browse.Directories) != 2 {
		t.Fatalf("browse directories = %d", len(browse.Directories))
	}

	c.cfg.SharedFolders[0].Path = filepath.Join(root, "missing")
	if _, err := c.RescanShares(context.Background()); err == nil {
		t.Fatal("expected failed rescan")
	}
	if c.shareSnapshot() != snapshot {
		t.Fatal("failed rescan replaced last-known-good snapshot")
	}
}

func TestShareSearchAndFolderLookup(t *testing.T) {
	s := &shareSnapshot{
		search: []*indexedFile{
			{virtual: `Music\Artist\keep.flac`, wire: peer.File{Name: `Music\Artist\keep.flac`, Size: 1, Extension: "flac"}},
			{virtual: `Music\Artist\demo.mp3`, wire: peer.File{Name: `Music\Artist\demo.mp3`, Size: 1, Extension: "mp3"}},
		},
		directories: []peer.Directory{{Name: `Music\Artist`, Files: []peer.File{{Name: "keep.flac"}}}, {Name: `Music\Artist\Disc 2`}},
	}
	matches := s.match("ARTIST -demo", 10)
	if len(matches) != 1 || matches[0].Name != `Music\Artist\keep.flac` {
		t.Fatalf("matches = %#v", matches)
	}
	response := s.folderResponse(7, `Music/Artist`)
	if response.Token != 7 || response.Folder != `Music/Artist` || len(response.Folders) != 2 {
		t.Fatalf("folder response = %#v", response)
	}
	for _, unsafe := range []string{`../secret`, `Music\..\secret`, `\Music`} {
		if got := s.folderResponse(1, unsafe); len(got.Folders) != 0 {
			t.Fatalf("unsafe lookup %q returned folders", unsafe)
		}
	}
}
