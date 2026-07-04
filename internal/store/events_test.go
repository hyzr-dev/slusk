package store

import (
	"context"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestAddJobEventRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, err := s.UpsertDiscoveredJob(ctx, 1, now)
	if err != nil {
		t.Fatalf("UpsertDiscoveredJob: %v", err)
	}
	if err := s.AddJobEvent(ctx, job.ID, core.EventSearch, "searched album, query=\"x\"", now); err != nil {
		t.Fatalf("AddJobEvent: %v", err)
	}

	events, err := s.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Event != core.EventSearch {
		t.Errorf("Event = %q, want %q", events[0].Event, core.EventSearch)
	}
	if events[0].Detail != "searched album, query=\"x\"" {
		t.Errorf("Detail = %q", events[0].Detail)
	}
	if events[0].AlbumJobID != job.ID {
		t.Errorf("AlbumJobID = %d, want %d", events[0].AlbumJobID, job.ID)
	}
	if !events[0].CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", events[0].CreatedAt, now)
	}
}

func TestJobEventsNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job, _ := s.UpsertDiscoveredJob(ctx, 1, now)
	if err := s.AddJobEvent(ctx, job.ID, core.EventSearch, "first", now); err != nil {
		t.Fatalf("AddJobEvent 1: %v", err)
	}
	later := now.Add(time.Minute)
	if err := s.AddJobEvent(ctx, job.ID, core.EventCandidateSelected, "second", later); err != nil {
		t.Fatalf("AddJobEvent 2: %v", err)
	}

	events, err := s.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Detail != "second" || events[1].Detail != "first" {
		t.Errorf("expected newest first, got %q then %q", events[0].Detail, events[1].Detail)
	}
}

func TestRecentEventsAcrossJobsNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	job1, _ := s.UpsertDiscoveredJob(ctx, 1, now)
	job2, _ := s.UpsertDiscoveredJob(ctx, 2, now)
	if err := s.AddJobEvent(ctx, job1.ID, core.EventSearch, "job1 event", now); err != nil {
		t.Fatalf("AddJobEvent job1: %v", err)
	}
	later := now.Add(time.Minute)
	if err := s.AddJobEvent(ctx, job2.ID, core.EventSearch, "job2 event", later); err != nil {
		t.Fatalf("AddJobEvent job2: %v", err)
	}

	events, err := s.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Detail != "job2 event" {
		t.Errorf("expected job2 event first (newest), got %q", events[0].Detail)
	}
}

func TestRecentEventsRespectsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertDiscoveredJob(ctx, 1, now)
	for i := 0; i < 5; i++ {
		if err := s.AddJobEvent(ctx, job.ID, core.EventSearch, "e", now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("AddJobEvent %d: %v", i, err)
		}
	}
	events, err := s.RecentEvents(ctx, 3)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events (limited), got %d", len(events))
	}
}

// TestPruneJobEventsDeletesOnlyOldRows reproduces the retention window:
// events older than jobEventRetention (30 days) must be deleted, but recent
// ones must survive.
func TestPruneJobEventsDeletesOnlyOldRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	job, _ := s.UpsertDiscoveredJob(ctx, 1, now)

	old := now.Add(-31 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)
	if err := s.AddJobEvent(ctx, job.ID, core.EventSearch, "old", old); err != nil {
		t.Fatalf("AddJobEvent old: %v", err)
	}
	if err := s.AddJobEvent(ctx, job.ID, core.EventSearch, "recent", recent); err != nil {
		t.Fatalf("AddJobEvent recent: %v", err)
	}

	if err := s.PruneJobEvents(ctx, now); err != nil {
		t.Fatalf("PruneJobEvents: %v", err)
	}

	events, err := s.JobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 surviving event, got %d", len(events))
	}
	if events[0].Detail != "recent" {
		t.Errorf("expected the recent event to survive, got %q", events[0].Detail)
	}
}
