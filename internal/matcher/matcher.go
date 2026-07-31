// Package matcher ranks provider-neutral search results into scored candidate
// users. It is a pure function of its inputs: no I/O, no database. Weights
// come from config so ranking can be tuned without recompiling. It also
// provides CheckRelevance (see relevance.go), a pure filter that rejects
// candidates whose files do not actually belong to the requested album -
// Rank alone has no notion of "wrong album", only "acceptable quality".
package matcher

import (
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// Weights are the tunable scoring weights the matcher ranks candidates by.
// Field names mirror config.Weights, which config.Load populates from TOML;
// the caller (cmd/slskdarr) converts config.Weights into a Weights value at
// NewWeighted, so the matcher package has no dependency on internal/config.
type Weights struct {
	Format      float64
	Bitrate     float64
	Reliability float64
	FileCount   float64
	KnownUser   float64
}

// Scorer ranks search results into candidates, best first. rel carries each
// candidate username's known peer-reliability history (see
// ReliabilityHistoryScore); a username absent from rel is treated as having
// no history. now is passed in explicitly (rather than read internally) so
// the decay math stays deterministic and testable.
type Scorer interface {
	Rank(results []core.SearchResult, rel map[string]core.PeerReliability, now time.Time) []core.RankedCandidate
}

// NewWeighted returns a Scorer that drops files below the quality floor (lossless
// always kept; lossy kept only if bitRate >= minBitrate) and scores the rest by
// format, bitrate, file count, and the peer's upload reliability.
func NewWeighted(w Weights, minBitrate int) Scorer {
	return &weighted{w: w, minBitrate: minBitrate}
}

type weighted struct {
	w          Weights
	minBitrate int
}

// FormatScore returns a 0..1 quality score for a filename's extension.
func FormatScore(filename string) float64 {
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
func (x *weighted) passesFloor(r core.SearchResult) bool {
	if FormatScore(r.Filename) >= 1.0 {
		return true
	}
	return r.BitRate >= x.minBitrate
}

// ReliabilityScore maps a peer's upload signals to a 0..1 factor.
func ReliabilityScore(r core.SearchResult) float64 {
	score := 0.0
	if r.HasFreeUploadSlot {
		score += 0.7
	}
	if r.QueueLength == 0 {
		score += 0.3
	}
	return score
}

// ReleaseDir returns the directory portion of a slskd filename (which uses "\"
// separators). Candidates are grouped by this so each one is a single release,
// not every matching file a user happens to share.
func ReleaseDir(filename string) string {
	return path.Dir(strings.ReplaceAll(filename, `\`, "/"))
}

// leadingTrackNumber matches a track number at the start of a base filename,
// e.g. "01", "01 -", "01.". Peers commonly share multiple formats or naming
// variants of the same album in a single directory (FLAC next to MP3, or
// re-tagged duplicates); this lets dedupeTracks recognize the same track
// across those variants.
var leadingTrackNumber = regexp.MustCompile(`^(\d{1,3})\b`)

// trackKey returns the track a file belongs to within its release directory,
// used to deduplicate format/naming variants of the same track. Files without
// a recognizable leading track number get a unique key (their own filename) so
// they are never wrongly merged with an unrelated file.
func trackKey(filename string) string {
	base := path.Base(strings.ReplaceAll(filename, `\`, "/"))
	if m := leadingTrackNumber.FindStringSubmatch(base); m != nil {
		return m[1]
	}
	return base
}

// ExtOf returns a filename's lowercased extension, used to bucket a release's
// files by format.
func ExtOf(filename string) string {
	return strings.ToLower(path.Ext(strings.ReplaceAll(filename, `\`, "/")))
}

// dedupeTracks collapses a release down to a single format (highest
// FormatScore; ties broken by larger bucket then extension name, for
// determinism) and, within that format, the single file per track (as
// identified by trackKey). A release is one format end to end: a track only
// available in a losing format is dropped rather than mixed in, so Lidarr
// never receives a part-FLAC, part-MP3 album from one candidate.
func dedupeTracks(files []core.SearchResult) []core.SearchResult {
	byExt := map[string][]core.SearchResult{}
	for _, f := range files {
		ext := ExtOf(f.Filename)
		byExt[ext] = append(byExt[ext], f)
	}
	var bestExt string
	for ext, group := range byExt {
		if bestExt == "" {
			bestExt = ext
			continue
		}
		score, bestScore := FormatScore(group[0].Filename), FormatScore(byExt[bestExt][0].Filename)
		switch {
		case score > bestScore:
			bestExt = ext
		case score == bestScore && len(group) > len(byExt[bestExt]):
			bestExt = ext
		case score == bestScore && len(group) == len(byExt[bestExt]) && ext < bestExt:
			bestExt = ext
		}
	}
	best := map[string]core.SearchResult{}
	order := make([]string, 0, len(byExt[bestExt]))
	for _, f := range byExt[bestExt] {
		k := trackKey(f.Filename)
		if _, ok := best[k]; !ok {
			order = append(order, k)
			best[k] = f
		}
	}
	out := make([]core.SearchResult, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

func (x *weighted) Rank(results []core.SearchResult, rel map[string]core.PeerReliability, now time.Time) []core.RankedCandidate {
	// Group by (username, release directory). A user who shares several releases of
	// the same album (e.g. FLAC + MP3) must NOT become one candidate — that would
	// enqueue every version at once. One candidate = one release from one user.
	type key struct{ user, dir string }
	groups := map[key][]core.SearchResult{}
	for _, r := range results {
		if !x.passesFloor(r) {
			continue
		}
		k := key{r.Username, ReleaseDir(r.Filename)}
		groups[k] = append(groups[k], r)
	}
	var candidates []core.RankedCandidate
	for k, files := range groups {
		files = dedupeTracks(files)
		var score float64
		for _, f := range files {
			score += x.w.Format * FormatScore(f.Filename)
			score += x.w.Bitrate * (float64(f.BitRate) / 1000.0)
		}
		score += x.w.FileCount * float64(len(files))
		score += x.w.Reliability * ReliabilityScore(files[0]) // per-user, same across files
		score += x.w.KnownUser * ReliabilityHistoryScore(rel[k.user], now)
		candidates = append(candidates, core.RankedCandidate{Username: k.user, Files: files, Score: score})
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
