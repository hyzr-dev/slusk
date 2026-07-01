package matcher

import (
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/config"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
)

func TestRankPrefersHigherBitrateFlac(t *testing.T) {
	w := config.Weights{Format: 1.0, Bitrate: 1.0, Reliability: 0, FileCount: 1.0}
	s := NewWeighted(w, 192)
	results := []slskd.Result{
		{Username: "low", Filename: "a.mp3", BitRate: 200},
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
	s := NewWeighted(config.Weights{Format: 1, FileCount: 1}, 192)
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

func TestRankDropsBelowBitrateFloor(t *testing.T) {
	s := NewWeighted(config.Weights{Format: 1, Bitrate: 1, FileCount: 1}, 192)
	results := []slskd.Result{
		{Username: "lowmp3", Filename: "a.mp3", BitRate: 128}, // below floor -> dropped
		{Username: "flac", Filename: "a.flac", BitRate: 0},    // lossless -> kept even with 0 bitrate
	}
	ranked := s.Rank(results)
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
	s := NewWeighted(config.Weights{Format: 1, Bitrate: 0, Reliability: 10, FileCount: 1}, 192)
	results := []slskd.Result{
		{Username: "busy", Filename: "a.flac", BitRate: 900, HasFreeUploadSlot: false, QueueLength: 20},
		{Username: "free", Filename: "a.flac", BitRate: 900, HasFreeUploadSlot: true, QueueLength: 0},
	}
	ranked := s.Rank(results)
	if ranked[0].Username != "free" {
		t.Errorf("free-slot uploader should rank first, got %q", ranked[0].Username)
	}
}

func TestRankGroupsPerReleaseNotPerUser(t *testing.T) {
	// One user sharing two releases of the same album (FLAC + MP3) must yield TWO
	// candidates (one per directory), so we never enqueue both versions at once.
	s := NewWeighted(config.Weights{Format: 1, FileCount: 1}, 192)
	results := []slskd.Result{
		{Username: "tau", Filename: `music\Belvedere\Seven Years FLAC\01.flac`, BitRate: 900},
		{Username: "tau", Filename: `music\Belvedere\Seven Years FLAC\02.flac`, BitRate: 900},
		{Username: "tau", Filename: `music\Belvedere\Seven Years MP3\01.mp3`, BitRate: 320},
		{Username: "tau", Filename: `music\Belvedere\Seven Years MP3\02.mp3`, BitRate: 320},
	}
	ranked := s.Rank(results)
	if len(ranked) != 2 {
		t.Fatalf("expected 2 candidates (one per release directory), got %d", len(ranked))
	}
	for _, c := range ranked {
		if len(c.Files) != 2 {
			t.Errorf("each release candidate should have 2 files, got %d", len(c.Files))
		}
	}
}
