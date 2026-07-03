package matcher

import (
	"strings"
	"testing"
	"time"

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
	ranked := s.Rank(results, nil, time.Now())
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
	ranked := s.Rank(results, nil, time.Now())
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
	s := NewWeighted(config.Weights{Format: 1, Bitrate: 0, Reliability: 10, FileCount: 1}, 192)
	results := []slskd.Result{
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
	s := NewWeighted(config.Weights{Format: 1, FileCount: 1}, 192)
	results := []slskd.Result{
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
	s := NewWeighted(config.Weights{Format: 1, FileCount: 1}, 192)
	results := []slskd.Result{
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
	s := NewWeighted(config.Weights{Format: 1, FileCount: 1}, 192)
	results := []slskd.Result{
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
