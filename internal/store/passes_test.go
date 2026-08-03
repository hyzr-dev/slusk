package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
)

func TestRecordSearchPassRoundTripNewestFirstWithLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		started := now.Add(time.Duration(i) * time.Minute)
		p := core.SearchPass{
			StartedAt:  started,
			FinishedAt: started.Add(time.Second),
			Searched:   1,
			Matched:    i % 2,
		}
		if err := s.RecordSearchPass(ctx, p); err != nil {
			t.Fatalf("RecordSearchPass %d: %v", i, err)
		}
	}

	passes, err := s.RecentSearchPasses(ctx, 2)
	if err != nil {
		t.Fatalf("RecentSearchPasses: %v", err)
	}
	if len(passes) != 2 {
		t.Fatalf("expected 2 passes (limited), got %d", len(passes))
	}
	// Newest first: the last-recorded pass (i=2, started at now+2m) must come
	// before the one before it (i=1, started at now+1m).
	if !passes[0].StartedAt.Equal(now.Add(2 * time.Minute)) {
		t.Errorf("passes[0].StartedAt = %v, want %v", passes[0].StartedAt, now.Add(2*time.Minute))
	}
	if !passes[1].StartedAt.Equal(now.Add(1 * time.Minute)) {
		t.Errorf("passes[1].StartedAt = %v, want %v", passes[1].StartedAt, now.Add(time.Minute))
	}
	if passes[0].Searched != 1 || passes[0].Matched != 0 {
		t.Errorf("passes[0] = %+v, want Searched=1 Matched=0", passes[0])
	}
}

func TestPruneSearchPassesDeletesOnlyOldRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	old := now.Add(-31 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)
	if err := s.RecordSearchPass(ctx, core.SearchPass{StartedAt: old, FinishedAt: old, Searched: 1}); err != nil {
		t.Fatalf("RecordSearchPass old: %v", err)
	}
	if err := s.RecordSearchPass(ctx, core.SearchPass{StartedAt: recent, FinishedAt: recent, Searched: 1}); err != nil {
		t.Fatalf("RecordSearchPass recent: %v", err)
	}

	if err := s.PruneSearchPasses(ctx, now); err != nil {
		t.Fatalf("PruneSearchPasses: %v", err)
	}

	passes, err := s.RecentSearchPasses(ctx, 10)
	if err != nil {
		t.Fatalf("RecentSearchPasses: %v", err)
	}
	if len(passes) != 1 {
		t.Fatalf("expected 1 surviving pass, got %d", len(passes))
	}
	if !passes[0].StartedAt.Equal(recent) {
		t.Errorf("surviving pass StartedAt = %v, want %v", passes[0].StartedAt, recent)
	}
}

// TestCompletedByHourCountsOnlyAttemptSucceededWithinWindow exercises
// CompletedByHour's SQL directly: it must count only attempt_succeeded
// events (ignoring decoy event types), only within the requested window
// (ignoring older-than-window rows), grouped by hour and sorted ascending.
func TestCompletedByHourCountsOnlyAttemptSucceededWithinWindow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertWantedJob(ctx, 1, now)

	hour9 := time.Date(2026, 7, 1, 9, 30, 0, 0, time.UTC)
	hour10a := time.Date(2026, 7, 1, 10, 10, 0, 0, time.UTC)
	hour10b := time.Date(2026, 7, 1, 10, 40, 0, 0, time.UTC)
	tooOld := time.Date(2026, 7, 1, 5, 0, 0, 0, time.UTC)

	events := []struct {
		event core.JobEventType
		at    time.Time
	}{
		{core.EventAttemptSucceeded, hour9},
		{core.EventAttemptSucceeded, hour10a},
		{core.EventAttemptSucceeded, hour10b},
		{core.EventSearch, hour10a},          // decoy event type, must not count
		{core.EventAttemptSucceeded, tooOld}, // older than window, must not count
	}
	for _, e := range events {
		if err := s.AddJobEvent(ctx, job.ID, e.event, "", e.at); err != nil {
			t.Fatalf("AddJobEvent %v: %v", e.event, err)
		}
	}

	since := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	counts, err := s.CompletedByHour(ctx, since)
	if err != nil {
		t.Fatalf("CompletedByHour: %v", err)
	}
	if len(counts) != 2 {
		t.Fatalf("expected 2 hour buckets, got %d: %+v", len(counts), counts)
	}
	if !counts[0].Hour.Equal(time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)) || counts[0].Count != 1 {
		t.Errorf("counts[0] = %+v, want hour=09:00 count=1", counts[0])
	}
	if !counts[1].Hour.Equal(time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)) || counts[1].Count != 2 {
		t.Errorf("counts[1] = %+v, want hour=10:00 count=2", counts[1])
	}
}
