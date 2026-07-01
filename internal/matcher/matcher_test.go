package matcher

import (
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/config"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
)

func TestRankPrefersHigherBitrateFlac(t *testing.T) {
	w := config.Weights{Format: 1.0, Bitrate: 1.0, Reliability: 0, FileCount: 1.0}
	s := NewWeighted(w)
	results := []slskd.Result{
		{Username: "low", Filename: "a.mp3", BitRate: 128},
		{Username: "high", Filename: "a.flac", BitRate: 1000},
	}
	ranked := s.Rank(results)
	if len(ranked) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(ranked))
	}
	if ranked[0].Username != "high" {
		t.Errorf("expected 'high' ranked first, got %q", ranked[0].Username)
	}
}

func TestRankGroupsByUser(t *testing.T) {
	s := NewWeighted(config.Weights{Format: 1, FileCount: 1})
	results := []slskd.Result{
		{Username: "bob", Filename: "01.flac", BitRate: 900},
		{Username: "bob", Filename: "02.flac", BitRate: 900},
	}
	ranked := s.Rank(results)
	if len(ranked) != 1 {
		t.Fatalf("expected 1 candidate (grouped), got %d", len(ranked))
	}
	if len(ranked[0].Files) != 2 {
		t.Errorf("expected 2 files for bob, got %d", len(ranked[0].Files))
	}
}
