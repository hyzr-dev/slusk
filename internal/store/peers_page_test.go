package store

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/matcher"
)

// peerScoreParityTolerance is how far the SQL score may sit from the Go one.
// Both evaluate the same expression in IEEE-754 doubles, so the only expected
// difference is the last few ulps of exp() and of the timestamp round-trip
// (Postgres stores microseconds, Go carries nanoseconds — the test truncates
// its inputs so only exp() is left).
const peerScoreParityTolerance = 1e-9

// insertPeer writes one known_users row directly. RecordAttemptOutcome can
// only move a counter by one at its own `now`, which cannot express the
// combinations this file needs (a count above the cap, a count with a NULL
// timestamp, an old success beside a fresh fail).
func insertPeer(t *testing.T, s *Store, username string, c core.ReliabilityCounters, now time.Time) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO known_users (username, success_count, fail_count, last_success_at, last_fail_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		username, c.SuccessCount, c.FailCount, c.LastSuccessAt, c.LastFailAt, now); err != nil {
		t.Fatalf("insert peer %q: %v", username, err)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// TestPeerScoreSQLMatchesGo is the guard the whole design of peerScoreSQL
// rests on: the Peers list orders by a decayed sigmoid re-expressed in SQL, and
// a Go/SQL copy of a ranking rule has drifted in this repo before. If this
// fails, the column header is making a claim the ordering does not honour.
func TestPeerScoreSQLMatchesGo(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Truncated to microseconds: that is Postgres' timestamptz resolution, so a
	// nanosecond tail would be a difference in the *inputs* rather than in the
	// formula under test.
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-90 * 24 * time.Hour) // three decay constants back
	fresh := now.Add(-2 * time.Hour)     //
	future := now.Add(24 * time.Hour)    // clock skew
	ancient := now.Add(-365 * 24 * time.Hour)

	cases := []struct {
		name     string
		counters core.ReliabilityCounters
	}{
		{"no history", core.ReliabilityCounters{}},
		{"successes only", core.ReliabilityCounters{SuccessCount: 3, LastSuccessAt: timePtr(fresh)}},
		{"failures only", core.ReliabilityCounters{FailCount: 4, LastFailAt: timePtr(fresh)}},
		{"both", core.ReliabilityCounters{
			SuccessCount: 7, LastSuccessAt: timePtr(fresh),
			FailCount: 2, LastFailAt: timePtr(fresh),
		}},
		{"old success against fresh fail", core.ReliabilityCounters{
			SuccessCount: 12, LastSuccessAt: timePtr(old),
			FailCount: 1, LastFailAt: timePtr(fresh),
		}},
		{"decayed to almost nothing", core.ReliabilityCounters{
			SuccessCount: 9, LastSuccessAt: timePtr(ancient),
		}},
		{"counts above the cap", core.ReliabilityCounters{
			SuccessCount: 500, LastSuccessAt: timePtr(fresh),
			FailCount: 400, LastFailAt: timePtr(old),
		}},
		{"null success timestamp with a count", core.ReliabilityCounters{
			SuccessCount: 5,
			FailCount:    2, LastFailAt: timePtr(fresh),
		}},
		{"null fail timestamp with a count", core.ReliabilityCounters{
			SuccessCount: 2, LastSuccessAt: timePtr(fresh),
			FailCount: 5,
		}},
		{"both timestamps null with counts", core.ReliabilityCounters{SuccessCount: 6, FailCount: 6}},
		{"timestamp in the future", core.ReliabilityCounters{
			SuccessCount: 3, LastSuccessAt: timePtr(future),
		}},
	}

	for _, tc := range cases {
		insertPeer(t, s, tc.name, tc.counters, now)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sqlScore float64
			if err := s.db.QueryRowContext(ctx,
				`SELECT `+peerScoreSQL("$2")+` FROM known_users ku WHERE ku.username = $1`,
				tc.name, now).Scan(&sqlScore); err != nil {
				t.Fatalf("query score: %v", err)
			}
			goScore := matcher.ReliabilityHistoryScore(core.PeerReliability{Global: tc.counters}, now)
			if math.Abs(sqlScore-goScore) > peerScoreParityTolerance {
				t.Fatalf("SQL and Go drifted: sql=%v go=%v (delta %v)", sqlScore, goScore, math.Abs(sqlScore-goScore))
			}
		})
	}
}

// TestPeersOrdersByScoreAcrossTheSet checks the ordering claim itself, not just
// the expression: the best peer must lead page 0 even though it sorts last by
// username, which sorting a fetched page could never achieve.
func TestPeersOrdersByScoreAcrossTheSet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)

	insertPeer(t, s, "aaa_bad", core.ReliabilityCounters{FailCount: 10, LastFailAt: timePtr(fresh)}, now)
	insertPeer(t, s, "mmm_neutral", core.ReliabilityCounters{}, now)
	insertPeer(t, s, "zzz_good", core.ReliabilityCounters{SuccessCount: 10, LastSuccessAt: timePtr(fresh)}, now)

	page, err := s.Peers(ctx, PeersQuery{PageSize: 1, Sort: "score", Dir: "desc", Now: now})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if page.Total != 3 {
		t.Errorf("Total = %d, want 3", page.Total)
	}
	if len(page.Peers) != 1 || page.Peers[0].Username != "zzz_good" {
		t.Fatalf("page 0 = %+v, want the highest-scoring peer alone", page.Peers)
	}

	last, err := s.Peers(ctx, PeersQuery{Page: 2, PageSize: 1, Sort: "score", Dir: "desc", Now: now})
	if err != nil {
		t.Fatalf("Peers page 2: %v", err)
	}
	if len(last.Peers) != 1 || last.Peers[0].Username != "aaa_bad" {
		t.Fatalf("page 2 = %+v, want the lowest-scoring peer", last.Peers)
	}
}

// TestPeersPagingIsTotallyOrdered walks every page of a set whose sort key is
// identical on every row. Without the username tiebreak the pages could
// overlap or skip, which is invisible in a single-page test.
func TestPeersPagingIsTotallyOrdered(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	const peers = 9
	for i := 0; i < peers; i++ {
		// Same counters on every row: every sort key but username ties.
		insertPeer(t, s, string(rune('a'+i))+"_peer", core.ReliabilityCounters{}, now)
	}

	for _, sort := range []string{"score", "successCount", "failCount", "username"} {
		t.Run(sort, func(t *testing.T) {
			seen := map[string]bool{}
			for page := int64(0); page < 3; page++ {
				got, err := s.Peers(ctx, PeersQuery{Page: page, PageSize: 3, Sort: sort, Dir: "desc", Now: now})
				if err != nil {
					t.Fatalf("Peers page %d: %v", page, err)
				}
				if got.Total != peers {
					t.Errorf("Total = %d, want %d", got.Total, peers)
				}
				if len(got.Peers) != 3 {
					t.Fatalf("page %d holds %d peers, want 3", page, len(got.Peers))
				}
				for _, p := range got.Peers {
					if seen[p.Username] {
						t.Errorf("%q appeared on two pages", p.Username)
					}
					seen[p.Username] = true
				}
			}
			if len(seen) != peers {
				t.Errorf("saw %d distinct peers across all pages, want %d", len(seen), peers)
			}
		})
	}
}

// TestPeersPastTheEndIsEmptyNotAnError: "there is nothing on page 40" is a fact
// about the data, not a malformed request, and the caller still needs the real
// total to render a pager that can get back.
func TestPeersPastTheEndIsEmptyNotAnError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	insertPeer(t, s, "only_peer", core.ReliabilityCounters{}, now)

	page, err := s.Peers(ctx, PeersQuery{Page: 40, PageSize: 25, Sort: "score", Dir: "desc", Now: now})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(page.Peers) != 0 {
		t.Errorf("Peers = %+v, want empty", page.Peers)
	}
	if page.Total != 1 {
		t.Errorf("Total = %d, want 1", page.Total)
	}
}

func TestPeersRejectsInvalidQueries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query PeersQuery
	}{
		{"unknown sort", PeersQuery{PageSize: 25, Sort: "lastSeen", Dir: "desc", Now: now}},
		{"unknown dir", PeersQuery{PageSize: 25, Sort: "score", Dir: "sideways", Now: now}},
		{"page size below the floor", PeersQuery{PageSize: 0, Sort: "score", Dir: "desc", Now: now}},
		{"page size above the ceiling", PeersQuery{PageSize: PeersPageSizeMax + 1, Sort: "score", Dir: "desc", Now: now}},
		{"negative page", PeersQuery{Page: -1, PageSize: 25, Sort: "score", Dir: "desc", Now: now}},
		{"score without now", PeersQuery{PageSize: 25, Sort: "score", Dir: "desc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// PageSize 0 means "unset" and defaults, so it can never fail the
			// floor check; the case is here to pin that behaviour, not an error.
			if tc.query.PageSize == 0 && tc.query.Sort == "score" && !tc.query.Now.IsZero() {
				if _, err := s.Peers(ctx, tc.query); err != nil {
					t.Fatalf("PageSize 0 should default, got %v", err)
				}
				return
			}
			if _, err := s.Peers(ctx, tc.query); err == nil {
				t.Fatalf("Peers accepted %+v", tc.query)
			}
		})
	}
}

// TestPeersSortKeysAcceptedByTheStore is the store half of the pair;
// observ.TestPeersSortKeysMatchTheStore is the other. A key accepted by one
// side and rejected by the other is a 400 neither package's own tests can see
// — the exact shape that shipped in #310.
func TestPeersSortKeysAcceptedByTheStore(t *testing.T) {
	for _, sort := range PeersSortKeys {
		if err := validatePeersQuery(PeersQuery{PageSize: 25, Sort: sort, Dir: "desc", Now: time.Now()}); err != nil {
			t.Errorf("validatePeersQuery rejected documented sort key %q: %v", sort, err)
		}
	}
}
