package soulseek

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/distributed"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
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

func TestRescanSharesSkipsBackslashComponentsAndUsesLocalBasenameExtension(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip("backslash is a path separator on this platform")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, `collision\track.mp3`), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "TRACK.FLAC"), []byte("good"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, `hidden\dir`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, `hidden\dir`, "nested.mp3"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	stats, err := c.RescanShares(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 {
		t.Fatalf("files = %d, want only the safe file", stats.Files)
	}
	indexed := c.shareSnapshot().files[`Music\TRACK.FLAC`]
	if indexed == nil || indexed.wire.Extension != "flac" {
		t.Fatalf("safe indexed file = %#v", indexed)
	}
}

func TestSharingHooksBrowseFolderAndDownloadCoexistence(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Album", "track.flac"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatal(err)
	}
	startSessionLifecycle(t, c)
	local, remote := net.Pipe()
	defer remote.Close()
	session := c.newSession(local, sessionKey{username: "friend", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
	if _, inserted := c.sessions.Register(session); !inserted {
		t.Fatal("register test session")
	}

	browse := &peer.SharedFileListRequest{}
	wire, err := browse.Serialize(browse)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.sessionHooks.frame(session, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeSharedFileListRequest), wire: wire}); err != nil {
		t.Fatalf("browse hook: %v", err)
	}
	select {
	case responseWire := <-session.writes:
		var response peer.SharedFileListResponse
		if err := response.Deserialize(bytes.NewReader(responseWire)); err != nil || len(response.Directories) != 2 {
			t.Fatalf("browse response directories=%d err=%v", len(response.Directories), err)
		}
	case <-time.After(time.Second):
		t.Fatal("browse response was not queued")
	}

	folder := &peer.FolderContentsRequest{Token: soul.Token(7), Folder: `Music\Album`}
	wire, err = folder.Serialize(folder)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.sessionHooks.frame(session, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeFolderContentsRequest), wire: wire}); err != nil {
		t.Fatalf("folder hook: %v", err)
	}
	select {
	case responseWire := <-session.writes:
		var response peer.FolderContentsResponse
		if err := response.Deserialize(bytes.NewReader(responseWire)); err != nil || response.Token != 7 || len(response.Folders) != 1 {
			t.Fatalf("folder response=%+v err=%v", response, err)
		}
	case <-time.After(time.Second):
		t.Fatal("folder response was not queued")
	}

	// The sharing hooks must leave #55 download frames available to their
	// existing sibling hook on the same retained P session.
	request := &peer.TransferRequest{Direction: peer.UploadToPeer, Token: 8, Filename: "unknown.flac", FileSize: 1}
	wire, err = request.Serialize(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.sessionHooks.frame(session, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeTransferRequest), wire: wire}); err != nil {
		t.Fatalf("download hook after sharing hooks: %v", err)
	}
}

func TestRepeatedHugeBrowseResponsesRespectSessionByteBudget(t *testing.T) {
	c := New(Config{Username: "me"}, testLogger())
	largeFrame := make([]byte, int(maxOrdinaryPeerQueuedBytes/2)+1)
	c.shares.Store(&shareSnapshot{files: map[string]*indexedFile{}, byDirectory: map[string]peer.Directory{}, sharedFrame: largeFrame})
	startSessionLifecycle(t, c)
	local, remote := net.Pipe()
	defer remote.Close()
	session := c.newSession(local, sessionKey{username: "browser", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
	defer session.Close(errors.New("test complete"))

	request := &peer.SharedFileListRequest{}
	wire, err := request.Serialize(request)
	if err != nil {
		t.Fatal(err)
	}
	frame := sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeSharedFileListRequest), wire: wire}
	for i := 0; i < cap(session.writes); i++ {
		if err := c.sessionHooks.frame(session, frame); err != nil {
			t.Fatalf("browse request %d: %v", i, err)
		}
	}
	if got, want := len(session.writes), 1; got != want {
		t.Fatalf("queued browse responses = %d, want %d", got, want)
	}
	if got, want := session.queuedWriteBytes.Load(), int64(len(largeFrame)); got != want {
		t.Fatalf("queued browse bytes = %d, want %d", got, want)
	}
	if got := session.queuedWriteBytes.Load(); got > maxOrdinaryPeerQueuedBytes {
		t.Fatalf("queued browse bytes = %d, exceeds %d-byte budget", got, maxOrdinaryPeerQueuedBytes)
	}
}

func TestShareSearchSchedulingCentralDistributedAndBackpressure(t *testing.T) {
	c := New(Config{Username: "me"}, testLogger())
	c.shares.Store(&shareSnapshot{
		files:  map[string]*indexedFile{},
		search: []*indexedFile{{virtual: `Music\track.flac`, wire: peer.File{Name: `Music\track.flac`, Size: 4, Extension: "flac"}}},
	})
	startSessionLifecycle(t, c)
	newQueuedSession := func(username string) *peerSession {
		local, remote := net.Pipe()
		t.Cleanup(func() { _ = remote.Close() })
		s := c.newSession(local, sessionKey{username: username, connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
		if _, inserted := c.sessions.Register(s); !inserted {
			t.Fatalf("register %s", username)
		}
		return s
	}
	assertResponse := func(session *peerSession, token soul.Token) {
		t.Helper()
		select {
		case wire := <-session.writes:
			var response peer.FileSearchResponse
			if err := response.Deserialize(bytes.NewReader(wire)); err != nil || response.Token != token || len(response.Results) != 1 {
				t.Fatalf("search response token=%d results=%d err=%v", response.Token, len(response.Results), err)
			}
		case <-time.After(time.Second):
			t.Fatal("search response was not scheduled")
		}
	}

	central := newQueuedSession("central")
	payload := new(bytes.Buffer)
	if err := writeUint32(payload, uint32(server.CodeFileSearch)); err != nil {
		t.Fatal(err)
	}
	if err := writeString(payload, "central"); err != nil {
		t.Fatal(err)
	}
	if err := writeUint32(payload, 11); err != nil {
		t.Fatal(err)
	}
	if err := writeString(payload, "track"); err != nil {
		t.Fatal(err)
	}
	wire := packFrame(payload.Bytes())
	if err := c.handleMessage(context.Background(), server.CodeFileSearch, bytes.NewReader(wire)); err != nil {
		t.Fatal(err)
	}
	assertResponse(central, 11)

	dist := newQueuedSession("distributed")
	c.handleDistributedShareSearch(distributed.Search{Username: "distributed", Token: 12, Query: "track"}, nil)
	assertResponse(dist, 12)

	backpressured := newQueuedSession("downloader")
	for i := 0; i < cap(backpressured.writes); i++ {
		if !backpressured.TrySend([]byte{1}) {
			t.Fatal("fill session write queue")
		}
	}
	c.respondToSearch("downloader", 13, "track")
	deadline := time.Now().Add(time.Second)
	for len(c.shareWorkers) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-backpressured.done:
		t.Fatal("share response backpressure closed a P session used by downloads")
	default:
	}

	for i := 0; i < cap(c.shareWorkers); i++ {
		c.shareWorkers <- struct{}{}
	}
	if err := c.sessionHooks.frame(backpressured, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeFolderContentsRequest), wire: mustFolderRequestWire(t, 14, `Music`)}); err != nil {
		t.Fatalf("saturated folder request closed shared P session: %v", err)
	}
	for i := 0; i < cap(c.shareWorkers); i++ {
		<-c.shareWorkers
	}
}

func TestShareSearchDeliveryFailureCanRetry(t *testing.T) {
	c := New(Config{Username: "me"}, testLogger())
	c.shares.Store(&shareSnapshot{
		files:  map[string]*indexedFile{},
		search: []*indexedFile{{virtual: `Music\track.flac`, wire: peer.File{Name: `Music\track.flac`, Size: 4, Extension: "flac"}}},
	})
	startSessionLifecycle(t, c)

	newSession := func() (*peerSession, net.Conn) {
		local, remote := net.Pipe()
		s := c.newSession(local, sessionKey{username: "requester", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
		if _, inserted := c.sessions.Register(s); !inserted {
			t.Fatal("register requester session")
		}
		return s, remote
	}
	blocked, blockedRemote := newSession()
	for i := 0; i < cap(blocked.writes); i++ {
		if !blocked.TrySend([]byte{1}) {
			t.Fatal("fill blocked session")
		}
	}
	c.respondToSearch("requester", 77, "track")
	waitForShareWorkers(t, c)
	blocked.Close(errors.New("replace blocked session"))
	_ = blockedRemote.Close()

	retry, retryRemote := newSession()
	defer retry.Close(errors.New("test complete"))
	defer retryRemote.Close()
	c.respondToSearch("requester", 77, "track")
	select {
	case wire := <-retry.writes:
		var response peer.FileSearchResponse
		if err := response.Deserialize(bytes.NewReader(wire)); err != nil {
			t.Fatalf("deserialize retry response: %v", err)
		}
		if response.Token != 77 || len(response.Results) != 1 {
			t.Fatalf("retry response token=%d results=%d", response.Token, len(response.Results))
		}
	case <-time.After(time.Second):
		t.Fatal("search response failure was incorrectly marked delivered; retry produced no response")
	}
}

func TestShareSearchDeliveryConcurrentDuplicatesQueueOnce(t *testing.T) {
	c := New(Config{Username: "me"}, testLogger())
	c.shares.Store(&shareSnapshot{
		files:  map[string]*indexedFile{},
		search: []*indexedFile{{virtual: `Music\track.flac`, wire: peer.File{Name: `Music\track.flac`, Size: 4, Extension: "flac"}}},
	})
	startSessionLifecycle(t, c)
	local, remote := net.Pipe()
	defer remote.Close()
	session := c.newSession(local, sessionKey{username: "duplicate", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
	defer session.Close(errors.New("test complete"))
	if _, inserted := c.sessions.Register(session); !inserted {
		t.Fatal("register duplicate session")
	}

	start := make(chan struct{})
	var callers sync.WaitGroup
	for i := 0; i < 32; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			c.respondToSearch("duplicate", 88, "track")
		}()
	}
	close(start)
	callers.Wait()
	waitForShareWorkers(t, c)
	if got, want := len(session.writes), 1; got != want {
		t.Fatalf("concurrent duplicate responses queued = %d, want %d", got, want)
	}
}

func waitForShareWorkers(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(c.shareWorkers) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(c.shareWorkers); got != 0 {
		t.Fatalf("share workers still active: %d", got)
	}
}

func mustFolderRequestWire(t *testing.T, token soul.Token, folder string) []byte {
	t.Helper()
	msg := &peer.FolderContentsRequest{Token: token, Folder: folder}
	wire, err := msg.Serialize(msg)
	if err != nil {
		t.Fatal(err)
	}
	return wire
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
