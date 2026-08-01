package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func recordTestUpload(t *testing.T, s *Store, e core.UploadHistoryEntry) {
	t.Helper()
	if err := s.RecordUpload(context.Background(), e); err != nil {
		t.Fatalf("RecordUpload: %v", err)
	}
}

// TestRecordUploadRoundTrip pins every column, including that a rejected row's
// zero bytes and zero rate survive as zeroes rather than being lost to a NULL.
func TestRecordUploadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)

	recordTestUpload(t, s, core.UploadHistoryEntry{
		Username: "alice", Filename: `Music\a.flac`, Size: 5_000_000, BytesSent: 5_000_000,
		AvgBytesPerSecond: 1_250_000, Status: core.UploadCompleted,
		StartedAt: started, FinishedAt: started.Add(4 * time.Second),
	})
	recordTestUpload(t, s, core.UploadHistoryEntry{
		Username: "bob", Filename: `Music\b.flac`, Size: 900, Status: core.UploadRejected,
		Detail: "peer declined", StartedAt: started, FinishedAt: started.Add(time.Second),
	})

	got, err := s.UploadHistory(ctx, 10, 0)
	if err != nil {
		t.Fatalf("UploadHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	// Newest first: bob was written last.
	rejected, completed := got[0], got[1]
	if rejected.Username != "bob" || rejected.Status != core.UploadRejected || rejected.Detail != "peer declined" {
		t.Errorf("rejected row = %+v", rejected)
	}
	if rejected.BytesSent != 0 || rejected.AvgBytesPerSecond != 0 {
		t.Errorf("rejected row bytes/rate = %d/%d, want 0/0", rejected.BytesSent, rejected.AvgBytesPerSecond)
	}
	if completed.Username != "alice" || completed.Filename != `Music\a.flac` || completed.Size != 5_000_000 ||
		completed.BytesSent != 5_000_000 || completed.AvgBytesPerSecond != 1_250_000 ||
		completed.Status != core.UploadCompleted || completed.Detail != "" {
		t.Errorf("completed row = %+v", completed)
	}
	if !completed.StartedAt.Equal(started) || !completed.FinishedAt.Equal(started.Add(4*time.Second)) {
		t.Errorf("timestamps = %v/%v, want %v/%v", completed.StartedAt, completed.FinishedAt, started, started.Add(4*time.Second))
	}
	if completed.ID == 0 || rejected.ID <= completed.ID {
		t.Errorf("ids = %d, %d; want assigned and increasing with write order", completed.ID, rejected.ID)
	}
}

// TestRecordUploadAppendsRatherThanUpserts asserts two transfers of the same
// file to the same peer are two rows: they are two facts, and collapsing them
// would erase a retry's history.
func TestRecordUploadAppendsRatherThanUpserts(t *testing.T) {
	s := newTestStore(t)
	started := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	e := core.UploadHistoryEntry{
		Username: "alice", Filename: `Music\a.flac`, Size: 100, BytesSent: 40,
		Status: core.UploadAborted, Detail: "transfer failed",
		StartedAt: started, FinishedAt: started.Add(time.Second),
	}
	recordTestUpload(t, s, e)
	e.BytesSent, e.Status, e.Detail = 100, core.UploadCompleted, ""
	recordTestUpload(t, s, e)

	got, err := s.UploadHistory(context.Background(), 10, 0)
	if err != nil {
		t.Fatalf("UploadHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (append, not upsert): %+v", len(got), got)
	}
}

// TestUploadHistoryKeysetPagination walks the whole table one short page at a
// time and asserts every row is seen exactly once, in descending id order.
func TestUploadHistoryKeysetPagination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	const total = 7
	for i := range total {
		recordTestUpload(t, s, core.UploadHistoryEntry{
			Username: "alice", Filename: `Music\a.flac`, Size: uint64(i), Status: core.UploadCompleted,
			StartedAt: started, FinishedAt: started.Add(time.Duration(i) * time.Second),
		})
	}

	var seen []int64
	var before int64
	for range total {
		page, err := s.UploadHistory(ctx, 2, before)
		if err != nil {
			t.Fatalf("UploadHistory(before=%d): %v", before, err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			if len(seen) > 0 && e.ID >= seen[len(seen)-1] {
				t.Fatalf("ids not strictly descending across pages: %v then %d", seen, e.ID)
			}
			seen = append(seen, e.ID)
		}
		before = page[len(page)-1].ID
	}
	if len(seen) != total {
		t.Fatalf("saw %d rows across pages, want %d: %v", len(seen), total, seen)
	}
}

// TestUploadHistoryMaxID proves the marker GET /api/stream folds into
// `event: invalidate` (issue #366) reads 0 on an empty table and the highest
// row id — not the row count, not the most recently inserted id in a
// different sense — once rows exist.
func TestUploadHistoryMaxID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	maxID, err := s.UploadHistoryMaxID(ctx)
	if err != nil {
		t.Fatalf("UploadHistoryMaxID (empty table): %v", err)
	}
	if maxID != 0 {
		t.Fatalf("maxID = %d, want 0 for an empty table", maxID)
	}

	started := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	for i := range 3 {
		recordTestUpload(t, s, core.UploadHistoryEntry{
			Username: "alice", Filename: `Music\a.flac`, Status: core.UploadCompleted,
			StartedAt: started, FinishedAt: started.Add(time.Duration(i) * time.Second),
		})
	}
	rows, err := s.UploadHistory(ctx, 10, 0)
	if err != nil {
		t.Fatalf("UploadHistory: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// Newest-first (id DESC, see UploadHistory), so the highest id is rows[0].
	want := rows[0].ID

	maxID, err = s.UploadHistoryMaxID(ctx)
	if err != nil {
		t.Fatalf("UploadHistoryMaxID: %v", err)
	}
	if maxID != want {
		t.Fatalf("maxID = %d, want %d (the highest inserted row id)", maxID, want)
	}
}

// TestPruneUploadHistoryDeletesOnlyOldRows asserts retention is keyed on
// finished_at and spares anything inside the window.
func TestPruneUploadHistoryDeletesOnlyOldRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)

	for _, age := range []time.Duration{uploadHistoryRetention + time.Hour, uploadHistoryRetention - time.Hour} {
		recordTestUpload(t, s, core.UploadHistoryEntry{
			Username: "alice", Filename: `Music\a.flac`, Status: core.UploadCompleted,
			StartedAt: now.Add(-age), FinishedAt: now.Add(-age),
		})
	}
	if err := s.PruneUploadHistory(ctx, now); err != nil {
		t.Fatalf("PruneUploadHistory: %v", err)
	}

	got, err := s.UploadHistory(ctx, 10, 0)
	if err != nil {
		t.Fatalf("UploadHistory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows after prune, want only the one inside retention: %+v", len(got), got)
	}
	if want := now.Add(-uploadHistoryRetention + time.Hour); !got[0].FinishedAt.Equal(want) {
		t.Errorf("surviving row finished at %v, want %v", got[0].FinishedAt, want)
	}
}
