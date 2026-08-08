package soulseek

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/distributed"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/peer"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/server"
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
	if len(snapshot.trigrams) == 0 || len(snapshot.match("album track", 10, nil)) != 1 {
		t.Fatal("published snapshot is missing its share search index")
	}
	if snapshot.files[`Music\Album\track.flac`] == nil || snapshot.files[`Music\README`] == nil {
		t.Fatalf("virtual files missing: %#v", snapshot.files)
	}
	for public, indexed := range snapshot.files {
		if strings.Contains(public, root) || strings.Contains(indexed.wire.Name, root) {
			t.Fatalf("local root leaked in public data: %q / %q", public, indexed.wire.Name)
		}
		if got, want := indexed.virtualLower, strings.ToLower(indexed.virtual); got != want {
			t.Fatalf("cached lowercase virtual path = %q, want %q", got, want)
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

// TestRescanSharesLogsFinalSummary asserts RescanShares logs a summary line
// with the directory/file counts and a duration once the snapshot is stored.
func TestRescanSharesLogsFinalSummary(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Album", "track.flac"), []byte("not really flac"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, logger)

	stats, err := c.RescanShares(context.Background())
	if err != nil {
		t.Fatalf("RescanShares: %v", err)
	}

	output := logBuf.String()
	if !strings.Contains(output, "shares scanned") {
		t.Fatalf("expected final summary log, got %q", output)
	}
	if !strings.Contains(output, fmt.Sprintf("directories=%d", stats.Directories)) {
		t.Fatalf("expected directories=%d in log, got %q", stats.Directories, output)
	}
	if !strings.Contains(output, fmt.Sprintf("files=%d", stats.Files)) {
		t.Fatalf("expected files=%d in log, got %q", stats.Files, output)
	}
	if !strings.Contains(output, "duration=") {
		t.Fatalf("expected duration= in log, got %q", output)
	}
}

// TestRescanSharesLogsPeriodicProgress asserts scanShares logs at least one
// throttled progress line while walking a share, using a near-zero log
// interval so the assertion is deterministic without sleeps or a fake clock.
func TestRescanSharesLogsPeriodicProgress(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("track%d.flac", i)
		if err := os.WriteFile(filepath.Join(root, "Album", name), []byte("not really flac"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	c := New(Config{
		SharedFolders:        []SharedFolder{{Name: "Music", Path: root}},
		shareScanLogInterval: time.Nanosecond,
	}, logger)

	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatalf("RescanShares: %v", err)
	}

	output := logBuf.String()
	if !strings.Contains(output, "share scan in progress") {
		t.Fatalf("expected periodic progress log, got %q", output)
	}
	if !strings.Contains(output, "share=Music") {
		t.Fatalf("expected share= in progress log, got %q", output)
	}
	if !strings.Contains(output, "directories=") {
		t.Fatalf("expected directories= in progress log, got %q", output)
	}
	if !strings.Contains(output, "files=") {
		t.Fatalf("expected files= in progress log, got %q", output)
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
	c.shares.Store(newTestShareSnapshot(t, `Music\track.flac`))
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

// TestShareSearchDeliverPoolSaturationDoesNotBlockMatch locks the match/deliver
// split: with the network-bound deliver pool fully saturated, a matching search
// must still run its match and release the match slot (the two are separate
// pools), dropping only the delivery — never blocking or dropping at the match
// stage the way the old single shared pool did.
func TestShareSearchDeliverPoolSaturationDoesNotBlockMatch(t *testing.T) {
	c := New(Config{Username: "me"}, testLogger())
	c.shares.Store(newTestShareSnapshot(t, `Music\track.flac`))
	startSessionLifecycle(t, c)

	// A registered session the delivery would use if it were not dropped.
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = remote.Close() })
	s := c.newSession(local, sessionKey{username: "downloader", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
	if _, inserted := c.sessions.Register(s); !inserted {
		t.Fatal("register downloader")
	}

	// Saturate the delivery pool so no match can be delivered.
	for i := 0; i < cap(c.deliverWorkers); i++ {
		c.deliverWorkers <- struct{}{}
	}

	c.respondToSearch("downloader", 42, "track")

	// The match slot must drain back to empty despite the saturated deliver pool.
	deadline := time.Now().Add(time.Second)
	for len(c.shareWorkers) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(c.shareWorkers); got != 0 {
		t.Fatalf("match slot still held (%d) while the deliver pool is saturated — pools not decoupled", got)
	}

	// The response was dropped, not delivered, because the deliver pool was full.
	select {
	case <-s.writes:
		t.Fatal("delivered a search response despite a saturated deliver pool")
	default:
	}

	for i := 0; i < cap(c.deliverWorkers); i++ {
		<-c.deliverWorkers
	}
}

func TestShareSearchDeliveryFailureCanRetry(t *testing.T) {
	c := New(Config{Username: "me"}, testLogger())
	c.shares.Store(newTestShareSnapshot(t, `Music\track.flac`))
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
	c.shares.Store(newTestShareSnapshot(t, `Music\track.flac`))
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

// waitForShareWorkers waits until all in-flight share-search work has finished.
// Since the match/deliver split, that means both pools must drain: the match
// slot is released before delivery, so a drained match pool alone no longer
// implies the response has been queued.
func waitForShareWorkers(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for (len(c.shareWorkers) != 0 || len(c.deliverWorkers) != 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(c.shareWorkers); got != 0 {
		t.Fatalf("share (match) workers still active: %d", got)
	}
	if got := len(c.deliverWorkers); got != 0 {
		t.Fatalf("deliver workers still active: %d", got)
	}
}

// startShareScanLifecycle starts c's lifecycle for share-scan tests that
// never open a peer listener (so stopLifecycle's net.Listener argument does
// not apply), and registers a cleanup that cancels it. Cancellation alone is
// enough here: it makes any shareScanHook blocked in a background scan
// observe ctx.Done() and return, which frees the share-scan slot via
// scanAndPublish's deferred releaseShareScan - so no goroutine started by a
// test using this helper can be left blocked past the test, even on an early
// t.Fatal.
func startShareScanLifecycle(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := c.beginLifecycle(ctx); err != nil {
		t.Fatalf("beginLifecycle: %v", err)
	}
	t.Cleanup(cancel)
}

// blockShareScan installs a shareScanHook on c that blocks until release is
// called (or ctx is done), reporting on entered once the hook has started.
// release is idempotent and registered as a cleanup, so a scan left blocked
// by a test that fails before calling it itself is still released.
func blockShareScan(t *testing.T, c *Client) (entered <-chan struct{}, release func()) {
	t.Helper()
	releaseCh := make(chan struct{})
	enteredCh := make(chan struct{}, 1)
	var once sync.Once
	releaseFn := func() { once.Do(func() { close(releaseCh) }) }
	c.cfg.shareScanHook = func(ctx context.Context) error {
		select {
		case enteredCh <- struct{}{}:
		default:
		}
		select {
		case <-releaseCh:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(releaseFn)
	return enteredCh, releaseFn
}

// waitForShareScanSlotFree polls until the share-scan slot can be claimed,
// then immediately releases it again - proving the slot was free rather than
// merely leaving it held for the caller.
func waitForShareScanSlotFree(t *testing.T, c *Client, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !c.tryAcquireShareScan() {
		if time.Now().After(deadline) {
			t.Fatalf("share-scan slot never freed within %v, len(shareScanSem) = %d", timeout, len(c.shareScanSem))
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.releaseShareScan()
}

// waitForScanning polls c.ShareReport().Scanning until it equals want.
func waitForScanning(t *testing.T, c *Client, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := c.ShareReport().Scanning; got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Scanning = %v, last ShareReport = %+v", want, c.ShareReport())
		}
		time.Sleep(5 * time.Millisecond)
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

func newTestShareSnapshot(tb testing.TB, names ...string) *shareSnapshot {
	tb.Helper()
	s := &shareSnapshot{files: map[string]*indexedFile{}}
	for _, name := range names {
		indexed := &indexedFile{
			virtual:      name,
			virtualLower: strings.ToLower(name),
			wire:         peer.File{Name: name, Size: 4, Extension: extensionOf(name)},
		}
		s.files[name] = indexed
		s.search = append(s.search, indexed)
	}
	sort.Slice(s.search, func(i, j int) bool { return s.search[i].virtualLower < s.search[j].virtualLower })
	var err error
	s.trigrams, err = buildShareTrigramIndex(context.Background(), s.search)
	if err != nil {
		tb.Fatalf("build test share index: %v", err)
	}
	return s
}

func TestShareSnapshotMatch(t *testing.T) {
	s := newTestShareSnapshot(t,
		`Music\Artist\Keep.FLAC`,
		`Music\Artist\Demo.MP3`,
		`Music\Other\Keep Demo.ogg`,
	)
	tests := []struct {
		name  string
		query string
		limit int
		want  []string
	}{
		{name: "case insensitive includes", query: "ARTIST KEEP", limit: 10, want: []string{`Music\Artist\Keep.FLAC`}},
		{name: "slash normalized", query: "music/artist/demo", limit: 10, want: []string{`Music\Artist\Demo.MP3`}},
		{name: "exclude overrides include", query: "keep -other", limit: 10, want: []string{`Music\Artist\Keep.FLAC`}},
		{name: "duplicate terms", query: "artist artist keep -demo -demo", limit: 10, want: []string{`Music\Artist\Keep.FLAC`}},
		{name: "all includes required", query: "artist missing", limit: 10},
		{name: "include required", query: "-demo", limit: 10},
		{name: "positive limit", query: "music", limit: 1, want: []string{`Music\Artist\Demo.MP3`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := s.match(tt.query, tt.limit, nil)
			if len(matches) != len(tt.want) {
				t.Fatalf("match count = %d, want %d: %#v", len(matches), len(tt.want), matches)
			}
			for i, want := range tt.want {
				if matches[i].Name != want {
					t.Fatalf("match %d = %q, want %q", i, matches[i].Name, want)
				}
			}
		})
	}
}

// TestShareSnapshotMatchExcludedPhrases locks the protocol rule for the
// server's excluded-search-phrase list (#324): a file whose *path* contains a
// phrase as a case-insensitive substring must not appear in a search response,
// while everything else is untouched - including when the list is absent, which
// is the state of a client that has just connected.
func TestShareSnapshotMatchExcludedPhrases(t *testing.T) {
	s := newTestShareSnapshot(t,
		`Music\A Bryan Adams\Summer.flac`,
		`Music\B Keep\Track.flac`,
	)
	phrases := func(list ...string) *[]string { return &list }
	tests := []struct {
		name     string
		query    string
		limit    int
		excluded *[]string
		want     []string
	}{
		{
			name: "no list yet", query: "music", limit: 10, excluded: nil,
			want: []string{`Music\A Bryan Adams\Summer.flac`, `Music\B Keep\Track.flac`},
		},
		{
			name: "empty list", query: "music", limit: 10, excluded: phrases(),
			want: []string{`Music\A Bryan Adams\Summer.flac`, `Music\B Keep\Track.flac`},
		},
		{
			name: "phrase in path is dropped", query: "music", limit: 10, excluded: phrases("bryan adams"),
			want: []string{`Music\B Keep\Track.flac`},
		},
		{
			name: "matching is case insensitive", query: "music", limit: 10, excluded: phrases("BRYAN Adams"),
			want: []string{`Music\B Keep\Track.flac`},
		},
		{
			name: "non-matching phrase changes nothing", query: "music", limit: 10, excluded: phrases("village people"),
			want: []string{`Music\A Bryan Adams\Summer.flac`, `Music\B Keep\Track.flac`},
		},
		{
			// The phrase covers the query but no path, so nothing is dropped:
			// the rule is about paths, not about the incoming query (#319 is
			// the query-side heuristic and has different semantics).
			name: "phrase matched against path not query", query: "music", limit: 10, excluded: phrases("music"),
			want: nil,
		},
		{
			// An excluded file must not consume a result slot, or a full
			// response would silently shrink.
			name: "excluded file does not spend the limit", query: "music", limit: 1, excluded: phrases("bryan adams"),
			want: []string{`Music\B Keep\Track.flac`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := s.match(tt.query, tt.limit, tt.excluded)
			if len(matches) != len(tt.want) {
				t.Fatalf("match count = %d, want %d: %#v", len(matches), len(tt.want), matches)
			}
			for i, want := range tt.want {
				if matches[i].Name != want {
					t.Fatalf("match %d = %q, want %q", i, matches[i].Name, want)
				}
			}
		})
	}
}

// TestRespondToSearchHonoursExcludedPhrases proves the filter is wired into the
// live response path, not just available on the snapshot: with every match
// excluded there is nothing left to send, so no response reaches the searcher.
func TestRespondToSearchHonoursExcludedPhrases(t *testing.T) {
	c := New(Config{Username: "me"}, testLogger())
	c.shares.Store(newTestShareSnapshot(t, `Music\Bryan Adams\track.flac`))
	startSessionLifecycle(t, c)

	local, remote := net.Pipe()
	t.Cleanup(func() { _ = remote.Close() })
	session := c.newSession(local, sessionKey{username: "requester", connType: peer.ConnectionType}, sessionInitiatorRemote, sessionRoleOrdinary, 0, nil)
	if _, inserted := c.sessions.Register(session); !inserted {
		t.Fatal("register requester")
	}

	phrases := []string{"bryan adams"}
	c.excludedPhrases.Store(&phrases)
	c.respondToSearch("requester", 21, "track")

	deadline := time.Now().Add(time.Second)
	for len(c.shareWorkers) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-session.writes:
		t.Fatal("responded with a file whose path contains an excluded phrase")
	default:
	}
}

func linearShareSnapshotMatch(s *shareSnapshot, query string, limit int) []peer.File {
	if limit <= 0 {
		return nil
	}
	include, exclude := parseShareSearchQuery(query)
	if len(include) == 0 {
		return nil
	}
	results := make([]peer.File, 0, min(limit, len(s.search)))
	for _, indexed := range s.search {
		if matchesShareSearch(indexed, include, exclude) {
			results = append(results, indexed.wire)
			if len(results) == limit {
				break
			}
		}
	}
	return results
}

func TestParseShareSearchQueryDeduplicatesTerms(t *testing.T) {
	include, exclude := parseShareSearchQuery("Music music MUSIC -demo -DEMO")
	if fmt.Sprint(include) != "[music]" || fmt.Sprint(exclude) != "[demo]" {
		t.Fatalf("include=%v exclude=%v", include, exclude)
	}
}

func TestShareSnapshotIndexedMatchEquivalentToLinear(t *testing.T) {
	s := newTestShareSnapshot(t,
		`Music\Artist\Keep.FLAC`,
		`Music\Artist\Demo.MP3`,
		`Music\Other\Keep Demo.ogg`,
		`Music\Aaaa\bananana--live.flac`,
		`Music\Ångström\Café.track`,
		`Music\Path\foo.bar-baz`,
	)
	tests := []struct {
		query string
		limit int
	}{
		{query: "ARTIST KEEP", limit: 10},
		{query: "music/artist/demo", limit: 10},
		{query: "keep -other", limit: 10},
		{query: "keep keep", limit: 10},
		{query: "aaaa", limit: 10},
		{query: "bananana", limit: 10},
		{query: ".flac", limit: 10},
		{query: `artist\keep`, limit: 10},
		{query: "foo.bar-baz", limit: 10},
		{query: "ÅNGSTRÖM CAFÉ", limit: 10},
		{query: "é", limit: 10},
		{query: "é track", limit: 10},
		{query: "mu ar", limit: 10},
		{query: "missing", limit: 10},
		{query: "keep -demo -other", limit: 10},
		{query: "-demo", limit: 10},
		{query: "music", limit: 2},
		{query: "music", limit: 0},
		{query: "music", limit: -1},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/limit=%d", tt.query, tt.limit), func(t *testing.T) {
			got := s.match(tt.query, tt.limit, nil)
			want := linearShareSnapshotMatch(s, tt.query, tt.limit)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("indexed matches = %#v, linear matches = %#v", got, want)
			}
		})
	}

	// A two-byte UTF-8 term has no trigrams and must not depend on the index.
	withoutIndex := *s
	withoutIndex.trigrams = nil
	if got, want := withoutIndex.match("é", 10, nil), linearShareSnapshotMatch(s, "é", 10); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("short-term fallback = %#v, want %#v", got, want)
	}
}

func TestBuildShareTrigramIndexPostingsUseFinalPositions(t *testing.T) {
	s := newTestShareSnapshot(t,
		`Music\Zulu\aaaaa.flac`,
		`Music\Alpha\aaaa.flac`,
		`Music\Middle\banana.flac`,
	)
	posting := s.trigrams[packShareTrigram("aaa", 0)]
	if cap(posting) != len(posting) {
		t.Fatalf("posting retains excess capacity: len=%d cap=%d", len(posting), cap(posting))
	}
	if len(posting) != 2 {
		t.Fatalf("aaa posting = %v, want one position for each of two files", posting)
	}
	for i, id := range posting {
		if int(id) >= len(s.search) {
			t.Fatalf("posting id %d is outside search", id)
		}
		if i > 0 && posting[i-1] >= id {
			t.Fatalf("posting is not strictly ascending: %v", posting)
		}
		if !strings.Contains(s.search[id].virtualLower, "aaa") {
			t.Fatalf("posting id %d points to %q without trigram", id, s.search[id].virtual)
		}
	}
	if s.search[posting[0]].virtual != `Music\Alpha\aaaa.flac` || s.search[posting[1]].virtual != `Music\Zulu\aaaaa.flac` {
		t.Fatalf("posting does not use final sorted positions: %v", posting)
	}
}

func TestBuildShareTrigramIndexCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := buildShareTrigramIndex(ctx, newTestShareSnapshot(t, `Music\track.flac`).search)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("build error = %v, want context.Canceled", err)
	}
}

func TestShareSnapshotConcurrentIndexedMatch(t *testing.T) {
	s := newTestShareSnapshot(t,
		`Music\Artist\Keep.FLAC`,
		`Music\Artist\Demo.MP3`,
		`Music\Other\Keep Demo.ogg`,
	)
	want := fmt.Sprint(s.match("music keep -other", 10, nil))
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if got := fmt.Sprint(s.match("music keep -other", 10, nil)); got != want {
					t.Errorf("concurrent match = %s, want %s", got, want)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestShareSearchAndFolderLookup(t *testing.T) {
	s := newTestShareSnapshot(t, `Music\Artist\keep.flac`, `Music\Artist\demo.mp3`)
	s.directories = []peer.Directory{{Name: `Music\Artist`, Files: []peer.File{{Name: "keep.flac"}}}, {Name: `Music\Artist\Disc 2`}}
	matches := s.match("ARTIST -demo", 10, nil)
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

func TestFolderResponsePrefixBoundaries(t *testing.T) {
	names := []string{`Music`, `Music (Live)`, `Music!`, `Music\A`, `Music\A\B`, `MusicVideos`, `Other`}
	// Sort exactly as scanShares does.
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	s := &shareSnapshot{}
	for _, name := range names {
		s.directories = append(s.directories, peer.Directory{Name: name})
	}

	tests := []struct {
		request string
		want    []string
	}{
		{request: "Music", want: []string{`Music`, `Music\A`, `Music\A\B`}},
		{request: "MUSIC", want: nil},
		{request: "Zzz", want: nil},
		{request: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.request, func(t *testing.T) {
			response := s.folderResponse(1, tt.request)
			var got []string
			for _, directory := range response.Folders {
				got = append(got, directory.Name)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("folderResponse(%q) folders = %v, want %v", tt.request, got, tt.want)
			}
		})
	}
}

func linearFolderResponse(s *shareSnapshot, token soul.Token, requested string) *peer.FolderContentsResponse {
	response := &peer.FolderContentsResponse{Token: token, Folder: requested}
	normalized, ok := normalizeVirtualPath(requested)
	if !ok {
		return response
	}
	prefix := normalized + `\`
	for _, directory := range s.directories {
		if directory.Name == normalized || strings.HasPrefix(directory.Name, prefix) {
			response.Folders = append(response.Folders, directory)
		}
	}
	return response
}

func TestFolderResponseEquivalentToLinearScan(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	segments := []string{"Music", "music", "MUSIC", "Artist", "artist", "Live", "A", "B", "Videos", "Other", "z"}
	randomSegment := func() string { return segments[rng.Intn(len(segments))] }

	s := &shareSnapshot{}
	seen := map[string]bool{}
	for len(s.directories) < 200 {
		depth := 1 + rng.Intn(3)
		parts := make([]string, depth)
		for i := range parts {
			parts[i] = randomSegment()
		}
		name := strings.Join(parts, `\`)
		if seen[name] {
			continue
		}
		seen[name] = true
		s.directories = append(s.directories, peer.Directory{Name: name})
	}
	sort.Slice(s.directories, func(i, j int) bool {
		return strings.ToLower(s.directories[i].Name) < strings.ToLower(s.directories[j].Name)
	})

	probes := make([]string, 0, len(s.directories)+20)
	for _, directory := range s.directories {
		probes = append(probes, directory.Name)
	}
	for range 20 {
		depth := 1 + rng.Intn(3)
		parts := make([]string, depth)
		for i := range parts {
			parts[i] = randomSegment()
		}
		probes = append(probes, strings.Join(parts, `\`))
	}

	for _, probe := range probes {
		got := s.folderResponse(1, probe)
		want := linearFolderResponse(s, 1, probe)
		if fmt.Sprint(got.Folders) != fmt.Sprint(want.Folders) {
			t.Fatalf("folderResponse(%q) = %#v, want %#v (linear)", probe, got.Folders, want.Folders)
		}
	}
}

// TestRunInitialShareScanRetriesThenPublishes locks issue #112's background
// scan helper directly, with no server connection involved: a failing hook
// must be retried with backoff (bounded here, not open-ended) before the
// scan succeeds and publishes the snapshot, and the skipped announce (no
// server connection, generation 0) must not itself cause an error loop.
func TestRunInitialShareScanRetriesThenPublishes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "track.flac"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	c.cfg.backoffBase = 2 * time.Millisecond
	c.cfg.backoffCap = 5 * time.Millisecond

	const failuresWanted = 2
	var attempts int
	c.cfg.shareScanHook = func(context.Context) error {
		attempts++
		if attempts <= failuresWanted {
			return errors.New("simulated scan failure")
		}
		return nil
	}

	start := time.Now()
	done := make(chan struct{})
	go func() {
		c.runInitialShareScan(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runInitialShareScan did not return after the scan succeeded")
	}

	if attempts != failuresWanted+1 {
		t.Fatalf("attempts = %d, want %d (failures then one success)", attempts, failuresWanted+1)
	}
	// Backoff must actually have been applied: failuresWanted waits of at
	// least backoffBase each.
	if elapsed := time.Since(start); elapsed < time.Duration(failuresWanted)*c.cfg.backoffBase {
		t.Fatalf("elapsed = %v, want at least %v (backoff applied between retries)", elapsed, time.Duration(failuresWanted)*c.cfg.backoffBase)
	}

	snapshot := c.shareSnapshot()
	if snapshot.stats.Files != 1 || snapshot.stats.Directories != 1 {
		t.Fatalf("published stats = %+v, want 1 file/1 directory", snapshot.stats)
	}
	if snapshot.files[`Music\track.flac`] == nil {
		t.Fatalf("virtual file missing after scan: %#v", snapshot.files)
	}
}

// failSerializeTooLarge makes the browse-frame serializer fail the way it does
// for a share too big to publish, and restores it when the test ends.
//
// The error is produced by the real serializer, not hand-written: what has to
// hold is that scanShares classifies the error peer actually returns at its
// limit, and a fake error string would keep passing if that shape ever
// changed. Only the *input* is synthetic - a share index that reaches the
// limit for real needs around 170k files on disk (issue #408), while 17
// maximum-length names reach it in memory - so the substitution costs nothing
// the classification depends on.
func failSerializeTooLarge(t *testing.T) {
	t.Helper()
	files := make([]peer.File, 17)
	for i := range files {
		files[i].Name = strings.Repeat("a", 1<<20)
	}
	oversized := &peer.SharedFileListResponse{Directories: []peer.Directory{{Name: "Music", Files: files}}}
	// Serialized once and the error reused: reaching the limit means actually
	// writing 16 MiB, which is the bulk of this test's runtime.
	_, tooLarge := oversized.Serialize(oversized)
	// Asserted, not assumed: if peer's limit ever rises above this fixture,
	// the seam would start returning success and every test using it would
	// pass for the wrong reason.
	if !errors.Is(tooLarge, soul.ErrMessageTooLarge) {
		t.Fatalf("fixture no longer exceeds the serializer's limit (err = %v); raise the file count", tooLarge)
	}

	restore := serializeSharedFileList
	serializeSharedFileList = func([]peer.Directory) ([]byte, error) { return nil, tooLarge }
	t.Cleanup(func() { serializeSharedFileList = restore })
}

// TestRunInitialShareScanStopsAfterPermanentFailure locks issue #408. A share
// index that does not fit in a browse response fails identically on every
// attempt, so the background scan must give up after exactly one, rather than
// re-walking every root and re-allocating the whole snapshot forever. The
// error it leaves behind has to name the file count and the limit: the
// underlying protocol error says only that some message was too big, which
// tells a user nothing about their library.
func TestRunInitialShareScanStopsAfterPermanentFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "track.flac"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	// Deliberately long: if the terminal classification regresses, the second
	// attempt cannot arrive before the deadline below, so this fails loudly
	// instead of quietly taking one extra lap and still passing.
	c.cfg.backoffBase = time.Hour
	c.cfg.backoffCap = time.Hour
	failSerializeTooLarge(t)

	var attempts int
	c.cfg.shareScanHook = func(context.Context) error {
		attempts++
		return nil
	}

	done := make(chan struct{})
	go func() {
		c.runInitialShareScan(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runInitialShareScan did not return after a permanent failure; it is still retrying")
	}

	if attempts != 1 {
		t.Fatalf("scan attempts = %d, want exactly 1 (a permanent failure is not retried)", attempts)
	}

	report := c.ShareReport()
	for _, want := range []string{"1 shared files", "16777216", "sharing is disabled"} {
		if !strings.Contains(report.LastError, want) {
			t.Fatalf("LastError = %q, want it to contain %q", report.LastError, want)
		}
	}
	if report.LastErrorAt.IsZero() {
		t.Fatal("LastErrorAt is zero after a permanent failure")
	}
	// Nothing was published, so the rest of slusk sees the empty index it
	// started with - not a half-built one.
	if report.Files != 0 || report.Directories != 0 {
		t.Fatalf("published stats = %+v, want the empty index (nothing is publishable)", report.ShareStats)
	}
}

// TestScanAndPublishClearsPermanentFailureOnSuccess covers the other
// direction of issue #408's failure field: once a scan succeeds - the user
// removed a share, or a rescan was triggered after fixing the config - the
// recorded failure must go, or /status and the Shares view would keep
// explaining a problem that no longer exists.
func TestScanAndPublishClearsPermanentFailureOnSuccess(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "track.flac"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())

	failSerializeTooLarge(t)
	if _, err := c.scanAndPublish(context.Background()); !errors.Is(err, ErrShareTooLarge) {
		t.Fatalf("scanAndPublish error = %v, want ErrShareTooLarge", err)
	}
	if c.ShareReport().LastError == "" {
		t.Fatal("LastError is empty after a permanent failure")
	}

	restore := serializeSharedFileList
	serializeSharedFileList = func(directories []peer.Directory) ([]byte, error) {
		msg := &peer.SharedFileListResponse{Directories: directories}
		return msg.Serialize(msg)
	}
	t.Cleanup(func() { serializeSharedFileList = restore })

	if _, err := c.scanAndPublish(context.Background()); err != nil {
		t.Fatalf("scanAndPublish after recovery: %v", err)
	}
	report := c.ShareReport()
	if report.LastError != "" || !report.LastErrorAt.IsZero() {
		t.Fatalf("failure still reported after a successful scan: %q at %v", report.LastError, report.LastErrorAt)
	}
	if report.Files != 1 {
		t.Fatalf("published files = %d, want 1", report.Files)
	}
}

// TestScanAndPublishDoesNotRecordTransientFailures guards the branch the
// permanent classification must not swallow: a transient scan error is still
// retried by runInitialShareScan (TestRunInitialShareScanRetriesThenPublishes
// covers that), and it must not leave a "sharing is disabled" reading behind,
// because sharing is not disabled - the previous index is still live and the
// next attempt may well succeed.
func TestScanAndPublishDoesNotRecordTransientFailures(t *testing.T) {
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: t.TempDir()}}}, testLogger())
	c.cfg.shareScanHook = func(context.Context) error { return errors.New("simulated I/O failure") }

	if _, err := c.scanAndPublish(context.Background()); err == nil {
		t.Fatal("scanAndPublish succeeded, want the injected failure")
	} else if errors.Is(err, ErrShareTooLarge) {
		t.Fatalf("transient error classified as permanent: %v", err)
	}
	if report := c.ShareReport(); report.LastError != "" {
		t.Fatalf("LastError = %q, want empty (a transient failure is not a permanent one)", report.LastError)
	}
}

// TestRescanSharesReportsPerFolderStats locks issue #160's per-folder
// breakdown: two shares with known file sizes, plus an escaping symlink that
// must contribute neither files nor bytes to either total.
func TestRescanSharesReportsPerFolderStats(t *testing.T) {
	musicRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(musicRoot, "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(musicRoot, "Album", "track.flac"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(musicRoot, "README"), []byte("abcde"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.mp3")
	if err := os.WriteFile(outside, []byte("this must not count"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(musicRoot, "escape.mp3")); err != nil {
		t.Fatal(err)
	}

	booksRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(booksRoot, "novel.epub"), []byte("0123456789012345"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := New(Config{SharedFolders: []SharedFolder{
		{Name: "Music", Path: musicRoot},
		{Name: "Books", Path: booksRoot},
	}}, testLogger())
	stats, err := c.RescanShares(context.Background())
	if err != nil {
		t.Fatalf("RescanShares: %v", err)
	}

	snapshot := c.shareSnapshot()
	if len(snapshot.folders) != 2 {
		t.Fatalf("folders = %#v, want 2", snapshot.folders)
	}
	byName := make(map[string]ShareFolderStats, len(snapshot.folders))
	for _, f := range snapshot.folders {
		byName[f.Name] = f
	}

	music, ok := byName["Music"]
	if !ok {
		t.Fatalf("missing Music folder stats: %#v", byName)
	}
	if music.Path != musicRoot {
		t.Errorf("Music.Path = %q, want %q", music.Path, musicRoot)
	}
	// root dir + Album dir = 2 directories; track.flac + README = 2 files,
	// 10 + 5 = 15 bytes. escape.mp3 (symlink) contributes nothing.
	if music.Directories != 2 || music.Files != 2 || music.TotalBytes != 15 {
		t.Fatalf("Music stats = %#v, want directories=2 files=2 totalBytes=15", music)
	}

	books, ok := byName["Books"]
	if !ok {
		t.Fatalf("missing Books folder stats: %#v", byName)
	}
	if books.Path != booksRoot {
		t.Errorf("Books.Path = %q, want %q", books.Path, booksRoot)
	}
	if books.Directories != 1 || books.Files != 1 || books.TotalBytes != 16 {
		t.Fatalf("Books stats = %#v, want directories=1 files=1 totalBytes=16", books)
	}

	var sumDirs, sumFiles int
	var sumBytes uint64
	for _, f := range snapshot.folders {
		sumDirs += f.Directories
		sumFiles += f.Files
		sumBytes += f.TotalBytes
	}
	if sumDirs != len(snapshot.directories) {
		t.Errorf("sum(folders.Directories) = %d, want %d (len(snapshot.directories))", sumDirs, len(snapshot.directories))
	}
	if sumFiles != len(snapshot.search) {
		t.Errorf("sum(folders.Files) = %d, want %d (len(snapshot.search))", sumFiles, len(snapshot.search))
	}
	if sumBytes != stats.TotalBytes {
		t.Errorf("sum(folders.TotalBytes) = %d, want %d (stats.TotalBytes)", sumBytes, stats.TotalBytes)
	}
	if stats.TotalBytes != 31 {
		t.Errorf("stats.TotalBytes = %d, want 31", stats.TotalBytes)
	}
}

// TestRescanSharesRecordsIndexTimeAndDuration locks issue #160's timing
// fields: IndexedAt falls within the scan's wall-clock window, ScanDuration
// is positive, and a second rescan advances IndexedAt.
func TestRescanSharesRecordsIndexTimeAndDuration(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "track.flac"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())

	before := time.Now()
	stats, err := c.RescanShares(context.Background())
	after := time.Now()
	if err != nil {
		t.Fatalf("RescanShares: %v", err)
	}
	if stats.IndexedAt.Before(before) || stats.IndexedAt.After(after) {
		t.Fatalf("IndexedAt = %v, want between %v and %v", stats.IndexedAt, before, after)
	}
	if stats.ScanDuration <= 0 {
		t.Fatalf("ScanDuration = %v, want > 0", stats.ScanDuration)
	}

	time.Sleep(time.Millisecond)
	stats2, err := c.RescanShares(context.Background())
	if err != nil {
		t.Fatalf("second RescanShares: %v", err)
	}
	if !stats2.IndexedAt.After(stats.IndexedAt) {
		t.Fatalf("second IndexedAt = %v, want after first %v", stats2.IndexedAt, stats.IndexedAt)
	}
}

// TestTriggerRescanSharesRejectsConcurrentScan locks issue #160's try-lock
// semantics: while a scan is blocked in shareScanHook, a second
// TriggerRescanShares call must not queue behind it - it must fail fast with
// ErrShareScanInProgress.
func TestTriggerRescanSharesRejectsConcurrentScan(t *testing.T) {
	root := t.TempDir()
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	startShareScanLifecycle(t, c)
	entered, release := blockShareScan(t, c)

	if err := c.TriggerRescanShares(); err != nil {
		t.Fatalf("first TriggerRescanShares: %v", err)
	}

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scan to start")
	}

	if err := c.TriggerRescanShares(); !errors.Is(err, ErrShareScanInProgress) {
		t.Fatalf("second TriggerRescanShares err = %v, want ErrShareScanInProgress", err)
	}

	release()
	// Draining: the slot must eventually free up once the background scan
	// finishes, proving the first call's slot was not leaked.
	waitForShareScanSlotFree(t, c, 2*time.Second)
}

// TestAcquireShareScanReturnsCtxErrWhenCancelled locks new-with-#160
// behaviour: acquireShareScan (and therefore scanAndPublish/RescanShares) can
// now return ctx.Err() while waiting for the share-scan slot, instead of
// blocking indefinitely, when the caller's ctx is done first.
func TestAcquireShareScanReturnsCtxErrWhenCancelled(t *testing.T) {
	root := t.TempDir()
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	if !c.tryAcquireShareScan() {
		t.Fatal("tryAcquireShareScan: slot unexpectedly held")
	}
	t.Cleanup(c.releaseShareScan)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.RescanShares(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RescanShares err = %v, want context.Canceled", err)
	}
}

// TestTriggerRescanSharesWhenNotRunning locks issue #160's contract for a
// client whose lifecycle was never started: TriggerRescanShares must report
// ErrClientStopped, and - critically - must release the share-scan slot it
// claimed rather than leaking it, or a subsequent blocking RescanShares would
// deadlock forever.
func TestTriggerRescanSharesWhenNotRunning(t *testing.T) {
	root := t.TempDir()
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())

	if err := c.TriggerRescanShares(); !errors.Is(err, ErrClientStopped) {
		t.Fatalf("TriggerRescanShares err = %v, want ErrClientStopped", err)
	}

	done := make(chan error, 1)
	go func() { _, err := c.RescanShares(context.Background()); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RescanShares after leaked TriggerRescanShares: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RescanShares deadlocked: TriggerRescanShares leaked the share-scan slot")
	}
}

// TestTriggerRescanSharesHappyPath locks the success path end to end: the
// background scan publishes a new snapshot and TriggerRescanShares itself
// returns immediately (well before the scan hook completes).
func TestTriggerRescanSharesHappyPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "track.flac"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	startShareScanLifecycle(t, c)

	if err := c.TriggerRescanShares(); err != nil {
		t.Fatalf("TriggerRescanShares: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for c.shareSnapshot().stats.Files != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("background scan never published; snapshot = %#v", c.shareSnapshot().stats)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAnnounceSkipsDuplicateStatsAcrossRescans guards the mandatory dedup fix
// alongside issue #160: two rescans that produce identical Directories/Files
// counts (only IndexedAt/ScanDuration differ) must still result in exactly
// one SharedFoldersFiles announcement.
func TestAnnounceSkipsDuplicateStatsAcrossRescans(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "track.flac"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{Address: "unused:0", Username: "me", Password: "p",
		SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c.serverConn = a
	c.serverGeneration = 1
	c.announcedGeneration = 1 // simulate login-time announcement already sent

	announced := make(chan error, 1)
	go func() { announced <- readUntilSharedFoldersFiles(b, 1, 1) }()

	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatalf("first RescanShares: %v", err)
	}
	select {
	case err := <-announced:
		if err != nil {
			t.Fatalf("first announcement: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first announcement")
	}

	// The extra reader is started before the second rescan (rather than
	// after) so that if the dedup fix regresses, the duplicate announcement's
	// write is drained immediately: with an unbuffered net.Pipe, starting the
	// reader only after RescanShares returns would instead have the write
	// block inside RescanShares until this test's outer timeout, reporting a
	// slow, off-message failure instead of the assertion below.
	extra := make(chan error, 1)
	go func() { extra <- readUntilSharedFoldersFiles(b, 1, 1) }()

	// Second rescan produces identical counts (same single file); IndexedAt
	// and ScanDuration necessarily differ, but the announce must still dedup.
	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatalf("second RescanShares: %v", err)
	}

	select {
	case err := <-extra:
		if err == nil {
			t.Fatal("second rescan sent a duplicate announcement, want dedup")
		}
	case <-time.After(200 * time.Millisecond):
		// No further frame arrived - the dedup held.
	}
}

// TestShareReportScanningReflectsInProgressScan locks ShareReport.Scanning:
// true while a triggered scan's hook blocks, false once it completes.
func TestShareReportScanningReflectsInProgressScan(t *testing.T) {
	root := t.TempDir()
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())
	startShareScanLifecycle(t, c)
	entered, release := blockShareScan(t, c)

	if report := c.ShareReport(); report.Scanning {
		t.Fatal("Scanning = true before any scan started")
	}

	if err := c.TriggerRescanShares(); err != nil {
		t.Fatalf("TriggerRescanShares: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scan to start")
	}

	if report := c.ShareReport(); !report.Scanning {
		t.Fatal("Scanning = false while scan hook is blocked")
	}

	release()
	waitForScanning(t, c, false, 2*time.Second)
}
