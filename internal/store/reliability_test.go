package store

import (
	"context"
	"testing"
	"time"
)

func TestRecordAttemptOutcomeUpsertsGlobalAndArtistScopes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	if err := s.RecordAttemptOutcome(ctx, 100, "reliable_peer", true, t1); err != nil {
		t.Fatalf("RecordAttemptOutcome success: %v", err)
	}
	if err := s.RecordAttemptOutcome(ctx, 100, "reliable_peer", false, t2); err != nil {
		t.Fatalf("RecordAttemptOutcome fail: %v", err)
	}

	rel, err := s.ReliabilityFor(ctx, 100, []string{"reliable_peer"})
	if err != nil {
		t.Fatalf("ReliabilityFor: %v", err)
	}
	pr, ok := rel["reliable_peer"]
	if !ok {
		t.Fatalf("expected reliable_peer in result, got %+v", rel)
	}
	if pr.Artist.SuccessCount != 1 || pr.Artist.FailCount != 1 {
		t.Errorf("artist counters = %+v, want success=1 fail=1", pr.Artist)
	}
	if pr.Artist.LastSuccessAt == nil || !pr.Artist.LastSuccessAt.Equal(t1) {
		t.Errorf("artist LastSuccessAt = %v, want %v", pr.Artist.LastSuccessAt, t1)
	}
	if pr.Artist.LastFailAt == nil || !pr.Artist.LastFailAt.Equal(t2) {
		t.Errorf("artist LastFailAt = %v, want %v", pr.Artist.LastFailAt, t2)
	}
	if pr.Global.SuccessCount != 1 || pr.Global.FailCount != 1 {
		t.Errorf("global counters = %+v, want success=1 fail=1", pr.Global)
	}
}

func TestRecordAttemptOutcomeSeparatesArtistScopes(t *testing.T) {
	// The same peer succeeding for one artist and failing for another must keep
	// each artist's row independent (both feed the shared global row, but a
	// candidate search for artist A must not see artist B's fail history as
	// artist-specific).
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if err := s.RecordAttemptOutcome(ctx, 1, "peer", true, now); err != nil {
		t.Fatalf("record success for artist 1: %v", err)
	}
	if err := s.RecordAttemptOutcome(ctx, 2, "peer", false, now); err != nil {
		t.Fatalf("record fail for artist 2: %v", err)
	}

	rel1, err := s.ReliabilityFor(ctx, 1, []string{"peer"})
	if err != nil {
		t.Fatalf("ReliabilityFor artist 1: %v", err)
	}
	if rel1["peer"].Artist.SuccessCount != 1 || rel1["peer"].Artist.FailCount != 0 {
		t.Errorf("artist 1 counters = %+v, want success=1 fail=0", rel1["peer"].Artist)
	}

	rel2, err := s.ReliabilityFor(ctx, 2, []string{"peer"})
	if err != nil {
		t.Fatalf("ReliabilityFor artist 2: %v", err)
	}
	if rel2["peer"].Artist.SuccessCount != 0 || rel2["peer"].Artist.FailCount != 1 {
		t.Errorf("artist 2 counters = %+v, want success=0 fail=1", rel2["peer"].Artist)
	}

	// Both artists still see the same shared global row.
	if rel1["peer"].Global.SuccessCount != 1 || rel1["peer"].Global.FailCount != 1 {
		t.Errorf("global counters via artist 1 lookup = %+v, want success=1 fail=1", rel1["peer"].Global)
	}
}

func TestRecordAttemptOutcomeSkipsArtistRowWhenArtistIDUnknown(t *testing.T) {
	// artistID <= 0 means the job's artist_id hasn't been backfilled yet (see
	// core.AlbumJob.ArtistID doc comment). The outcome must still be recorded
	// globally, but no artist_user_reliability row should be written for the
	// sentinel "unknown" artist.
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if err := s.RecordAttemptOutcome(ctx, 0, "peer", true, now); err != nil {
		t.Fatalf("RecordAttemptOutcome: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM artist_user_reliability`).Scan(&count); err != nil {
		t.Fatalf("count artist_user_reliability: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no artist_user_reliability rows for unknown artist, got %d", count)
	}

	rel, err := s.ReliabilityFor(ctx, 0, []string{"peer"})
	if err != nil {
		t.Fatalf("ReliabilityFor: %v", err)
	}
	if rel["peer"].Global.SuccessCount != 1 {
		t.Errorf("global success count = %d, want 1", rel["peer"].Global.SuccessCount)
	}
}

func TestReliabilityForOmitsUsernamesWithNoHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if err := s.RecordAttemptOutcome(ctx, 1, "known", true, now); err != nil {
		t.Fatalf("RecordAttemptOutcome: %v", err)
	}

	rel, err := s.ReliabilityFor(ctx, 1, []string{"known", "unknown"})
	if err != nil {
		t.Fatalf("ReliabilityFor: %v", err)
	}
	if _, ok := rel["unknown"]; ok {
		t.Errorf("expected 'unknown' to be absent from the result, got %+v", rel["unknown"])
	}
	if _, ok := rel["known"]; !ok {
		t.Errorf("expected 'known' present in the result")
	}
}
