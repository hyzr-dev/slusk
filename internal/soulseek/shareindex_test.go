package soulseek

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul"
	"github.com/hyzr-dev/slusk/internal/soulseek/soul/peer"
)

// logCapture collects log messages and their attribute values verbatim. The
// text and JSON handlers both escape the quotes in a reason like `"Music" moved
// from ...`, which is exactly the substring a test wants to assert on.
type logCapture struct {
	mu    sync.Mutex
	lines strings.Builder
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		c.lines.WriteString(" " + a.Value.String())
		return true
	})
	c.lines.WriteString("\n")
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lines.String()
}

func captureLogger() (*slog.Logger, *logCapture) {
	capture := &logCapture{}
	return slog.New(capture), capture
}

// fakeShareIndexStore is a ShareIndexStore that keeps the saved index in
// memory and can be made to fail either call, so a test can move an index from
// one client to another exactly as a restart would.
type fakeShareIndexStore struct {
	index   *ShareIndex
	loadErr error
	saveErr error

	saveCalls int
}

func (f *fakeShareIndexStore) LoadShareIndex(ctx context.Context) (*ShareIndex, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.index, nil
}

func (f *fakeShareIndexStore) SaveShareIndex(ctx context.Context, index *ShareIndex) error {
	f.saveCalls++
	if f.saveErr != nil {
		return f.saveErr
	}
	stored := *index
	// The real store derives file_count from the rows it writes and returns it
	// on load; the fake has to do the same or every round trip would look
	// truncated.
	stored.FileCount = len(index.Files)
	f.index = &stored
	return nil
}

// shareTreeForIndexTest writes a share tree with a nested directory that holds
// no files of its own - the case a file-rows-only index would silently lose.
func shareTreeForIndexTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeShareFile(t, filepath.Join(root, "Artist", "Album", "01 track.flac"), []byte("not really flac"))
	writeShareFile(t, filepath.Join(root, "Artist", "Album", "02 track.mp3"), []byte("not really mp3"))
	writeShareFile(t, filepath.Join(root, "README"), []byte("readme"))
	if err := os.MkdirAll(filepath.Join(root, "Artist", "Empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestShareIndexRoundTripReproducesTheScannedSnapshot is the load path's
// central claim: a client that loads the stored index publishes exactly what
// the scan that stored it published - same browse frame byte for byte, same
// directory listing, same statistics, same search behaviour - without touching
// the filesystem.
func TestShareIndexRoundTripReproducesTheScannedSnapshot(t *testing.T) {
	root := shareTreeForIndexTest(t)
	folders := []SharedFolder{{Name: "Music", Path: root}}
	indexStore := &fakeShareIndexStore{}

	scanned := New(Config{SharedFolders: folders, ShareIndexStore: indexStore}, testLogger())
	if _, err := scanned.scanAndPublish(context.Background()); err != nil {
		t.Fatalf("scanAndPublish: %v", err)
	}
	if indexStore.saveCalls != 1 {
		t.Fatalf("SaveShareIndex called %d times, want 1", indexStore.saveCalls)
	}
	want := scanned.shareSnapshot()

	loaded := New(Config{SharedFolders: folders, ShareIndexStore: indexStore}, testLogger())
	// The scanned tree is removed first, so any filesystem access on the load
	// path would fail loudly rather than quietly making the test pass.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	ok, err := loaded.loadAndPublishShareIndex(context.Background())
	if err != nil || !ok {
		t.Fatalf("loadAndPublishShareIndex = %v, %v; want true, nil", ok, err)
	}
	got := loaded.shareSnapshot()

	if !bytes.Equal(got.sharedFrame, want.sharedFrame) {
		t.Fatalf("browse frame differs: %d bytes loaded, %d bytes scanned", len(got.sharedFrame), len(want.sharedFrame))
	}
	if !reflect.DeepEqual(got.directories, want.directories) {
		t.Fatalf("directories differ:\n loaded %#v\nscanned %#v", got.directories, want.directories)
	}
	if !reflect.DeepEqual(got.stats, want.stats) {
		t.Fatalf("stats differ:\n loaded %+v\nscanned %+v", got.stats, want.stats)
	}
	if !reflect.DeepEqual(got.folders, want.folders) {
		t.Fatalf("folder stats differ:\n loaded %#v\nscanned %#v", got.folders, want.folders)
	}
	if len(got.match("album track", 10, nil)) != len(want.match("album track", 10, nil)) {
		t.Fatal("loaded snapshot does not answer the same search as the scanned one")
	}
	for virtual, indexed := range want.files {
		other, ok := got.files[virtual]
		if !ok {
			t.Fatalf("loaded index is missing %q", virtual)
		}
		if other.local != indexed.local || other.root != indexed.root ||
			!reflect.DeepEqual(other.wire, indexed.wire) ||
			other.modTime.UnixMicro() != indexed.modTime.UnixMicro() {
			t.Fatalf("loaded entry differs for %q:\n loaded %+v\nscanned %+v", virtual, other, indexed)
		}
	}
}

// TestShareIndexLoadKeepsTheOriginalScanTime pins the rule the dashboard
// depends on: a loaded index reports when the filesystem was read, never when
// it was read back out of the database.
func TestShareIndexLoadKeepsTheOriginalScanTime(t *testing.T) {
	scannedAt := time.Now().Add(-72 * time.Hour).UTC()
	indexStore := &fakeShareIndexStore{index: &ShareIndex{
		ScannedAt:    scannedAt,
		ScanDuration: 90 * time.Second,
		Folders:      []SharedFolder{{Name: "Music", Path: "/music"}},
		Directories:  []string{"Music"},
		Files: []ShareIndexEntry{{
			VirtualPath: `Music\track.flac`, LocalPath: "/music/track.flac", ShareRoot: "/music",
			Size: 10, Extension: "flac", ModTime: scannedAt,
		}},
		FileCount:  1,
		TotalBytes: 10,
	}}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: "/music"}}, ShareIndexStore: indexStore}, testLogger())

	if ok, err := c.loadAndPublishShareIndex(context.Background()); err != nil || !ok {
		t.Fatalf("loadAndPublishShareIndex = %v, %v; want true, nil", ok, err)
	}
	report := c.ShareReport()
	if !report.IndexedAt.Equal(scannedAt) {
		t.Fatalf("IndexedAt = %v, want the stored scan time %v", report.IndexedAt, scannedAt)
	}
	if report.ScanDuration != 90*time.Second {
		t.Fatalf("ScanDuration = %v, want the stored scan's 90s", report.ScanDuration)
	}
}

// TestShareIndexLoadDeclinesWithALoggedReason walks every reason a stored
// index may not be used and asserts two things for each: the load declines, and
// the log says why. A silent fallback would make "why did this restart re-read
// my NAS" unanswerable.
func TestShareIndexLoadDeclinesWithALoggedReason(t *testing.T) {
	valid := func() *ShareIndex {
		return &ShareIndex{
			ScannedAt:   time.Now(),
			Folders:     []SharedFolder{{Name: "Music", Path: "/music"}},
			Directories: []string{"Music"},
			Files: []ShareIndexEntry{{
				VirtualPath: `Music\track.flac`, LocalPath: "/music/track.flac",
				ShareRoot: "/music", Size: 10, Extension: "flac",
			}},
			FileCount: 1,
		}
	}
	cases := []struct {
		name     string
		store    *fakeShareIndexStore
		folders  []SharedFolder
		wantLogs []string
	}{
		{
			name:     "nothing stored",
			store:    &fakeShareIndexStore{},
			folders:  []SharedFolder{{Name: "Music", Path: "/music"}},
			wantLogs: []string{"no share index has been stored yet"},
		},
		{
			name:     "load failed",
			store:    &fakeShareIndexStore{loadErr: errors.New("connection refused")},
			folders:  []SharedFolder{{Name: "Music", Path: "/music"}},
			wantLogs: []string{"reading the stored share index failed", "connection refused"},
		},
		{
			name:     "a folder moved",
			store:    &fakeShareIndexStore{index: valid()},
			folders:  []SharedFolder{{Name: "Music", Path: "/mnt/music"}},
			wantLogs: []string{"the shared folders changed", `"Music" moved from "/music" to "/mnt/music"`},
		},
		{
			name:     "a folder was added",
			store:    &fakeShareIndexStore{index: valid()},
			folders:  []SharedFolder{{Name: "Music", Path: "/music"}, {Name: "Bootlegs", Path: "/bootlegs"}},
			wantLogs: []string{"the shared folders changed", `added "Bootlegs"`},
		},
		{
			name:     "a folder was removed",
			store:    &fakeShareIndexStore{index: valid()},
			folders:  nil,
			wantLogs: []string{"the shared folders changed", `removed "Music"`},
		},
		{
			name: "the stored index is truncated",
			store: func() *fakeShareIndexStore {
				index := valid()
				index.FileCount = 5
				return &fakeShareIndexStore{index: index}
			}(),
			folders:  []SharedFolder{{Name: "Music", Path: "/music"}},
			wantLogs: []string{"the stored share index is incomplete", "records 5 files but holds 1 rows"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := captureLogger()
			c := New(Config{SharedFolders: tc.folders, ShareIndexStore: tc.store}, logger)
			ok, err := c.loadAndPublishShareIndex(context.Background())
			if ok || err != nil {
				t.Fatalf("loadAndPublishShareIndex = %v, %v; want false, nil", ok, err)
			}
			if c.shareSnapshot().stats.Files != 0 {
				t.Fatal("declining the stored index must leave the empty snapshot published")
			}
			for _, want := range tc.wantLogs {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("log does not explain the fallback: want %q in\n%s", want, logs.String())
				}
			}
		})
	}
}

// TestShareIndexLoadTooLargeIsPermanent asserts a browse frame that does not
// fit is as final coming out of the database as it is coming off the disk: the
// failure is recorded, the empty snapshot stays published, and no scan is run
// to reach the same conclusion the slow way.
func TestShareIndexLoadTooLargeIsPermanent(t *testing.T) {
	original := serializeSharedFileList
	serializeSharedFileList = func([]peer.Directory) ([]byte, error) { return nil, soul.ErrMessageTooLarge }
	t.Cleanup(func() { serializeSharedFileList = original })

	indexStore := &fakeShareIndexStore{index: &ShareIndex{
		ScannedAt:   time.Now(),
		Folders:     []SharedFolder{{Name: "Music", Path: "/music"}},
		Directories: []string{"Music"},
		Files: []ShareIndexEntry{{
			VirtualPath: `Music\track.flac`, LocalPath: "/music/track.flac",
			ShareRoot: "/music", Size: 10, Extension: "flac",
		}},
		FileCount: 1,
	}}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: "/music"}}, ShareIndexStore: indexStore}, testLogger())

	ok, err := c.loadAndPublishShareIndex(context.Background())
	if ok || !errors.Is(err, ErrShareTooLarge) {
		t.Fatalf("loadAndPublishShareIndex = %v, %v; want false, ErrShareTooLarge", ok, err)
	}
	report := c.ShareReport()
	if report.LastError == "" || report.Files != 0 {
		t.Fatalf("report = %+v, want the failure recorded over an empty index", report)
	}
}

// TestSaveShareIndexFailureLeavesTheScanIntact asserts the priority the write
// path is built on: the database is a copy, the memory is the share. A failed
// save is a warning, not a failed scan.
func TestSaveShareIndexFailureLeavesTheScanIntact(t *testing.T) {
	root := shareTreeForIndexTest(t)
	logger, logs := captureLogger()
	indexStore := &fakeShareIndexStore{saveErr: errors.New("disk full")}
	c := New(Config{
		SharedFolders:   []SharedFolder{{Name: "Music", Path: root}},
		ShareIndexStore: indexStore,
	}, logger)

	stats, err := c.scanAndPublish(context.Background())
	if err != nil {
		t.Fatalf("scanAndPublish: %v", err)
	}
	if stats.Files != 3 {
		t.Fatalf("stats = %+v, want the 3 scanned files published anyway", stats)
	}
	if c.ShareReport().LastError != "" {
		t.Fatal("a failed index save must not be recorded as a share failure")
	}
	if !strings.Contains(logs.String(), "storing the share index failed") {
		t.Fatalf("save failure was not logged:\n%s", logs.String())
	}
}

// TestShareIndexSaveHappensInsideTheScanSlot asserts the save is issued while
// the share-scan slot is still held. Outside it, two saves could race over
// which one the tables record as the latest scan.
func TestShareIndexSaveHappensInsideTheScanSlot(t *testing.T) {
	root := shareTreeForIndexTest(t)
	held := make(chan bool, 1)
	indexStore := &shareSlotProbeStore{onSave: func(c *Client) { held <- len(c.shareScanSem) > 0 }}
	c := New(Config{
		SharedFolders:   []SharedFolder{{Name: "Music", Path: root}},
		ShareIndexStore: indexStore,
	}, testLogger())
	indexStore.client = c

	if _, err := c.scanAndPublish(context.Background()); err != nil {
		t.Fatalf("scanAndPublish: %v", err)
	}
	select {
	case ok := <-held:
		if !ok {
			t.Fatal("the share index was saved outside the share-scan slot")
		}
	default:
		t.Fatal("SaveShareIndex was never called")
	}
}

// shareSlotProbeStore reports on the client's state at the moment SaveShareIndex
// is called.
type shareSlotProbeStore struct {
	client *Client
	onSave func(*Client)
}

func (s *shareSlotProbeStore) LoadShareIndex(context.Context) (*ShareIndex, error) { return nil, nil }

func (s *shareSlotProbeStore) SaveShareIndex(context.Context, *ShareIndex) error {
	s.onSave(s.client)
	return nil
}

// TestDiffSharedFoldersIgnoresOrder asserts reordering the configuration file
// is not a reason to re-read the filesystem: order does not affect what is
// indexed.
func TestDiffSharedFoldersIgnoresOrder(t *testing.T) {
	stored := []SharedFolder{{Name: "Music", Path: "/music"}, {Name: "Bootlegs", Path: "/bootlegs"}}
	configured := []SharedFolder{{Name: "Bootlegs", Path: "/bootlegs"}, {Name: "Music", Path: "/music"}}
	if diff := diffSharedFolders(stored, configured); diff != "" {
		t.Fatalf("diffSharedFolders = %q, want no difference", diff)
	}
}

// TestShareIndexEmptyDirectoriesSurviveTheRoundTrip pins why directories are
// stored separately: a folder holding only subfolders has no file rows to be
// rebuilt from, and dropping it would shrink the count announced to the server
// and hide the folder from a browsing peer.
func TestShareIndexEmptyDirectoriesSurviveTheRoundTrip(t *testing.T) {
	root := shareTreeForIndexTest(t)
	indexStore := &fakeShareIndexStore{}
	folders := []SharedFolder{{Name: "Music", Path: root}}

	scanned := New(Config{SharedFolders: folders, ShareIndexStore: indexStore}, testLogger())
	if _, err := scanned.scanAndPublish(context.Background()); err != nil {
		t.Fatalf("scanAndPublish: %v", err)
	}
	loaded := New(Config{SharedFolders: folders, ShareIndexStore: indexStore}, testLogger())
	if ok, err := loaded.loadAndPublishShareIndex(context.Background()); err != nil || !ok {
		t.Fatalf("loadAndPublishShareIndex = %v, %v; want true, nil", ok, err)
	}

	if _, ok := loaded.shareSnapshot().byDirectory[`Music\Artist\Empty`]; !ok {
		t.Fatalf("the empty directory was lost: %#v", loaded.shareSnapshot().byDirectory)
	}
	if got, want := loaded.shareSnapshot().stats.Directories, scanned.shareSnapshot().stats.Directories; got != want {
		t.Fatalf("directory count = %d, want the scanned %d", got, want)
	}
}
