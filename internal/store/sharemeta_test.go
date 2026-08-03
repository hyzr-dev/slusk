package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

func shareMeta(path string, size int64, mod time.Time, bitrate, duration uint32) core.ShareFileMeta {
	return core.ShareFileMeta{Path: path, Size: size, ModTime: mod, Bitrate: bitrate, Duration: duration}
}

// TestShareFileMetadataUpsertLoadRoundTrip asserts an upserted entry comes
// back with size/mtime/bitrate/duration bit-identical, including an odd
// (non-round) microsecond mod time - the whole point of storing mtime_us
// rather than truncating to seconds.
func TestShareFileMetadataUpsertLoadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mod := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC).Add(123457 * time.Microsecond)
	entry := shareMeta("/music/a.flac", 12345, mod, 320, 200)

	if err := s.UpsertShareFileMetadata(ctx, []core.ShareFileMeta{entry}, time.Now()); err != nil {
		t.Fatalf("UpsertShareFileMetadata: %v", err)
	}

	got, err := s.ShareFileMetadata(ctx)
	if err != nil {
		t.Fatalf("ShareFileMetadata: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	row := got[0]
	if row.Path != entry.Path || row.Size != entry.Size || row.Bitrate != entry.Bitrate || row.Duration != entry.Duration {
		t.Fatalf("row = %+v, want fields matching %+v", row, entry)
	}
	if !row.ModTime.Equal(mod) || row.ModTime.UnixMicro() != mod.UnixMicro() {
		t.Fatalf("ModTime = %v (unixmicro %d), want %v (unixmicro %d)", row.ModTime, row.ModTime.UnixMicro(), mod, mod.UnixMicro())
	}
}

// TestShareFileMetadataUpsertUpdatesExistingRow asserts a second upsert on
// the same path overwrites the row's fields and advances updated_at.
func TestShareFileMetadataUpsertUpdatesExistingRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mod1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.UpsertShareFileMetadata(ctx, []core.ShareFileMeta{shareMeta("/music/a.flac", 100, mod1, 128, 60)}, first); err != nil {
		t.Fatalf("first UpsertShareFileMetadata: %v", err)
	}

	mod2 := mod1.Add(time.Hour)
	second := first.Add(time.Minute)
	if err := s.UpsertShareFileMetadata(ctx, []core.ShareFileMeta{shareMeta("/music/a.flac", 200, mod2, 320, 90)}, second); err != nil {
		t.Fatalf("second UpsertShareFileMetadata: %v", err)
	}

	got, err := s.ShareFileMetadata(ctx)
	if err != nil {
		t.Fatalf("ShareFileMetadata: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	row := got[0]
	if row.Size != 200 || row.Bitrate != 320 || row.Duration != 90 {
		t.Fatalf("row after update = %+v, want size=200 bitrate=320 duration=90", row)
	}
	if !row.ModTime.Equal(mod2) {
		t.Fatalf("ModTime after update = %v, want %v", row.ModTime, mod2)
	}
	if !row.UpdatedAt.Equal(second) {
		t.Fatalf("UpdatedAt after update = %v, want %v", row.UpdatedAt, second)
	}
}

// TestShareFileMetadataPreservesNegativeResult asserts a cached negative
// result (bitrate and duration both zero, meaning "examined, no attributes")
// survives the round trip and is not mistaken for anything else.
func TestShareFileMetadataPreservesNegativeResult(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertShareFileMetadata(ctx, []core.ShareFileMeta{
		shareMeta("/music/corrupt.mp3", 999, time.Now(), 0, 0),
	}, time.Now()); err != nil {
		t.Fatalf("UpsertShareFileMetadata: %v", err)
	}

	got, err := s.ShareFileMetadata(ctx)
	if err != nil {
		t.Fatalf("ShareFileMetadata: %v", err)
	}
	if len(got) != 1 || got[0].Bitrate != 0 || got[0].Duration != 0 {
		t.Fatalf("rows = %+v, want one row with bitrate=0 duration=0", got)
	}
}

// TestShareFileMetadataUpsertDeduplicatesInputPaths is a regression test for
// the Postgres "ON CONFLICT DO UPDATE command cannot affect row a second
// time" error: passing the same path twice in one call must not error, and
// the last occurrence must win.
func TestShareFileMetadataUpsertDeduplicatesInputPaths(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now()
	entries := []core.ShareFileMeta{
		shareMeta("/music/dup.flac", 100, now, 128, 60),
		shareMeta("/music/dup.flac", 200, now, 320, 90),
	}
	if err := s.UpsertShareFileMetadata(ctx, entries, now); err != nil {
		t.Fatalf("UpsertShareFileMetadata with duplicate paths: %v", err)
	}

	got, err := s.ShareFileMetadata(ctx)
	if err != nil {
		t.Fatalf("ShareFileMetadata: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1 (deduplicated)", len(got))
	}
	if got[0].Size != 200 || got[0].Bitrate != 320 {
		t.Fatalf("row = %+v, want the last duplicate occurrence (size=200 bitrate=320)", got[0])
	}
}

// TestDeleteShareFileMetadataRemovesExactlyNamedRows asserts DeleteShareFileMetadata
// deletes only the rows it is given, leaving unrelated rows untouched.
func TestDeleteShareFileMetadataRemovesExactlyNamedRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	entries := []core.ShareFileMeta{
		shareMeta("/music/keep.flac", 1, now, 1, 1),
		shareMeta("/music/gone1.flac", 1, now, 1, 1),
		shareMeta("/music/gone2.flac", 1, now, 1, 1),
	}
	if err := s.UpsertShareFileMetadata(ctx, entries, now); err != nil {
		t.Fatalf("UpsertShareFileMetadata: %v", err)
	}

	if err := s.DeleteShareFileMetadata(ctx, []string{"/music/gone1.flac", "/music/gone2.flac"}); err != nil {
		t.Fatalf("DeleteShareFileMetadata: %v", err)
	}

	got, err := s.ShareFileMetadata(ctx)
	if err != nil {
		t.Fatalf("ShareFileMetadata: %v", err)
	}
	if len(got) != 1 || got[0].Path != "/music/keep.flac" {
		t.Fatalf("rows after delete = %+v, want only /music/keep.flac", got)
	}
}

// TestShareFileMetadataEmptySlicesAreNoOps asserts upserting or deleting an
// empty slice does not touch the database (and, in particular, does not
// error).
func TestShareFileMetadataEmptySlicesAreNoOps(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertShareFileMetadata(ctx, nil, time.Now()); err != nil {
		t.Fatalf("UpsertShareFileMetadata(nil): %v", err)
	}
	if err := s.DeleteShareFileMetadata(ctx, nil); err != nil {
		t.Fatalf("DeleteShareFileMetadata(nil): %v", err)
	}

	got, err := s.ShareFileMetadata(ctx)
	if err != nil {
		t.Fatalf("ShareFileMetadata: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("rows = %d, want 0", len(got))
	}
}

// TestShareFileMetadataUpsertCrossesBatchBoundary writes shareMetaBatch+7
// rows in one call, exercising the chunking loop's boundary handling.
func TestShareFileMetadataUpsertCrossesBatchBoundary(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	const total = shareMetaBatch + 7
	entries := make([]core.ShareFileMeta, total)
	for i := range total {
		entries[i] = shareMeta(fmt.Sprintf("/music/track-%05d.flac", i), int64(i), now, 128, 60)
	}

	if err := s.UpsertShareFileMetadata(ctx, entries, now); err != nil {
		t.Fatalf("UpsertShareFileMetadata: %v", err)
	}

	got, err := s.ShareFileMetadata(ctx)
	if err != nil {
		t.Fatalf("ShareFileMetadata: %v", err)
	}
	if len(got) != total {
		t.Fatalf("rows = %d, want %d", len(got), total)
	}
}

// TestShareFileMetadataUpsertSkipsOverlongPathButKeepsRest asserts a path
// exceeding maxSharePathBytes is silently skipped while the rest of the
// batch is still written.
func TestShareFileMetadataUpsertSkipsOverlongPathButKeepsRest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	padding := make([]byte, maxSharePathBytes+1)
	for i := range padding {
		padding[i] = 'a'
	}
	overlong := "/music/" + string(padding)
	entries := []core.ShareFileMeta{
		shareMeta(overlong, 1, now, 1, 1),
		shareMeta("/music/ok.flac", 2, now, 2, 2),
	}
	if err := s.UpsertShareFileMetadata(ctx, entries, now); err != nil {
		t.Fatalf("UpsertShareFileMetadata: %v", err)
	}

	got, err := s.ShareFileMetadata(ctx)
	if err != nil {
		t.Fatalf("ShareFileMetadata: %v", err)
	}
	if len(got) != 1 || got[0].Path != "/music/ok.flac" {
		t.Fatalf("rows = %+v, want only the non-overlong path", got)
	}
}

// TestShareFileMetadataUpsertSkipsInvalidUTF8PathButKeepsRest is a regression
// test for a Latin-1-encoded filename (arbitrary bytes are a valid Linux
// filename, but not valid UTF-8): without a guard, Postgres rejects the
// *entire* INSERT statement with "invalid byte sequence for encoding UTF8",
// silently dropping every other row in the same batch along with it. The
// invalid entry must be skipped without error, and the rest of the batch
// must still be written.
func TestShareFileMetadataUpsertSkipsInvalidUTF8PathButKeepsRest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	invalid := "/music/" + string([]byte{0xff, 0xfe}) + ".flac"
	entries := []core.ShareFileMeta{
		shareMeta(invalid, 1, now, 1, 1),
		shareMeta("/music/ok.flac", 2, now, 2, 2),
	}
	if err := s.UpsertShareFileMetadata(ctx, entries, now); err != nil {
		t.Fatalf("UpsertShareFileMetadata: %v", err)
	}

	got, err := s.ShareFileMetadata(ctx)
	if err != nil {
		t.Fatalf("ShareFileMetadata: %v", err)
	}
	if len(got) != 1 || got[0].Path != "/music/ok.flac" {
		t.Fatalf("rows = %+v, want only the valid-UTF-8 path", got)
	}
}
