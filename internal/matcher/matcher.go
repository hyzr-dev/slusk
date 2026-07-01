// Package matcher ranks slskd search results into scored candidate users. It is
// a pure function of its inputs: no I/O, no database. Weights come from config
// so ranking can be tuned without recompiling.
package matcher

import (
	"path"
	"sort"
	"strings"

	"github.com/samuelenocsson/slskdarr/internal/config"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
)

// Candidate is one user offering a group of files, with an aggregate score.
type Candidate struct {
	Username string
	Files    []slskd.Result
	Score    float64
}

// Scorer ranks search results into candidates, best first.
type Scorer interface {
	Rank(results []slskd.Result) []Candidate
}

// NewWeighted returns a Scorer that drops files below the quality floor (lossless
// always kept; lossy kept only if bitRate >= minBitrate) and scores the rest by
// format, bitrate, file count, and the peer's upload reliability.
func NewWeighted(w config.Weights, minBitrate int) Scorer {
	return &weighted{w: w, minBitrate: minBitrate}
}

type weighted struct {
	w          config.Weights
	minBitrate int
}

// formatScore returns a 0..1 quality score for a filename's extension.
func formatScore(filename string) float64 {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".flac"):
		return 1.0
	case strings.HasSuffix(lower, ".mp3"):
		return 0.5
	default:
		return 0.2
	}
}

// passesFloor reports whether a file meets the quality floor. Lossless formats
// (score 1.0) are always kept; lossy files need bitRate >= minBitrate.
func (x *weighted) passesFloor(r slskd.Result) bool {
	if formatScore(r.Filename) >= 1.0 {
		return true
	}
	return r.BitRate >= x.minBitrate
}

// reliabilityScore maps a peer's upload signals to a 0..1 factor.
func reliabilityScore(r slskd.Result) float64 {
	score := 0.0
	if r.HasFreeUploadSlot {
		score += 0.7
	}
	if r.QueueLength == 0 {
		score += 0.3
	}
	return score
}

// releaseDir returns the directory portion of a slskd filename (which uses "\"
// separators). Candidates are grouped by this so each one is a single release,
// not every matching file a user happens to share.
func releaseDir(filename string) string {
	return path.Dir(strings.ReplaceAll(filename, `\`, "/"))
}

func (x *weighted) Rank(results []slskd.Result) []Candidate {
	// Group by (username, release directory). A user who shares several releases of
	// the same album (e.g. FLAC + MP3) must NOT become one candidate — that would
	// enqueue every version at once. One candidate = one release from one user.
	type key struct{ user, dir string }
	groups := map[key][]slskd.Result{}
	for _, r := range results {
		if !x.passesFloor(r) {
			continue
		}
		k := key{r.Username, releaseDir(r.Filename)}
		groups[k] = append(groups[k], r)
	}
	var candidates []Candidate
	for k, files := range groups {
		var score float64
		for _, f := range files {
			score += x.w.Format * formatScore(f.Filename)
			score += x.w.Bitrate * (float64(f.BitRate) / 1000.0)
		}
		score += x.w.FileCount * float64(len(files))
		score += x.w.Reliability * reliabilityScore(files[0]) // per-user, same across files
		candidates = append(candidates, Candidate{Username: k.user, Files: files, Score: score})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if candidates[i].Username != candidates[j].Username {
			return candidates[i].Username < candidates[j].Username
		}
		// Same user, different releases: tiebreak on the first file for determinism.
		return candidates[i].Files[0].Filename < candidates[j].Files[0].Filename
	})
	return candidates
}
