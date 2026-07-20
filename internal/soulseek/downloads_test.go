package soulseek

import (
	"net"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/pipeline"
	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul"
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
