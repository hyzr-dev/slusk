package soulseek

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
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

// promoteFirstWaiting pops the head of m.waiting and marks it active, the
// same bookkeeping dispatch does when it hands a job a slot, without
// actually running m.execute. It returns the promoted job so tests can set
// its size/sent directly.
func promoteFirstWaiting(m *uploadManager) *uploadJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.waiting[0]
	m.waiting = m.waiting[1:]
	job.active = true
	m.active++
	return job
}

func newUploadReportTestManager(t *testing.T) *uploadManager {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.flac"), []byte("a.flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, UploadSlots: 4}, testLogger())
	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatal(err)
	}
	return newUploadManager(c, 4)
}

// TestUploadReportOrdersActiveThenQueue asserts report() lists active
// uploads first (Position 0, ordered by enqueue seq) followed by the
// waiting queue in FIFO order with 1-based positions matching position().
func TestUploadReportOrdersActiveThenQueue(t *testing.T) {
	m := newUploadReportTestManager(t)
	for _, user := range []string{"alice", "bob", "carol"} {
		if err := m.enqueue(user, `Music\a.flac`); err != nil {
			t.Fatal(err)
		}
	}
	active := promoteFirstWaiting(m) // alice
	active.size.Store(1000)
	active.sent.Store(250)

	report := m.report(maxReportedUploads)
	if report.Slots != 4 || report.Active != 1 || report.Queued != 2 || report.Truncated != 0 {
		t.Fatalf("report = %+v, want Slots:4 Active:1 Queued:2 Truncated:0", report)
	}
	if report.Uploads == nil {
		t.Fatal("Uploads is nil, want non-nil")
	}
	if len(report.Uploads) != 3 {
		t.Fatalf("len(Uploads) = %d, want 3", len(report.Uploads))
	}
	first := report.Uploads[0]
	if first.Username != "alice" || !first.Active || first.Position != 0 || first.Size != 1000 || first.BytesWritten != 250 {
		t.Fatalf("Uploads[0] = %+v, want active alice with size 1000 sent 250", first)
	}
	wantWaiting := []struct {
		username string
		position uint32
	}{
		{"bob", 1},
		{"carol", 2},
	}
	for i, want := range wantWaiting {
		got := report.Uploads[i+1]
		if got.Username != want.username || got.Active || got.Position != want.position {
			t.Fatalf("Uploads[%d] = %+v, want waiting %s at position %d", i+1, got, want.username, want.position)
		}
		place, ok := m.position(uploadKey{username: got.Username, filename: `Music\a.flac`})
		if !ok || place != want.position {
			t.Fatalf("position(%s) = %d, %v, want %d, true", got.Username, place, ok, want.position)
		}
	}
}

// TestUploadReportTruncatesQueue asserts a queue longer than limit is
// truncated in Uploads while Queued still reports the full waiting count and
// Truncated reports exactly what was omitted - and that an active upload is
// never dropped by truncation, since it is bounded by slots, not limit.
func TestUploadThroughputSnapshotIsUncappedExcludesQueueAndSurvivesReset(t *testing.T) {
	m := newUploadReportTestManager(t)
	m.slots = 150
	for i := 0; i < 130; i++ {
		if err := m.enqueue(fmt.Sprintf("user%d", i), `Music\a.flac`); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 125; i++ {
		promoteFirstWaiting(m)
	}
	m.totalWritten.Store(42)

	total, active := m.throughputSnapshot()
	if total != 42 || active != 125 {
		t.Fatalf("throughput snapshot = total:%d active:%d, want 42/125", total, active)
	}
	m.mu.Lock()
	queued := len(m.waiting)
	m.mu.Unlock()
	if queued != 5 {
		t.Fatalf("queued = %d, want 5 excluded from active", queued)
	}

	m.reset()
	total, active = m.throughputSnapshot()
	if total != 42 || active != 0 {
		t.Fatalf("snapshot after reset = total:%d active:%d, want manager-lifetime total 42 and active 0", total, active)
	}
}

func TestUploadReportTruncatesQueue(t *testing.T) {
	m := newUploadReportTestManager(t)
	for i := 0; i < 6; i++ {
		if err := m.enqueue(fmt.Sprintf("user%d", i), `Music\a.flac`); err != nil {
			t.Fatal(err)
		}
	}
	promoteFirstWaiting(m) // user0 becomes active, 5 remain waiting

	const limit = 3
	report := m.report(limit)
	if report.Active != 1 {
		t.Fatalf("Active = %d, want 1", report.Active)
	}
	if report.Queued != 5 {
		t.Fatalf("Queued = %d, want 5 (the full waiting count)", report.Queued)
	}
	if report.Truncated != 2 {
		t.Fatalf("Truncated = %d, want 2 (5 waiting - 3 limit)", report.Truncated)
	}
	if len(report.Uploads) != 1+limit {
		t.Fatalf("len(Uploads) = %d, want %d (1 active + %d waiting)", len(report.Uploads), 1+limit, limit)
	}
	activeCount := 0
	for _, entry := range report.Uploads {
		if entry.Active {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("active entries in Uploads = %d, want 1 - truncation must never drop an active upload", activeCount)
	}
}

// TestUploadReportCopiesValues asserts report() copies every field by
// value: mutating a job's atomics or active flag after taking a report must
// never change the already-returned UploadReport, proving no *uploadJob
// pointer escaped the lock.
func TestUploadReportCopiesValues(t *testing.T) {
	m := newUploadReportTestManager(t)
	if err := m.enqueue("alice", `Music\a.flac`); err != nil {
		t.Fatal(err)
	}
	job := promoteFirstWaiting(m)
	job.size.Store(1000)
	job.sent.Store(500)

	report := m.report(maxReportedUploads)
	if len(report.Uploads) != 1 {
		t.Fatalf("len(Uploads) = %d, want 1", len(report.Uploads))
	}
	before := report.Uploads[0]

	job.sent.Store(999)
	job.size.Store(12345)
	m.mu.Lock()
	job.active = false
	m.mu.Unlock()

	after := report.Uploads[0]
	if after != before {
		t.Fatalf("report mutated after job changed: before=%+v after=%+v", before, after)
	}
	if after.BytesWritten != 500 || after.Size != 1000 || !after.Active {
		t.Fatalf("report entry = %+v, want the snapshot taken at report() time", after)
	}
}

// TestUploadReportRaceWithLiveTransfer runs c.UploadReport() concurrently
// with a job whose sent counter is being updated by a live "transfer"
// goroutine (m.execute overridden, same harness as
// TestUploadDispatcherHandsOffSlotAndResetsOnShutdown), asserting
// BytesWritten never appears to go backwards across successive reports.
// Must be race-clean: report() must never read job.sent/size via anything
// but the atomics.
func TestUploadReportRaceWithLiveTransfer(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.flac"), []byte("a.flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, UploadSlots: 1}, testLogger())
	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatal(err)
	}
	startSessionLifecycle(t, c)
	m := c.uploads
	release := make(chan struct{})
	started := make(chan struct{})
	m.execute = func(ctx context.Context, job *uploadJob) {
		job.size.Store(1 << 30)
		close(started)
		for {
			select {
			case <-release:
				return
			case <-ctx.Done():
				return
			default:
				job.sent.Add(4096)
			}
		}
	}

	ctx, cancel := context.WithCancel(c.lifecycleContext())
	defer cancel()
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
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upload did not start")
	}

	var wg sync.WaitGroup
	fail := make(chan string, 2)
	hammer := func() {
		defer wg.Done()
		var last uint64
		for i := 0; i < 500; i++ {
			report := c.UploadReport()
			for _, entry := range report.Uploads {
				if entry.Username != "alice" {
					continue
				}
				if entry.BytesWritten < last {
					fail <- fmt.Sprintf("BytesWritten went backwards: %d then %d", last, entry.BytesWritten)
					return
				}
				last = entry.BytesWritten
			}
		}
	}
	wg.Add(2)
	go hammer()
	go hammer()
	wg.Wait()
	close(fail)
	for msg := range fail {
		t.Fatal(msg)
	}

	close(release)
	cancel()
	select {
	case <-dispatchDone:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not stop")
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
