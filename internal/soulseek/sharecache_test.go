package soulseek

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul/peer"
)

// fakeShareMetaCache is a ShareMetaCache whose behaviour and call history are
// entirely inspectable: it records the last Load/Save call's arguments and
// how many times Save was invoked, and can be made to fail either call.
type fakeShareMetaCache struct {
	entries []ShareFileMeta
	loadErr error
	saveErr error

	upserts   []ShareFileMeta
	stale     []string
	saveCalls int
}

func (f *fakeShareMetaCache) LoadShareMeta(ctx context.Context) ([]ShareFileMeta, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return append([]ShareFileMeta(nil), f.entries...), nil
}

func (f *fakeShareMetaCache) SaveShareMeta(ctx context.Context, upserts []ShareFileMeta, stalePaths []string) error {
	f.saveCalls++
	f.upserts = upserts
	f.stale = stalePaths
	return f.saveErr
}

// resolvedLocalPath mirrors scanShares's own resolution of a share root
// (EvalSymlinks then Abs), so tests can predict the exact map key
// scanShares uses (which may differ from the raw t.TempDir() string on
// platforms where the temp dir is itself a symlink, e.g. macOS /tmp).
func resolvedLocalPath(t *testing.T, root string, parts ...string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		t.Fatalf("Abs(%q): %v", resolved, err)
	}
	return filepath.Join(append([]string{resolved}, parts...)...)
}

func writeShareFile(t *testing.T, path string, data []byte) os.FileInfo {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func attrsEqual(a, b []peer.Attribute) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestScanSharesUsesCachedAttributes asserts a cache hit is served straight
// from the cache, without extractTechnicalMetadata ever opening the file
// (proven by the file's garbage content: extractTechnicalMetadata would
// return nil for it, but the wire attributes come from the seeded cache).
func TestScanSharesUsesCachedAttributes(t *testing.T) {
	root := t.TempDir()
	local := resolvedLocalPath(t, root, "Album", "track.flac")
	info := writeShareFile(t, local, []byte("not really flac"))

	fake := &fakeShareMetaCache{entries: []ShareFileMeta{
		{Path: local, Size: info.Size(), ModTime: info.ModTime(), Bitrate: 320, Duration: 200},
	}}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, ShareMetaCache: fake}, testLogger())

	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatalf("RescanShares: %v", err)
	}

	snapshot := c.shareSnapshot()
	indexed := snapshot.files[`Music\Album\track.flac`]
	if indexed == nil {
		t.Fatal("expected file to be indexed")
	}
	want := []peer.Attribute{{Code: peer.Bitrate, Value: 320}, {Code: peer.Duration, Value: 200}}
	if !attrsEqual(indexed.wire.Attributes, want) {
		t.Fatalf("wire attributes = %#v, want %#v (cache hit)", indexed.wire.Attributes, want)
	}
	if len(fake.upserts) != 0 {
		t.Fatalf("upserts = %#v, want none (nothing was recomputed on a full cache hit)", fake.upserts)
	}
}

// TestScanSharesCacheHitToleratesSubMicrosecondModTime is a regression test
// for the precision trap: a cached mod time necessarily lost any
// sub-microsecond precision on its round trip through the store (mtime_us),
// so the hit/miss comparison must be done on UnixMicro(), not time.Equal.
func TestScanSharesCacheHitToleratesSubMicrosecondModTime(t *testing.T) {
	root := t.TempDir()
	local := resolvedLocalPath(t, root, "track.flac")
	writeShareFile(t, local, []byte("not really flac"))

	// A timestamp with a nonzero nanosecond remainder below one microsecond.
	mod := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
	if err := os.Chtimes(local, mod, mod); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeShareMetaCache{entries: []ShareFileMeta{
		// Seeded exactly as the store would return it: truncated to
		// microseconds via UnixMicro(), not the raw filesystem ModTime.
		{Path: local, Size: info.Size(), ModTime: time.UnixMicro(info.ModTime().UnixMicro()), Bitrate: 320, Duration: 200},
	}}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, ShareMetaCache: fake}, testLogger())

	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatalf("RescanShares: %v", err)
	}

	indexed := c.shareSnapshot().files[`Music\track.flac`]
	if indexed == nil {
		t.Fatal("expected file to be indexed")
	}
	want := []peer.Attribute{{Code: peer.Bitrate, Value: 320}, {Code: peer.Duration, Value: 200}}
	if !attrsEqual(indexed.wire.Attributes, want) {
		t.Fatalf("wire attributes = %#v, want %#v (sub-microsecond mtime must still be a cache hit)", indexed.wire.Attributes, want)
	}
	if len(fake.upserts) != 0 {
		t.Fatalf("upserts = %#v, want none", fake.upserts)
	}
}

// TestScanSharesReplacesStaleCacheEntry asserts a cache row whose size or mod
// time no longer matches the file on disk is treated as a miss: the file is
// re-read (here, garbage content, so extractTechnicalMetadata yields no
// attributes) rather than blindly trusting the stale cached values, and the
// fresh (negative) result is queued for upsert.
func TestScanSharesReplacesStaleCacheEntry(t *testing.T) {
	root := t.TempDir()
	local := resolvedLocalPath(t, root, "track.flac")
	info := writeShareFile(t, local, []byte("not really flac"))

	cases := map[string]ShareFileMeta{
		"wrong size":     {Path: local, Size: info.Size() + 1, ModTime: info.ModTime(), Bitrate: 320, Duration: 200},
		"wrong mod time": {Path: local, Size: info.Size(), ModTime: info.ModTime().Add(time.Hour), Bitrate: 320, Duration: 200},
	}
	for name, stale := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeShareMetaCache{entries: []ShareFileMeta{stale}}
			c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, ShareMetaCache: fake}, testLogger())

			if _, err := c.RescanShares(context.Background()); err != nil {
				t.Fatalf("RescanShares: %v", err)
			}

			indexed := c.shareSnapshot().files[`Music\track.flac`]
			if indexed == nil {
				t.Fatal("expected file to be indexed")
			}
			if len(indexed.wire.Attributes) != 0 {
				t.Fatalf("wire attributes = %#v, want none (garbage content re-read, not the stale cached values)", indexed.wire.Attributes)
			}
			if len(fake.upserts) != 1 || fake.upserts[0].Path != local {
				t.Fatalf("upserts = %#v, want exactly one fresh entry for %q", fake.upserts, local)
			}
			if fake.upserts[0].Bitrate != 0 || fake.upserts[0].Duration != 0 {
				t.Fatalf("upserted entry = %#v, want a negative result (bitrate=duration=0)", fake.upserts[0])
			}
		})
	}
}

// TestScanSharesCachesNegativeResultButNotNonAudio asserts a corrupt/unparsable
// audio file's negative result is cached (so it isn't reopened every scan),
// while non-audio files (which extractTechnicalMetadata never even looks at)
// never produce a cache row at all.
func TestScanSharesCachesNegativeResultButNotNonAudio(t *testing.T) {
	root := t.TempDir()
	flacLocal := resolvedLocalPath(t, root, "bad.flac")
	writeShareFile(t, flacLocal, []byte("broken"))
	writeShareFile(t, resolvedLocalPath(t, root, "README"), []byte("hello"))
	writeShareFile(t, resolvedLocalPath(t, root, "book.epub"), []byte("not really epub"))

	fake := &fakeShareMetaCache{}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, ShareMetaCache: fake}, testLogger())

	stats, err := c.RescanShares(context.Background())
	if err != nil {
		t.Fatalf("RescanShares: %v", err)
	}
	if stats.Files != 3 {
		t.Fatalf("stats.Files = %d, want 3", stats.Files)
	}
	if len(fake.upserts) != 1 || fake.upserts[0].Path != flacLocal {
		t.Fatalf("upserts = %#v, want exactly one entry, for the unparsable .flac", fake.upserts)
	}
	if fake.upserts[0].Bitrate != 0 || fake.upserts[0].Duration != 0 {
		t.Fatalf("upserted entry = %#v, want a negative result", fake.upserts[0])
	}
}

// TestScanSharesWritesRealAttributesOnMiss asserts a genuinely parsable audio
// file, on a cold (empty) cache, is read and its real technical attributes
// both land on the wire and are queued for upsert.
func TestScanSharesWritesRealAttributesOnMiss(t *testing.T) {
	root := t.TempDir()
	local := resolvedLocalPath(t, root, "track.flac")
	writeShareFile(t, local, flacBytes())

	fake := &fakeShareMetaCache{}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, ShareMetaCache: fake}, testLogger())

	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatalf("RescanShares: %v", err)
	}

	indexed := c.shareSnapshot().files[`Music\track.flac`]
	if indexed == nil {
		t.Fatal("expected file to be indexed")
	}
	assertAudioAttrs(t, indexed.wire.Attributes, 2)

	if len(fake.upserts) != 1 || fake.upserts[0].Path != local {
		t.Fatalf("upserts = %#v, want exactly one entry for %q", fake.upserts, local)
	}
	bitrate, duration := attributeValues(indexed.wire.Attributes)
	if fake.upserts[0].Bitrate != bitrate || fake.upserts[0].Duration != duration {
		t.Fatalf("upserted entry = %#v, want bitrate=%d duration=%d", fake.upserts[0], bitrate, duration)
	}
}

// TestScanSharesPrunesOnlyUnobservedLoadedPaths asserts a scan's stale-path
// pruning contains exactly the cached paths that were loaded but not observed
// this scan (i.e. files removed since the cache row was written), never a
// path that is still present and still valid.
func TestScanSharesPrunesOnlyUnobservedLoadedPaths(t *testing.T) {
	root := t.TempDir()
	present := resolvedLocalPath(t, root, "still-here.flac")
	info := writeShareFile(t, present, []byte("not really flac"))
	removed := resolvedLocalPath(t, root, "no-longer-here.flac")

	fake := &fakeShareMetaCache{entries: []ShareFileMeta{
		{Path: present, Size: info.Size(), ModTime: info.ModTime(), Bitrate: 128, Duration: 60},
		{Path: removed, Size: 1, ModTime: info.ModTime(), Bitrate: 128, Duration: 60},
	}}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, ShareMetaCache: fake}, testLogger())

	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatalf("RescanShares: %v", err)
	}

	if fake.saveCalls != 1 {
		t.Fatalf("saveCalls = %d, want 1", fake.saveCalls)
	}
	if len(fake.stale) != 1 || fake.stale[0] != removed {
		t.Fatalf("stale = %#v, want exactly [%q]", fake.stale, removed)
	}
}

// TestScanSharesDoesNotPruneWhenLoadFails asserts a failed cache load
// deactivates the cache for the whole scan: nothing is saved (in particular,
// nothing is pruned) since the scan never had a valid "cached" set to diff
// observed paths against.
func TestScanSharesDoesNotPruneWhenLoadFails(t *testing.T) {
	root := t.TempDir()
	writeShareFile(t, resolvedLocalPath(t, root, "track.flac"), []byte("not really flac"))

	fake := &fakeShareMetaCache{loadErr: errors.New("load failed")}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, ShareMetaCache: fake}, testLogger())

	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatalf("RescanShares: %v", err)
	}
	if fake.saveCalls != 0 {
		t.Fatalf("saveCalls = %d, want 0 (cache inactive after a failed load)", fake.saveCalls)
	}
}

// TestScanSharesDoesNotPruneEmptyScan asserts the zero-files guard: if a scan
// walks a share root that is present but genuinely empty (so the walk indexes
// zero files) while the cache held rows, pruning is skipped rather than
// wiping the entire cache (which would otherwise happen if, say, a mount is
// transiently empty for one tick).
func TestScanSharesDoesNotPruneEmptyScan(t *testing.T) {
	root := t.TempDir() // present but empty: the walk indexes zero files

	fake := &fakeShareMetaCache{entries: []ShareFileMeta{
		{Path: "/music/somewhere.flac", Size: 1, ModTime: time.Now(), Bitrate: 128, Duration: 60},
	}}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, ShareMetaCache: fake}, testLogger())

	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatalf("RescanShares: %v", err)
	}
	if len(fake.stale) != 0 {
		t.Fatalf("stale = %#v, want none (zero-observed-files guard must have skipped pruning)", fake.stale)
	}
}

// TestScanSharesPrunesWhenObservedFilesAreAllNonAudio asserts the zero-files
// guard is measured against every file the walk indexed, not just the
// audio-only observed set: a share containing files but no mp3/flac still
// observes nothing audio-related, yet the walk indexed real files, so a
// genuinely stale cache row must still be pruned rather than mistaken for an
// empty/dropped mount.
func TestScanSharesPrunesWhenObservedFilesAreAllNonAudio(t *testing.T) {
	root := t.TempDir()
	writeShareFile(t, resolvedLocalPath(t, root, "README"), []byte("hello"))
	writeShareFile(t, resolvedLocalPath(t, root, "book.epub"), []byte("not really epub"))
	stalePath := resolvedLocalPath(t, root, "no-longer-here.flac")

	fake := &fakeShareMetaCache{entries: []ShareFileMeta{
		{Path: stalePath, Size: 1, ModTime: time.Now(), Bitrate: 128, Duration: 60},
	}}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, ShareMetaCache: fake}, testLogger())

	if _, err := c.RescanShares(context.Background()); err != nil {
		t.Fatalf("RescanShares: %v", err)
	}
	if len(fake.stale) != 1 || fake.stale[0] != stalePath {
		t.Fatalf("stale = %#v, want exactly [%q] (non-audio files must not disable pruning)", fake.stale, stalePath)
	}
}

// TestScanSharesSurvivesSaveError asserts a failing SaveShareMeta never
// surfaces as a RescanShares error nor prevents the new snapshot from being
// published — the cache is best-effort and must not affect what is
// advertised.
func TestScanSharesSurvivesSaveError(t *testing.T) {
	root := t.TempDir()
	writeShareFile(t, resolvedLocalPath(t, root, "track.flac"), []byte("not really flac"))

	fake := &fakeShareMetaCache{saveErr: errors.New("save failed")}
	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}, ShareMetaCache: fake}, testLogger())

	stats, err := c.RescanShares(context.Background())
	if err != nil {
		t.Fatalf("RescanShares: %v", err)
	}
	if stats.Files != 1 {
		t.Fatalf("stats.Files = %d, want 1", stats.Files)
	}
	if c.shareSnapshot().files[`Music\track.flac`] == nil {
		t.Fatal("snapshot was not published despite the save error")
	}
	if fake.saveCalls != 1 {
		t.Fatalf("saveCalls = %d, want 1", fake.saveCalls)
	}
}

// TestFailedScanNeverTouchesCache asserts a scan that fails partway through
// (a second configured share whose root does not exist) never reaches the
// cache flush at all: flushShareMetaCache sits after every configured root
// has walked successfully.
func TestFailedScanNeverTouchesCache(t *testing.T) {
	root := t.TempDir()
	writeShareFile(t, resolvedLocalPath(t, root, "track.flac"), []byte("not really flac"))
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	fake := &fakeShareMetaCache{}
	c := New(Config{SharedFolders: []SharedFolder{
		{Name: "Music", Path: root},
		{Name: "Missing", Path: missing},
	}, ShareMetaCache: fake}, testLogger())

	if _, err := c.RescanShares(context.Background()); err == nil {
		t.Fatal("expected RescanShares to fail for a nonexistent share root")
	}
	if fake.saveCalls != 0 {
		t.Fatalf("saveCalls = %d, want 0 (a failed scan must never flush the cache)", fake.saveCalls)
	}
}

// TestScanSharesDeduplicatesOverlappingShares asserts scanning the same
// underlying file through two configured shares (one is a subdirectory of
// the other, so they are distinct configured paths but overlap on disk)
// completes without error and produces correct attributes for both virtual
// entries — the local-path-keyed cache never gets confused by a file being
// visited more than once in the same scan.
func TestScanSharesDeduplicatesOverlappingShares(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "Sub")
	local := resolvedLocalPath(t, root, "Sub", "track.flac")
	writeShareFile(t, local, flacBytes())

	fake := &fakeShareMetaCache{}
	c := New(Config{SharedFolders: []SharedFolder{
		{Name: "Root", Path: root},
		{Name: "SubShare", Path: sub},
	}, ShareMetaCache: fake}, testLogger())

	stats, err := c.RescanShares(context.Background())
	if err != nil {
		t.Fatalf("RescanShares: %v", err)
	}
	if stats.Files != 2 {
		t.Fatalf("stats.Files = %d, want 2 (same file indexed under both shares)", stats.Files)
	}
	for _, virtual := range []string{`Root\Sub\track.flac`, `SubShare\track.flac`} {
		indexed := c.shareSnapshot().files[virtual]
		if indexed == nil {
			t.Fatalf("expected %q to be indexed", virtual)
		}
		assertAudioAttrs(t, indexed.wire.Attributes, 2)
	}

	// The overlapping shares walk the same local file twice, so it is queued
	// for upsert twice under the same path - this is exactly the duplicate
	// input UpsertShareFileMetadata's dedup guards against (see
	// internal/store/sharemeta.go and
	// TestShareFileMetadataUpsertDeduplicatesInputPaths).
	var occurrences int
	for _, u := range fake.upserts {
		if u.Path == local {
			occurrences++
		}
	}
	if occurrences != 2 {
		t.Fatalf("upserts for %q = %d, want 2 (same local file observed via both shares)", local, occurrences)
	}
}

// TestNilShareMetaCacheKeepsCurrentBehaviour asserts a Client configured
// without a ShareMetaCache (the zero value, matching every Client that
// existed before issue #197) scans exactly as it did before this cache
// existed: every file is read from disk on every scan, with no cache
// interaction of any kind.
func TestNilShareMetaCacheKeepsCurrentBehaviour(t *testing.T) {
	root := t.TempDir()
	writeShareFile(t, resolvedLocalPath(t, root, "track.flac"), []byte("not really flac"))

	c := New(Config{SharedFolders: []SharedFolder{{Name: "Music", Path: root}}}, testLogger())

	stats, err := c.RescanShares(context.Background())
	if err != nil {
		t.Fatalf("RescanShares: %v", err)
	}
	if stats.Files != 1 {
		t.Fatalf("stats.Files = %d, want 1", stats.Files)
	}
	indexed := c.shareSnapshot().files[`Music\track.flac`]
	if indexed == nil {
		t.Fatal("expected file to be indexed")
	}
	if len(indexed.wire.Attributes) != 0 {
		t.Fatalf("wire attributes = %#v, want none (garbage content, no cache configured)", indexed.wire.Attributes)
	}
}
