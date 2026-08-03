package store

import (
	"context"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

func TestRecordThroughputMinuteRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	minute := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	m := core.ThroughputMinute{
		Minute: minute, AvgBytesPerSecond: 1000, MaxBytesPerSecond: 2000, MaxActive: 3, Samples: 60,
	}
	if err := s.RecordThroughputMinute(ctx, m); err != nil {
		t.Fatalf("RecordThroughputMinute: %v", err)
	}

	got, err := s.ThroughputMinutes(ctx, minute.Add(-time.Hour))
	if err != nil {
		t.Fatalf("ThroughputMinutes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d: %+v", len(got), got)
	}
	if !got[0].Minute.Equal(minute) {
		t.Errorf("Minute = %v, want %v", got[0].Minute, minute)
	}
	if got[0].AvgBytesPerSecond != 1000 || got[0].MaxBytesPerSecond != 2000 || got[0].MaxActive != 3 || got[0].Samples != 60 {
		t.Errorf("round-tripped row = %+v, want AvgBytesPerSecond=1000 MaxBytesPerSecond=2000 MaxActive=3 Samples=60", got[0])
	}
}

// TestRecordThroughputMinuteUpsertPrefersMoreSamples asserts the ON CONFLICT
// clause's "more samples wins" rule (see RecordThroughputMinute's doc
// comment): writing a 12-sample partial minute followed by a 60-sample full
// minute for the same wall-clock minute leaves one row with 60 samples, and a
// subsequent re-write of the smaller partial does not regress it. This is
// what makes restart order irrelevant for the shutdown-flush path.
func TestRecordThroughputMinuteUpsertPrefersMoreSamples(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	minute := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	partial := core.ThroughputMinute{
		Minute: minute, AvgBytesPerSecond: 100, MaxBytesPerSecond: 150, MaxActive: 1, Samples: 12,
	}
	if err := s.RecordThroughputMinute(ctx, partial); err != nil {
		t.Fatalf("RecordThroughputMinute partial: %v", err)
	}

	full := core.ThroughputMinute{
		Minute: minute, AvgBytesPerSecond: 500, MaxBytesPerSecond: 900, MaxActive: 5, Samples: 60,
	}
	if err := s.RecordThroughputMinute(ctx, full); err != nil {
		t.Fatalf("RecordThroughputMinute full: %v", err)
	}

	got, err := s.ThroughputMinutes(ctx, minute.Add(-time.Hour))
	if err != nil {
		t.Fatalf("ThroughputMinutes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 row (upsert, not insert), got %d: %+v", len(got), got)
	}
	if got[0].Samples != 60 || got[0].AvgBytesPerSecond != 500 {
		t.Errorf("row after full write = %+v, want Samples=60 AvgBytesPerSecond=500", got[0])
	}

	// Re-writing the smaller partial afterward must not regress the row.
	if err := s.RecordThroughputMinute(ctx, partial); err != nil {
		t.Fatalf("RecordThroughputMinute re-write partial: %v", err)
	}
	got, err = s.ThroughputMinutes(ctx, minute.Add(-time.Hour))
	if err != nil {
		t.Fatalf("ThroughputMinutes after re-write: %v", err)
	}
	if len(got) != 1 || got[0].Samples != 60 {
		t.Fatalf("row after re-writing the smaller partial = %+v, want unchanged Samples=60", got)
	}
}

func TestPruneThroughputMinutesDeletesOnlyOldRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	old := now.Add(-31 * 24 * time.Hour)
	recent := now.Add(-time.Hour)
	if err := s.RecordThroughputMinute(ctx, core.ThroughputMinute{Minute: old, Samples: 1}); err != nil {
		t.Fatalf("RecordThroughputMinute old: %v", err)
	}
	if err := s.RecordThroughputMinute(ctx, core.ThroughputMinute{Minute: recent, Samples: 1}); err != nil {
		t.Fatalf("RecordThroughputMinute recent: %v", err)
	}

	if err := s.PruneThroughputMinutes(ctx, now); err != nil {
		t.Fatalf("PruneThroughputMinutes: %v", err)
	}

	got, err := s.ThroughputMinutes(ctx, now.Add(-40*24*time.Hour))
	if err != nil {
		t.Fatalf("ThroughputMinutes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 surviving row, got %d: %+v", len(got), got)
	}
	if !got[0].Minute.Equal(recent) {
		t.Errorf("surviving row Minute = %v, want %v", got[0].Minute, recent)
	}
}
