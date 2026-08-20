package matcher

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

func TestRankPrefersHigherBitrateFlac(t *testing.T) {
	w := Weights{Format: 1.0, Bitrate: 1.0, Reliability: 0, FileCount: 1.0}
	s := NewWeighted(w, 192)
	results := []core.SearchResult{
		{Username: "low", Filename: "a.mp3", BitRate: 200},
		{Username: "high", Filename: "a.flac", BitRate: 1000},
	}
	ranked := s.Rank(results, nil, time.Now())
	if len(ranked) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(ranked))
	}
	if ranked[0].Username != "high" {
		t.Errorf("expected 'high' ranked first, got %q", ranked[0].Username)
	}
}

func TestRankGroupsByUser(t *testing.T) {
	s := NewWeighted(Weights{Format: 1, FileCount: 1}, 192)
	results := []core.SearchResult{
		{Username: "bob", Filename: "01.flac", BitRate: 900},
		{Username: "bob", Filename: "02.flac", BitRate: 900},
	}
	ranked := s.Rank(results, nil, time.Now())
	if len(ranked) != 1 {
		t.Fatalf("expected 1 candidate (grouped), got %d", len(ranked))
	}
	if len(ranked[0].Files) != 2 {
		t.Errorf("expected 2 files for bob, got %d", len(ranked[0].Files))
	}
}

func TestRankDropsBelowBitrateFloor(t *testing.T) {
	s := NewWeighted(Weights{Format: 1, Bitrate: 1, FileCount: 1}, 192)
	results := []core.SearchResult{
		{Username: "lowmp3", Filename: "a.mp3", BitRate: 128}, // below floor -> dropped
		{Username: "flac", Filename: "a.flac", BitRate: 0},    // lossless -> kept even with 0 bitrate
	}
	ranked := s.Rank(results, nil, time.Now())
	for _, c := range ranked {
		if c.Username == "lowmp3" {
			t.Errorf("128kbps mp3 should be dropped by the 192 floor")
		}
	}
	if len(ranked) != 1 || ranked[0].Username != "flac" {
		t.Fatalf("expected only the flac candidate, got %+v", ranked)
	}
}

func TestRankRewardsReliableUploader(t *testing.T) {
	s := NewWeighted(Weights{Format: 1, Bitrate: 0, Reliability: 10, FileCount: 1}, 192)
	results := []core.SearchResult{
		{Username: "busy", Filename: "a.flac", BitRate: 900, HasFreeUploadSlot: false, QueueLength: 20},
		{Username: "free", Filename: "a.flac", BitRate: 900, HasFreeUploadSlot: true, QueueLength: 0},
	}
	ranked := s.Rank(results, nil, time.Now())
	if ranked[0].Username != "free" {
		t.Errorf("free-slot uploader should rank first, got %q", ranked[0].Username)
	}
}

func TestRankDedupesTracksWithinSameDirectory(t *testing.T) {
	// A peer's share directory can itself mix formats of the same album (e.g. FLAC
	// and MP3 side by side, not split into subfolders). Since these share one
	// (user, dir) group they become ONE candidate — but every track must appear
	// only once, keeping the best-scoring format, or Lidarr rejects the whole
	// album as "has unmatched tracks".
	s := NewWeighted(Weights{Format: 1, FileCount: 1}, 192)
	results := []core.SearchResult{
		{Username: "tau", Filename: `music\Eden\01 First Light.flac`, BitRate: 900},
		{Username: "tau", Filename: `music\Eden\01 First Light.mp3`, BitRate: 320},
		{Username: "tau", Filename: `music\Eden\02 Second Light.flac`, BitRate: 900},
		{Username: "tau", Filename: `music\Eden\02 Second Light.mp3`, BitRate: 320},
	}
	ranked := s.Rank(results, nil, time.Now())
	if len(ranked) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(ranked))
	}
	if len(ranked[0].Files) != 2 {
		t.Fatalf("expected 2 files (one per track, dedup'd), got %d: %+v", len(ranked[0].Files), ranked[0].Files)
	}
	for _, f := range ranked[0].Files {
		if !strings.HasSuffix(f.Filename, ".flac") {
			t.Errorf("expected the FLAC variant to win per track, got %q", f.Filename)
		}
	}
}

func TestRankDedupeKeepsWholeReleaseOneFormat(t *testing.T) {
	// If the winning format (FLAC) doesn't cover every track, the MP3 fallback for
	// the missing track must NOT be kept either -- one release means one format,
	// even at the cost of a gap, so Lidarr never receives a mixed-format album.
	s := NewWeighted(Weights{Format: 1, FileCount: 1}, 192)
	results := []core.SearchResult{
		{Username: "tau", Filename: `music\Eden\01 First Light.flac`, BitRate: 900},
		{Username: "tau", Filename: `music\Eden\01 First Light.mp3`, BitRate: 320},
		{Username: "tau", Filename: `music\Eden\02 Second Light.mp3`, BitRate: 320}, // no FLAC counterpart
	}
	ranked := s.Rank(results, nil, time.Now())
	if len(ranked) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(ranked))
	}
	if len(ranked[0].Files) != 1 {
		t.Fatalf("expected only the 1 FLAC track (MP3-only track dropped), got %d: %+v", len(ranked[0].Files), ranked[0].Files)
	}
	if !strings.HasSuffix(ranked[0].Files[0].Filename, "01 First Light.flac") {
		t.Errorf("expected the surviving track to be the FLAC one, got %q", ranked[0].Files[0].Filename)
	}
}

func TestRankGroupsPerReleaseNotPerUser(t *testing.T) {
	// One user sharing two releases of the same album (FLAC + MP3) must yield TWO
	// candidates (one per directory), so we never enqueue both versions at once.
	s := NewWeighted(Weights{Format: 1, FileCount: 1}, 192)
	results := []core.SearchResult{
		{Username: "tau", Filename: `music\Belvedere\Seven Years FLAC\01.flac`, BitRate: 900},
		{Username: "tau", Filename: `music\Belvedere\Seven Years FLAC\02.flac`, BitRate: 900},
		{Username: "tau", Filename: `music\Belvedere\Seven Years MP3\01.mp3`, BitRate: 320},
		{Username: "tau", Filename: `music\Belvedere\Seven Years MP3\02.mp3`, BitRate: 320},
	}
	ranked := s.Rank(results, nil, time.Now())
	if len(ranked) != 2 {
		t.Fatalf("expected 2 candidates (one per release directory), got %d", len(ranked))
	}
	for _, c := range ranked {
		if len(c.Files) != 2 {
			t.Errorf("each release candidate should have 2 files, got %d", len(c.Files))
		}
	}
}

// ruinedPeer is the reliability record of a chronically failing peer: stale
// successes, fresh fails. See IsLastResortPeer for why recency rather than the
// raw counts is what puts a peer below LastResortThreshold.
func ruinedPeer(now time.Time) core.PeerReliability {
	return core.PeerReliability{
		Global: core.ReliabilityCounters{
			SuccessCount:  31,
			LastSuccessAt: timePtr(now.Add(-ReliabilityDecayTau)),
			FailCount:     657,
			LastFailAt:    timePtr(now.Add(-time.Hour)),
		},
	}
}

func TestRankSortsLastResortPeerBehindEveryoneElse(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	// The exact case issue #508 describes: the ruined peer advertises a
	// complete FLAC set and the healthy one a partial MP3 set, so on score
	// alone the ruined peer wins by a wide margin.
	s := NewWeighted(Weights{Format: 1, Bitrate: 1, FileCount: 1, KnownUser: 1}, 192)
	var results []core.SearchResult
	for i := range 11 {
		results = append(results, core.SearchResult{
			Username: "ruined", Filename: fmt.Sprintf("ruined/%02d.flac", i+1), BitRate: 1000,
		})
	}
	results = append(results, core.SearchResult{Username: "unknown", Filename: "unknown/01.mp3", BitRate: 256})

	rel := map[string]core.PeerReliability{"ruined": ruinedPeer(now)}
	ranked := s.Rank(results, rel, now)

	if len(ranked) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(ranked))
	}
	if ranked[0].Username != "unknown" {
		t.Errorf("ranked[0] = %q, want the untried peer first", ranked[0].Username)
	}
	if ranked[1].Username != "ruined" {
		t.Errorf("ranked[1] = %q, want the ruined peer last", ranked[1].Username)
	}
	if ranked[1].Score <= ranked[0].Score {
		t.Fatalf("test is not exercising the tier: the last-resort candidate scored %v, "+
			"not more than the %v it must be sorted behind", ranked[1].Score, ranked[0].Score)
	}
	if ranked[0].LastResort {
		t.Error("the untried peer is flagged LastResort, want false")
	}
	if !ranked[1].LastResort {
		t.Error("the ruined peer is not flagged LastResort, want true")
	}
}

func TestRankKeepsRelativeOrderWithinTheLastResortTier(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	s := NewWeighted(Weights{Format: 1, Bitrate: 1, FileCount: 1, KnownUser: 1}, 192)
	results := []core.SearchResult{
		{Username: "worse", Filename: "worse/01.mp3", BitRate: 256},
		{Username: "better", Filename: "better/01.flac", BitRate: 1000},
	}
	rel := map[string]core.PeerReliability{
		"worse":  ruinedPeer(now),
		"better": ruinedPeer(now),
	}
	ranked := s.Rank(results, rel, now)

	if len(ranked) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(ranked))
	}
	for _, c := range ranked {
		if !c.LastResort {
			t.Fatalf("candidate %q not flagged LastResort; both peers are ruined", c.Username)
		}
	}
	// Both are in the penalty tier, so the ordinary score ordering decides
	// between them: the tier demotes as a block, it does not flatten.
	if ranked[0].Username != "better" {
		t.Errorf("ranked[0] = %q, want the higher-scoring last-resort candidate", ranked[0].Username)
	}
}

func TestRankKeepsALastResortPeerWhenItIsTheOnlyCandidate(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	// The tier is an ordering, never a filter. An album whose only seeder is
	// a ruined peer must stay fetchable.
	s := NewWeighted(Weights{Format: 1, Bitrate: 1, FileCount: 1, KnownUser: 1}, 192)
	results := []core.SearchResult{{Username: "ruined", Filename: "ruined/01.flac", BitRate: 1000}}
	ranked := s.Rank(results, map[string]core.PeerReliability{"ruined": ruinedPeer(now)}, now)

	if len(ranked) != 1 {
		t.Fatalf("expected the sole last-resort candidate to survive ranking, got %d", len(ranked))
	}
	if !ranked[0].LastResort {
		t.Error("sole candidate not flagged LastResort, want true")
	}
}
