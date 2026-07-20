package soulseek

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

func TestUploadQueueFIFOPositionsDeduplicateAndBounds(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.flac", "b.flac"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, UploadSlots: 1}, testLogger())
	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := newUploadManager(c, 1)
	if err := m.enqueue("alice", `Music\a.flac`); err != nil {
		t.Fatal(err)
	}
	if err := m.enqueue("bob", `Music\b.flac`); err != nil {
		t.Fatal(err)
	}
	if err := m.enqueue("alice", `Music\a.flac`); err != nil {
		t.Fatal(err)
	}
	if place, ok := m.position(uploadKey{"alice", `Music\a.flac`}); !ok || place != 1 {
		t.Fatalf("alice place = %d, %v", place, ok)
	}
	if place, ok := m.position(uploadKey{"bob", `Music\b.flac`}); !ok || place != 2 {
		t.Fatalf("bob place = %d, %v", place, ok)
	}
	if free, queued := m.availability(); free || queued != 2 {
		t.Fatalf("availability = %v, %d", free, queued)
	}
	if err := m.enqueue("mallory", `Music\..\secret`); err != peer.ErrFileNotShared {
		t.Fatalf("traversal enqueue error = %v", err)
	}
}

func TestOpenIndexedFileRejectsReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "track.flac")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatal(err)
	}
	indexed := c.shareSnapshot().files[`Music\track.flac`]
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if f, err := openIndexedFile(indexed); err == nil {
		f.Close()
		t.Fatal("replacement opened without rescan")
	}
}
