package soulseek

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
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

func TestLegacyDownloadTransferRequestIsQueuedAndAcknowledged(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "track.mp3"), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, UploadSlots: 1}, testLogger())
	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatal(err)
	}
	startSessionLifecycle(t, c)
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	session := c.newSession(local, sessionKey{username: "slskd", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)

	request := &peer.TransferRequest{Direction: peer.DownloadFromPeer, Token: soul.Token(42), Filename: `Music\track.mp3`}
	wire, err := request.Serialize(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.sessionHooks.frame(session, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeTransferRequest), wire: wire}); err != nil {
		t.Fatalf("legacy transfer request: %v", err)
	}

	select {
	case responseWire := <-session.writes:
		var response peer.TransferResponse
		if err := response.Deserialize(bytes.NewReader(responseWire)); err != nil {
			t.Fatalf("deserialize response: %v", err)
		}
		if response.Token != 42 || response.Allowed || response.Reason == nil || response.Reason.Error() != peer.ErrQueued.Error() {
			t.Fatalf("response = %+v, want token 42 and Queued", response)
		}
	case <-time.After(time.Second):
		t.Fatal("legacy transfer request was not acknowledged")
	}
	if place, ok := c.uploads.position(uploadKey{username: "slskd", filename: `Music\track.mp3`}); !ok || place != 1 {
		t.Fatalf("queued upload position = %d, %v", place, ok)
	}
}

func TestUploadDispatcherHandsOffSlotAndResetsOnShutdown(t *testing.T) {
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
	startSessionLifecycle(t, c)
	m := c.uploads
	started := make(chan uploadKey, 2)
	releaseFirst := make(chan struct{})
	m.execute = func(ctx context.Context, job *uploadJob) {
		started <- job.key
		switch job.key.username {
		case "alice":
			select {
			case <-releaseFirst:
			case <-ctx.Done():
			}
		case "bob":
			<-ctx.Done()
		}
	}
	ctx, cancel := context.WithCancel(c.lifecycleContext())
	dispatchDone := make(chan struct{})
	if !c.startTracked(func() {
		m.dispatch(ctx)
		close(dispatchDone)
	}) {
		t.Fatal("dispatcher did not start")
	}
	if err := m.enqueue("alice", `Music\a.flac`); err != nil {
		t.Fatal(err)
	}
	if err := m.enqueue("bob", `Music\b.flac`); err != nil {
		t.Fatal(err)
	}
	select {
	case key := <-started:
		if key.username != "alice" {
			t.Fatalf("first upload = %+v", key)
		}
	case <-time.After(time.Second):
		t.Fatal("first upload did not start")
	}
	if place, ok := m.position(uploadKey{"bob", `Music\b.flac`}); !ok || place != 1 {
		t.Fatalf("bob waiting position = %d, %v", place, ok)
	}
	close(releaseFirst)
	select {
	case key := <-started:
		if key.username != "bob" {
			t.Fatalf("second upload = %+v", key)
		}
	case <-time.After(time.Second):
		t.Fatal("slot was not handed to second upload")
	}
	if err := m.enqueue("carol", `Music\a.flac`); err != nil {
		t.Fatal(err)
	}
	responses := make(chan peer.TransferResponse, 1)
	m.registerToken(99, "carol", responses)
	cancel()
	select {
	case <-dispatchDone:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not stop")
	}
	m.mu.Lock()
	waiting, active, keys, users, tokens := len(m.waiting), m.active, len(m.byKey), len(m.perUser), len(m.byToken)
	m.mu.Unlock()
	if waiting != 0 || active != 0 || keys != 0 || users != 0 || tokens != 0 {
		t.Fatalf("queue state after shutdown = waiting:%d active:%d keys:%d users:%d tokens:%d", waiting, active, keys, users, tokens)
	}

	restartCtx, restartCancel := context.WithCancel(c.lifecycleContext())
	restartDone := make(chan struct{})
	if !c.startTracked(func() {
		m.dispatch(restartCtx)
		close(restartDone)
	}) {
		t.Fatal("restarted dispatcher did not start")
	}
	if err := m.enqueue("dave", `Music\b.flac`); err != nil {
		t.Fatal(err)
	}
	select {
	case key := <-started:
		if key.username != "dave" {
			t.Fatalf("upload after restart = %+v", key)
		}
	case <-time.After(time.Second):
		t.Fatal("upload did not start after dispatcher restart")
	}
	restartCancel()
	select {
	case <-restartDone:
	case <-time.After(time.Second):
		t.Fatal("restarted dispatcher did not stop")
	}
}

func TestUploadTransferResponseIsBoundToPeer(t *testing.T) {
	m := newUploadManager(nil, 1)
	responses := make(chan peer.TransferResponse, 1)
	m.registerToken(99, "alice", responses)

	m.deliver("mallory", peer.TransferResponse{Token: 99, Allowed: true})
	select {
	case <-responses:
		t.Fatal("response from the wrong peer was delivered")
	default:
	}

	want := peer.TransferResponse{Token: 99, Allowed: true}
	m.deliver("alice", want)
	select {
	case got := <-responses:
		if got.Token != want.Token || got.Allowed != want.Allowed {
			t.Fatalf("response = %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("response from the owning peer was not delivered")
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

func TestOpenIndexedFileRejectsParentSymlinkEscapeEvenForSameInode(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "album")
	if err := os.Mkdir(album, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(album, "track.flac")
	if err := os.WriteFile(path, []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatal(err)
	}
	indexed := c.shareSnapshot().files[`Music\album\track.flac`]
	outside := t.TempDir()
	if err := os.Link(path, filepath.Join(outside, "track.flac")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := os.Rename(album, filepath.Join(root, "album-old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, album); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if f, err := openIndexedFile(indexed); err == nil {
		f.Close()
		t.Fatal("file reached through an escaped parent symlink was opened")
	}
}
