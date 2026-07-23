package soulseek

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
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
	if len(snapshot.trigrams) == 0 || len(snapshot.match("album track", 10)) != 1 {
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
			matches := s.match(tt.query, tt.limit)
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
			got := s.match(tt.query, tt.limit)
			want := linearShareSnapshotMatch(s, tt.query, tt.limit)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("indexed matches = %#v, linear matches = %#v", got, want)
			}
		})
	}

	// A two-byte UTF-8 term has no trigrams and must not depend on the index.
	withoutIndex := *s
	withoutIndex.trigrams = nil
	if got, want := withoutIndex.match("é", 10), linearShareSnapshotMatch(s, "é", 10); fmt.Sprint(got) != fmt.Sprint(want) {
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
	want := fmt.Sprint(s.match("music keep -other", 10))
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if got := fmt.Sprint(s.match("music keep -other", 10)); got != want {
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
