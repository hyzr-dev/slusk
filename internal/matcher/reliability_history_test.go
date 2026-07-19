package matcher

import (
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func timePtr(t time.Time) *time.Time { return &t }

func TestReliabilityHistoryScoreNoHistoryIsNeutral(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	got := ReliabilityHistoryScore(core.PeerReliability{}, now)
	if got != 0.5 {
		t.Errorf("no-history score = %v, want 0.5 (neutral)", got)
	}
}

func TestReliabilityHistoryScoreRecentSuccessScoresAboveNeutral(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rel := core.PeerReliability{
		Artist: core.ReliabilityCounters{SuccessCount: 3, LastSuccessAt: timePtr(now.Add(-time.Hour))},
	}
	got := ReliabilityHistoryScore(rel, now)
	if got <= 0.5 {
		t.Errorf("recent success score = %v, want > 0.5", got)
	}
}

func TestReliabilityHistoryScoreRecentFailScoresBelowNeutral(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rel := core.PeerReliability{
		Artist: core.ReliabilityCounters{FailCount: 3, LastFailAt: timePtr(now.Add(-time.Hour))},
	}
	got := ReliabilityHistoryScore(rel, now)
	if got >= 0.5 {
		t.Errorf("recent fail score = %v, want < 0.5", got)
	}
}

func TestReliabilityHistoryScoreAncientSuccessFadesTowardNeutral(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	recentSuccess := core.PeerReliability{
		Artist: core.ReliabilityCounters{SuccessCount: 5, LastSuccessAt: timePtr(now.Add(-time.Hour))},
	}
	ancientSuccess := core.PeerReliability{
		Artist: core.ReliabilityCounters{SuccessCount: 5, LastSuccessAt: timePtr(now.Add(-365 * 24 * time.Hour))},
	}
	recentScore := ReliabilityHistoryScore(recentSuccess, now)
	ancientScore := ReliabilityHistoryScore(ancientSuccess, now)
	if ancientScore >= recentScore {
		t.Errorf("ancient success score %v should be lower than recent success score %v", ancientScore, recentScore)
	}
	if diff := ancientScore - 0.5; diff < 0 || diff > 0.02 {
		t.Errorf("ancient (1yr old) success score %v should have decayed to nearly neutral 0.5", ancientScore)
	}
}

func TestReliabilityHistoryScoreRecentFailOutweighsAncientSuccess(t *testing.T) {
	// A peer with a big success history from a year ago but a fresh fail must
	// score BELOW a peer with no history at all - this is what breaks the
	// "same bad peer re-picked forever" loop: an old success must not keep
	// protecting a peer that has since gone bad.
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rel := core.PeerReliability{
		Artist: core.ReliabilityCounters{
			SuccessCount: 10, LastSuccessAt: timePtr(now.Add(-365 * 24 * time.Hour)),
			FailCount: 2, LastFailAt: timePtr(now.Add(-time.Hour)),
		},
	}
	got := ReliabilityHistoryScore(rel, now)
	if got >= 0.5 {
		t.Errorf("recent-fail-over-ancient-success score = %v, want < 0.5 (neutral)", got)
	}
}

func TestReliabilityHistoryScoreGlobalIsFallbackAtHalfInfluence(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	artistOnly := core.PeerReliability{
		Artist: core.ReliabilityCounters{SuccessCount: 4, LastSuccessAt: timePtr(now.Add(-time.Hour))},
	}
	globalOnly := core.PeerReliability{
		Global: core.ReliabilityCounters{SuccessCount: 4, LastSuccessAt: timePtr(now.Add(-time.Hour))},
	}
	artistScore := ReliabilityHistoryScore(artistOnly, now)
	globalScore := ReliabilityHistoryScore(globalOnly, now)
	if globalScore <= 0.5 {
		t.Errorf("global-only score = %v, want > 0.5 (it should still boost)", globalScore)
	}
	if globalScore >= artistScore {
		t.Errorf("global-only score %v should be weaker than the same history at artist scope %v", globalScore, artistScore)
	}
}

func TestRankKnownGoodPeerWinsTieOverUnknownPeer(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	w := Weights{Format: 1, Bitrate: 1, FileCount: 1, KnownUser: 1.0}
	s := NewWeighted(w, 192)
	results := []core.SearchResult{
		{Username: "unknown", Filename: "a.flac", BitRate: 900},
		{Username: "known_good", Filename: "a.flac", BitRate: 900},
	}
	rel := map[string]core.PeerReliability{
		"known_good": {Artist: core.ReliabilityCounters{SuccessCount: 5, LastSuccessAt: timePtr(now.Add(-time.Hour))}},
	}
	ranked := s.Rank(results, rel, now)
	if ranked[0].Username != "known_good" {
		t.Errorf("expected known_good to rank first on an otherwise-tied candidate, got %q first", ranked[0].Username)
	}
}

func TestRankRecentFailuresSuppressPeerBelowUnknown(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	w := Weights{Format: 1, Bitrate: 1, FileCount: 1, KnownUser: 1.0}
	s := NewWeighted(w, 192)
	results := []core.SearchResult{
		{Username: "unknown", Filename: "a.flac", BitRate: 900},
		{Username: "known_bad", Filename: "a.flac", BitRate: 900},
	}
	rel := map[string]core.PeerReliability{
		"known_bad": {Artist: core.ReliabilityCounters{FailCount: 5, LastFailAt: timePtr(now.Add(-time.Hour))}},
	}
	ranked := s.Rank(results, rel, now)
	if ranked[0].Username != "unknown" {
		t.Errorf("expected the peer with no history to outrank the recently-failing peer, got %q first", ranked[0].Username)
	}
}

func TestRankKnownUserHistoryDoesNotBeatFreshFlacOverMP3(t *testing.T) {
	// A peer with a strong known-good history but only MP3s must NOT outrank a
	// completely unknown peer offering the full FLAC album under default
	// weights - the known_user boost is bounded (0..1 factor) and must stay
	// small relative to a real format/bitrate gap across a whole album.
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// Mirrors config.example.toml's [engine.weights] defaults.
	w := Weights{Format: 1.0, Bitrate: 0.5, Reliability: 0.8, FileCount: 1.0, KnownUser: 1.0}
	s := NewWeighted(w, 192)
	var results []core.SearchResult
	for i := 1; i <= 10; i++ {
		results = append(results,
			core.SearchResult{Username: "mp3_known", Filename: trackFile("mp3_known", i, "mp3"), BitRate: 320},
			core.SearchResult{Username: "flac_unknown", Filename: trackFile("flac_unknown", i, "flac"), BitRate: 900},
		)
	}
	rel := map[string]core.PeerReliability{
		// Maximally good, maximally fresh history: as strong a boost as the
		// model can produce.
		"mp3_known": {Artist: core.ReliabilityCounters{SuccessCount: 100, LastSuccessAt: timePtr(now)}},
	}
	ranked := s.Rank(results, rel, now)
	if ranked[0].Username != "flac_unknown" {
		t.Errorf("expected the fresh FLAC candidate to win despite the MP3 peer's known-good history, got %q first", ranked[0].Username)
	}
}

func trackFile(user string, n int, ext string) string {
	dir := "music\\" + user + "\\Album"
	return dir + "\\" + string(rune('0'+n/10)) + string(rune('0'+n%10)) + " Track." + ext
}
