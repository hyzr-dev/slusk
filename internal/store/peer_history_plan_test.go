package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestPeerHistoryUsesItsIndexes pins migration 0014 to the query it exists for
// (issue #424). The two are easy to drift apart silently: dropping the
// `artist_name <> ”` predicate from PeerHistory, or adding a second filter
// column, leaves the query correct and the partial index unusable — and the
// only symptom is one sequential scan of album_jobs per artist row on a table
// that grows with the library.
//
// enable_seqscan = off is deliberate. The per-test database holds a handful of
// rows, so the planner would rightly seq-scan whatever the indexes look like;
// forcing its hand asks the question this test actually cares about — can
// these indexes serve this query shape at all — rather than what the planner
// prefers at zero scale.
func TestPeerHistoryUsesItsIndexes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.RecordAttemptOutcome(ctx, 1, "explained_peer", true, time.Now()); err != nil {
		t.Fatalf("RecordAttemptOutcome: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}

	rows, err := s.db.QueryContext(ctx, `EXPLAIN `+peerHistoryArtistsSQL, 1)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}

	for _, index := range []string{"idx_artist_user_reliability_user", "idx_album_jobs_artist_name"} {
		if !strings.Contains(plan.String(), index) {
			t.Errorf("plan does not use %s:\n%s", index, plan.String())
		}
	}
	if strings.Contains(plan.String(), "Seq Scan on album_jobs") {
		t.Errorf("plan sequentially scans album_jobs:\n%s", plan.String())
	}
}
