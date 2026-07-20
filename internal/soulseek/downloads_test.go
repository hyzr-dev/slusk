package soulseek

import (
	"context"
	"errors"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/pipeline"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/peer"
)

// Compile-time confirmation that Client satisfies the pipeline ports Group E
// implements Enqueue/ListDownloads/Cancel/Remove/DeleteDownloadFolder for.
var (
	_ pipeline.PeerSearcher = (*Client)(nil)
	_ pipeline.PeerNetwork  = (*Client)(nil)
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

// --- downloadRegistry / transfer ---

func TestNewTransferStartsQueued(t *testing.T) {
	tr := newTransfer("id1", "alice", "song.flac", 1024)
	if tr.state != core.TransferQueued {
		t.Errorf("newTransfer state = %q, want %q", tr.state, core.TransferQueued)
	}
	if tr.id != "id1" || tr.username != "alice" || tr.filename != "song.flac" || tr.size != 1024 {
		t.Errorf("newTransfer identity fields = %+v, want id1/alice/song.flac/1024", tr)
	}
}

func TestDownloadRegistryInsertLookup(t *testing.T) {
	reg := newDownloadRegistry()
	tr := newTransfer("id1", "alice", "song.flac", 100)
	reg.insert(tr)

	if got := reg.lookupByID("id1"); got != tr {
		t.Errorf("lookupByID(id1) = %v, want %v", got, tr)
	}
	if got := reg.lookupByID("missing"); got != nil {
		t.Errorf("lookupByID(missing) = %v, want nil", got)
	}
	if got := reg.lookupByKey("alice", "song.flac"); got != tr {
		t.Errorf("lookupByKey(alice, song.flac) = %v, want %v", got, tr)
	}
	if got := reg.lookupByKey("bob", "song.flac"); got != nil {
		t.Errorf("lookupByKey(bob, song.flac) = %v, want nil", got)
	}
}

func TestDownloadRegistryRegisterAndClaimToken(t *testing.T) {
	reg := newDownloadRegistry()
	tr := newTransfer("id1", "alice", "song.flac", 100)
	reg.insert(tr)

	token := soul.Token(42)
	reg.registerToken(tr, token)

	tr.mu.Lock()
	gotToken := tr.token
	tr.mu.Unlock()
	if gotToken != token {
		t.Errorf("tr.token = %v, want %v", gotToken, token)
	}

	claimed := reg.claimByToken(token)
	if claimed != tr {
		t.Fatalf("claimByToken(%v) = %v, want %v", token, claimed, tr)
	}
	if again := reg.claimByToken(token); again != nil {
		t.Errorf("second claimByToken(%v) = %v, want nil (one-shot claim)", token, again)
	}
	if unknown := reg.claimByToken(soul.Token(999)); unknown != nil {
		t.Errorf("claimByToken(unregistered) = %v, want nil", unknown)
	}
}

func TestDownloadRegistryRemove(t *testing.T) {
	reg := newDownloadRegistry()
	tr := newTransfer("id1", "alice", "song.flac", 100)
	reg.insert(tr)
	reg.registerToken(tr, soul.Token(7))

	reg.remove(tr)

	if got := reg.lookupByID("id1"); got != nil {
		t.Errorf("lookupByID after remove = %v, want nil", got)
	}
	if got := reg.lookupByKey("alice", "song.flac"); got != nil {
		t.Errorf("lookupByKey after remove = %v, want nil", got)
	}
	if got := reg.claimByToken(soul.Token(7)); got != nil {
		t.Errorf("claimByToken after remove = %v, want nil", got)
	}
}

func TestDownloadRegistrySnapshot(t *testing.T) {
	reg := newDownloadRegistry()
	tr1 := newTransfer("id1", "alice", "a.flac", 1)
	tr2 := newTransfer("id2", "bob", "b.flac", 2)
	reg.insert(tr1)
	reg.insert(tr2)

	snap := reg.snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot length = %d, want 2", len(snap))
	}
	seen := map[string]bool{}
	for _, tr := range snap {
		seen[tr.id] = true
	}
	if !seen["id1"] || !seen["id2"] {
		t.Errorf("snapshot ids = %v, want id1 and id2", seen)
	}
}

func TestTransferAttachFileConnDeliversOnce(t *testing.T) {
	tr := newTransfer("id1", "alice", "song.flac", 100)

	first, firstPeer := net.Pipe()
	defer first.Close()
	defer firstPeer.Close()
	second, secondPeer := net.Pipe()
	defer second.Close()
	defer secondPeer.Close()

	// Two deliveries back-to-back, with nobody reading fileConnCh in between:
	// the first fills its single buffer slot, so the second must be refused
	// rather than overwrite or block.
	if !tr.attachFileConn(first, nil) {
		t.Fatal("attachFileConn (first) = false, want true")
	}
	if tr.attachFileConn(second, nil) {
		t.Error("attachFileConn (second, buffer already full) = true, want false")
	}

	select {
	case handoff := <-tr.fileConnCh:
		if handoff.conn != first {
			t.Errorf("delivered conn = %v, want the first delivery %v", handoff.conn, first)
		}
	default:
		t.Fatal("fileConnCh empty after a successful attachFileConn")
	}
}

// --- downloadSessionHooks ---

// TestDownloadHooksClaimAndDispatch exercises downloadSessionHooks.frame
// directly against handmade sessionFrames (the same technique
// TestSearchHookNonResponseCodeIsUnhandled uses), covering every code it
// owns: TransferRequest claims the token and wakes the orchestration,
// PlaceInQueueResponse updates queuePosition, and UploadFailed/UploadDenied
// deliver a correctly categorized failure. None of them may return an error
// (a claimed code returning non-nil would close the P session).
func TestDownloadHooksClaimAndDispatch(t *testing.T) {
	downloads := newDownloadRegistry()
	hook := &downloadSessionHooks{downloads: downloads, logger: testLogger()}
	session := &peerSession{key: sessionKey{username: "friend", connType: peer.ConnectionType}}

	tr := newTransfer("id1", "friend", "song.flac", 100)
	downloads.insert(tr)

	req := &peer.TransferRequest{Direction: peer.UploadToPeer, Token: soul.Token(7), Filename: "song.flac", FileSize: 100}
	wire, err := req.Serialize(req)
	if err != nil {
		t.Fatalf("serialize transfer request: %v", err)
	}
	if err := hook.frame(session, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeTransferRequest), wire: wire}); err != nil {
		t.Fatalf("frame(TransferRequest) = %v, want nil", err)
	}
	select {
	case got := <-tr.transferRequestCh:
		if got.Token != soul.Token(7) || got.Filename != "song.flac" {
			t.Errorf("delivered transfer request = %+v", got)
		}
	default:
		t.Fatal("transferRequestCh empty after a TransferRequest frame")
	}
	tr.mu.Lock()
	gotToken := tr.token
	tr.mu.Unlock()
	if gotToken != soul.Token(7) {
		t.Errorf("tr.token = %v, want 7", gotToken)
	}
	if claimed := downloads.claimByToken(soul.Token(7)); claimed != tr {
		t.Fatal("TransferRequest did not registerToken in the registry")
	}

	piq := &peer.PlaceInQueueResponse{Filename: "song.flac", Place: 3}
	wire, err = piq.Serialize(piq)
	if err != nil {
		t.Fatalf("serialize place in queue response: %v", err)
	}
	if err := hook.frame(session, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodePlaceInQueueResponse), wire: wire}); err != nil {
		t.Fatalf("frame(PlaceInQueueResponse) = %v, want nil", err)
	}
	tr.mu.Lock()
	place := tr.queuePosition
	tr.mu.Unlock()
	if place != 3 {
		t.Errorf("queuePosition = %d, want 3", place)
	}

	failed := &peer.UploadFailed{Filename: "song.flac"}
	wire, err = failed.Serialize(failed)
	if err != nil {
		t.Fatalf("serialize upload failed: %v", err)
	}
	if err := hook.frame(session, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeUploadFailed), wire: wire}); err != nil {
		t.Fatalf("frame(UploadFailed) = %v, want nil", err)
	}
	select {
	case f := <-tr.failCh:
		if !f.retryable {
			t.Error("UploadFailed retryable = false, want true")
		}
	default:
		t.Fatal("failCh empty after an UploadFailed frame")
	}

	denied := &peer.UploadDenied{Filename: "song.flac", Reason: peer.ErrFileNotShared}
	wire, err = denied.Serialize(denied)
	if err != nil {
		t.Fatalf("serialize upload denied: %v", err)
	}
	if err := hook.frame(session, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeUploadDenied), wire: wire}); err != nil {
		t.Fatalf("frame(UploadDenied) = %v, want nil", err)
	}
	select {
	case f := <-tr.failCh:
		if f.retryable {
			t.Errorf("UploadDenied(ErrFileNotShared) retryable = true, want false")
		}
		if f.reason != peer.ErrFileNotShared.Error() {
			t.Errorf("UploadDenied failure reason = %q, want %q", f.reason, peer.ErrFileNotShared.Error())
		}
	default:
		t.Fatal("failCh empty after an UploadDenied frame")
	}
}

// TestDownloadHooksTransferRequestForUnknownDownloadIsHandled covers a
// TransferRequest that arrives for a (username, filename) pair with no
// registered transfer - e.g. a stale/duplicate relay: it must still be
// treated as handled (nil), not close the P session.
func TestDownloadHooksTransferRequestForUnknownDownloadIsHandled(t *testing.T) {
	hook := &downloadSessionHooks{downloads: newDownloadRegistry()}
	session := &peerSession{key: sessionKey{username: "friend", connType: peer.ConnectionType}}
	req := &peer.TransferRequest{Direction: peer.UploadToPeer, Token: soul.Token(1), Filename: "unknown.flac", FileSize: 1}
	wire, err := req.Serialize(req)
	if err != nil {
		t.Fatalf("serialize transfer request: %v", err)
	}
	if err := hook.frame(session, sessionFrame{connType: peer.ConnectionType, code: int(peer.CodeTransferRequest), wire: wire}); err != nil {
		t.Fatalf("frame(TransferRequest for unknown download) = %v, want nil", err)
	}
}

// TestDownloadHooksUnknownCodeUnhandled locks in the claim-sentinel contract
// downloadSessionHooks must follow: it owns exactly
// {40,41,44,46,50}/TransferRequest,TransferResponse,PlaceInQueueResponse,
// UploadFailed,UploadDenied on a P connection. Any other code, or a non-P
// frame, must come back as errUnhandledPeerFrame so a sibling hook (or,
// ultimately, composedSessionHooks's own unclaimed-code rejection - see
// TestComposedSessionHooksUnknownCodeStillClosesSession in search_test.go)
// gets to decide, rather than downloadSessionHooks silently swallowing it.
func TestDownloadHooksUnknownCodeUnhandled(t *testing.T) {
	hook := &downloadSessionHooks{downloads: newDownloadRegistry()}
	session := &peerSession{key: sessionKey{username: "friend", connType: peer.ConnectionType}}
	if err := hook.frame(session, sessionFrame{connType: peer.ConnectionType, code: 0xffff}); !errors.Is(err, errUnhandledPeerFrame) {
		t.Errorf("frame(unknown P code) = %v, want errUnhandledPeerFrame", err)
	}
	if err := hook.frame(session, sessionFrame{connType: "D", code: int(peer.CodeTransferRequest)}); !errors.Is(err, errUnhandledPeerFrame) {
		t.Errorf("frame(non-P connType) = %v, want errUnhandledPeerFrame", err)
	}
}

// --- Enqueue / ListDownloads / Cancel / Remove / DeleteDownloadFolder ---

func TestEnqueueDeduplicatesByKey(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	startSessionLifecycle(t, c)

	id1, err := c.Enqueue(context.Background(), "alice", "song.flac", 100)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	id2, err := c.Enqueue(context.Background(), "alice", "song.flac", 100)
	if err != nil {
		t.Fatalf("Enqueue (duplicate): %v", err)
	}
	if id1 != id2 {
		t.Errorf("Enqueue ids = %q, %q, want identical (byKey dedupe, no second goroutine)", id1, id2)
	}
	if got := len(c.downloads.snapshot()); got != 1 {
		t.Errorf("registry size after duplicate Enqueue = %d, want 1", got)
	}
}

func TestListDownloadsMapsFields(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())

	tr := newTransfer("id1", "alice", "song.flac", 1000)
	tr.state = core.TransferInProgress
	tr.failure = "stalled"
	tr.retryable = true
	tr.queuePosition = 5
	tr.speed = 4096
	tr.bytesDone.Store(500)
	c.downloads.insert(tr)

	list, err := c.ListDownloads(context.Background())
	if err != nil {
		t.Fatalf("ListDownloads: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListDownloads length = %d, want 1", len(list))
	}
	want := core.RemoteTransfer{
		ID: "id1", Username: "alice", Filename: "song.flac",
		State: core.TransferInProgress, Size: 1000, BytesDone: 500,
		Failure: "stalled", Retryable: true, QueuePosition: 5, Speed: 4096,
	}
	if got := list[0]; got != want {
		t.Errorf("ListDownloads mapping = %+v, want %+v", got, want)
	}
}

func TestCancelAndRemoveNotFound(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	if err := c.Cancel(context.Background(), "alice", "missing"); !errors.Is(err, core.ErrRemoteNotFound) {
		t.Errorf("Cancel(missing) = %v, want wrapping core.ErrRemoteNotFound", err)
	}
	if err := c.Remove(context.Background(), "alice", "missing"); !errors.Is(err, core.ErrRemoteNotFound) {
		t.Errorf("Remove(missing) = %v, want wrapping core.ErrRemoteNotFound", err)
	}
}

func TestCancelMarksTransferCancelled(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	tr := newTransfer("id1", "alice", "song.flac", 100)
	_, cancel := context.WithCancel(context.Background())
	tr.cancel = cancel
	c.downloads.insert(tr)

	if err := c.Cancel(context.Background(), "alice", "id1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	tr.mu.Lock()
	state := tr.state
	tr.mu.Unlock()
	if state != core.TransferCancelled {
		t.Errorf("state after Cancel = %q, want %q", state, core.TransferCancelled)
	}
}

func TestRemoveDeletesPartialDownloadAndRegistryEntry(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	c.cfg.DownloadDir = t.TempDir()

	const filename = `Music\Artist - Album\01 track.flac`
	tr := newTransfer("id1", "alice", filename, 100)
	_, cancel := context.WithCancel(context.Background())
	tr.cancel = cancel
	c.downloads.insert(tr)

	partPath := downloadDestPath(c.cfg.DownloadDir, filename) + ".part"
	if err := os.MkdirAll(filepath.Dir(partPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(partPath, []byte("partial"), 0o644); err != nil {
		t.Fatalf("seed .part: %v", err)
	}

	if err := c.Remove(context.Background(), "alice", "id1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(partPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat .part after Remove: err = %v, want os.ErrNotExist", err)
	}
	if got := c.downloads.lookupByID("id1"); got != nil {
		t.Error("transfer still present in the registry after Remove")
	}
}

func TestDeleteDownloadFolder(t *testing.T) {
	c := New(Config{Address: "unused:0", Username: "me", Password: "p"}, testLogger())
	c.cfg.DownloadDir = t.TempDir()

	const leaf = "Artist - Album"
	dir := filepath.Join(c.cfg.DownloadDir, leaf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01.flac"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := c.DeleteDownloadFolder(context.Background(), leaf); err != nil {
		t.Fatalf("DeleteDownloadFolder: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat folder after delete: err = %v, want os.ErrNotExist", err)
	}

	if err := c.DeleteDownloadFolder(context.Background(), "missing"); !errors.Is(err, core.ErrRemoteNotFound) {
		t.Errorf("DeleteDownloadFolder(missing) = %v, want wrapping core.ErrRemoteNotFound", err)
	}
}
