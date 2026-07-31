package matcher

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Soulseek's network search matches tokens across a peer's whole shared
// path, AND'd together - not "this file belongs to this album". Searching
// "The Absence The Absence" is a valid network hit against
// "Kansas\The Absence Of Presence (2020)\...", because every query token
// appears somewhere in that path. matcher.Rank and Discovery's track-count
// band never check this: they only ever ask "is this a plausible-looking,
// plausibly-sized release", never "is it the right album". CheckRelevance is
// the gate that asks that question, in the two ways evidence is normally
// available: the peer's own filenames (track titles), or the release
// directory name (against the requested artist/title).

const (
	// minTitleRecall is the fraction of the album title's own (non-noise)
	// tokens that must appear in a candidate's directory tokens. Kept at 0.8
	// rather than 1.0 so a 5+-token title survives a peer dropping one word
	// (routine); for a 1-2 token title, 0.8 rounds up to needing all of
	// them (1/2 = 0.5 < 0.8), so the dangerous short-title-is-a-prefix case
	// (e.g. "The Absence" vs "The Absence Of Presence") stays strict.
	minTitleRecall = 0.8
	// minDirPrecision is the fraction of a directory's non-noise tokens that
	// must be explained by the requested title or artist. 0.6, not a
	// stricter 0.75: a scene release "The Absence-The Absence-2016-MTD" has
	// directory tokens {the, absence, mtd} -> precision 0.67, which is a
	// common and legitimate naming pattern that 0.75 would wrongly reject.
	// The Kansas false positive from issue #316 sits at 0.40, well below it.
	minDirPrecision = 0.6
	// minTrackTitleRecall is the fraction of one expected track title's
	// tokens that must be present in a candidate filename for that filename
	// to count as matching that track.
	minTrackTitleRecall = 0.75
	// minTrackMatchRatio is the fraction of a candidate's interpretable
	// filenames that must match some expected track title. Not higher than
	// 0.5 because Lidarr's tracklist may describe a different edition than
	// the peer's rip (bonus/hidden tracks inflate the denominator harmlessly,
	// and a handful of legitimately differently-named tracks should not sink
	// an otherwise-matching candidate).
	minTrackMatchRatio = 0.5
	// minInterpretableFiles is the absolute floor of interpretable filenames
	// (see interpretable) before track titles are trusted as evidence at
	// all - "01.mp3", "02.mp3" carries no title information no matter how
	// many of them there are.
	minInterpretableFiles = 3
	// minInterpretableRatio additionally requires interpretable filenames to
	// be a real majority of the candidate's files, not just an absolute
	// handful buried in an otherwise numeric-only release.
	minInterpretableRatio = 0.5
)

// RelevanceSource reports which evidence CheckRelevance's decision was based
// on, so callers can log why without recomputing.
type RelevanceSource int

const (
	// SourceNoData means the album title carried no usable (non-noise)
	// tokens at all; the gate cannot evaluate anything and always accepts,
	// mirroring the track-band filter's (0,0)-band skip-on-no-data policy.
	SourceNoData RelevanceSource = iota
	// SourceTrackTitles means the decision was made by matching candidate
	// filenames against the album's expected track titles.
	SourceTrackTitles
	// SourceDirectory means the decision was made by comparing the
	// candidate's release directory name against the requested title/artist.
	SourceDirectory
)

// String renders s readably for logging (see discovery.go's rejection log,
// which logs Source alongside Reason).
func (s RelevanceSource) String() string {
	switch s {
	case SourceNoData:
		return "no_data"
	case SourceTrackTitles:
		return "track_titles"
	case SourceDirectory:
		return "directory"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// RelevanceInput carries everything CheckRelevance needs to judge whether a
// candidate's files actually belong to the requested album.
type RelevanceInput struct {
	ArtistName string
	AlbumTitle string
	// TrackTitles is the album's expected track titles from Lidarr. May be
	// empty - AlbumTracks can fail or return nothing, in which case the gate
	// falls back to the directory check alone.
	TrackTitles []string
	// Files are the candidate's filenames, in the provider's own path syntax
	// (slskd uses "\" separators).
	Files []string
}

// Relevance is CheckRelevance's verdict.
type Relevance struct {
	Match  bool
	Source RelevanceSource
	// Reason is a short human-readable explanation of the verdict, for
	// logging at the call site without recomputing the check.
	Reason string
}

// CheckRelevance reports whether a candidate's files plausibly belong to the
// requested album, as a defense against Soulseek's whole-path token-AND
// search matching an unrelated album that happens to contain every query
// token (see the design-rationale note at the top of this file). It never
// has full certainty - only two proxies for it, tried in order of the better
// evidence:
//
//  1. If the album's expected track titles are usable evidence (enough of
//     the candidate's filenames are non-numeric, i.e. actually named), match
//     candidate filenames against them directly. This is decisive in both
//     directions: it can accept a well-matching candidate in a badly-named
//     folder, and reject a wrong candidate that happens to sit in a
//     plausible-looking folder.
//  2. Otherwise, compare the candidate's release directory name against the
//     requested artist/title tokens. Only when that last segment carries no
//     usable tokens at all (a multi-disc layout like ".../The Absence
//     (2016)/CD1/", where "CD1" has none) does this fall back to its parent
//     segment - never merely because the last segment's dirCheck failed. A
//     fail-then-try-parent OR would give a second, unrelated chance to every
//     candidate: for a self-titled album ("The Absence" by "The Absence"),
//     titleTokens == artistTokens, so a peer's ordinary
//     Music\<Artist>\<any other album>\ layout would satisfy the parent
//     check on the artist name alone, defeating the gate for exactly the
//     album shape issue #316 was filed about.
func CheckRelevance(in RelevanceInput) Relevance {
	titleTokens := nonNoise(in.AlbumTitle)
	artistTokens := nonNoise(in.ArtistName)
	if len(titleTokens) == 0 {
		return Relevance{Match: true, Source: SourceNoData, Reason: "album title carries no usable tokens, skipping relevance check"}
	}

	var expected []map[string]bool
	for _, t := range in.TrackTitles {
		// Bracketed groups ("(feat. Guest)", "(Live)", "(Remastered 2011)")
		// are stripped from expected TRACK TITLES before tokenizing - unlike
		// directory segments in dirCheck below, which deliberately keep every
		// token. This is not an inconsistency: a Lidarr track title's
		// parenthetical is routine edition/collaborator metadata that a
		// peer's filename legitimately omits ("01 - Song One.flac" for
		// "Song One (feat. Guest A)"), so keeping it here would reject
		// correct hip-hop/pop and live albums on nearly every track. A
		// directory's parenthetical is exactly the opposite case - it is the
		// only thing standing between "The Absence" and "The Absence Of
		// Presence" (issue #316) - so it must never be stripped there.
		if toks := nonNoise(stripBracketedGroups(t)); len(toks) > 0 {
			expected = append(expected, toks)
		}
	}

	total := len(in.Files)
	if total > 0 && len(expected) > 0 {
		interpretableCount, matched := 0, 0
		for _, f := range in.Files {
			if !interpretable(f) {
				continue
			}
			interpretableCount++
			if matchesSomeTrack(f, expected) {
				matched++
			}
		}
		if interpretableCount >= minInterpretableFiles &&
			float64(interpretableCount)/float64(total) >= minInterpretableRatio {
			ratio := float64(matched) / float64(interpretableCount)
			ok := ratio >= minTrackMatchRatio
			reason := fmt.Sprintf("track titles: %d/%d interpretable filenames match the requested album's tracklist (min %.2f)",
				matched, interpretableCount, minTrackMatchRatio)
			return Relevance{Match: ok, Source: SourceTrackTitles, Reason: reason}
		}
	}

	last, parent := splitReleaseDir(in.Files)
	// The parent segment is a fallback for the multi-disc case only (see
	// CheckRelevance's doc comment): consulted solely when the last segment
	// has no usable tokens to judge in the first place, never merely because
	// dirCheck rejected it.
	if len(nonNoise(stripTrailingBracketNoise(last))) == 0 && parent != "" {
		ok, reason := dirCheck(parent, titleTokens, artistTokens)
		if ok {
			return Relevance{Match: true, Source: SourceDirectory, Reason: "parent segment: " + reason}
		}
		return Relevance{Match: false, Source: SourceDirectory,
			Reason: fmt.Sprintf("directory %q carries no usable tokens; parent segment: %s", last, reason)}
	}
	ok, reason := dirCheck(last, titleTokens, artistTokens)
	return Relevance{Match: ok, Source: SourceDirectory, Reason: reason}
}

// splitReleaseDir returns the last path segment and its parent segment of
// the first file's directory, used to evaluate multi-disc releases (e.g.
// ".../The Absence (2016)/CD1/01 - Wartorn.flac") where the immediate
// directory carries no album information (its non-noise token set is empty)
// but its parent does. parent is "" when there is no grandparent segment to
// check. Note this is only ever consulted by CheckRelevance when the last
// segment is empty of usable tokens - a last segment that has tokens but
// fails dirCheck is a real verdict, not a reason to look at parent.
func splitReleaseDir(files []string) (last, parent string) {
	if len(files) == 0 {
		return "", ""
	}
	dir := path.Dir(strings.ReplaceAll(files[0], `\`, "/"))
	last = path.Base(dir)
	up := path.Dir(dir)
	if up == "." || up == "/" || up == dir {
		return last, ""
	}
	return last, path.Base(up)
}

// trailingBracketGroupRe matches a square- or curly-bracket group - never a
// parenthesized one - trailing a release directory segment, e.g.
// " [Epic Records]" or " {MB3984-15107}". Applied repeatedly so multiple
// trailing groups (" [FLAC] {MB3984-15107}") are all removed. In scene/peer
// naming these hold labels, catalogue numbers and quality tags - metadata,
// never the album title - so dirCheck treats their contents as noise.
//
// This deliberately does NOT extend to parentheses: the issue #316 false
// positive is "The Absence" matching "The Absence Of Presence", where "Of
// Presence" is unbracketed, and forgiving parenthesized directory content is
// exactly what would defeat the gate (see the package comment and tokens'
// doc comment in normalize.go). Square/curly groups are safe to forgive
// because real album titles are essentially never wrapped in them; "(...)"
// is used for exactly that far too often (live albums, editions, and - as
// #316 shows - real subtitles).
var trailingBracketGroupRe = regexp.MustCompile(`\s*[\[{][^\[\]{}]*[\]}]\s*$`)

// stripTrailingBracketNoise repeatedly removes trailing "[...]"/"{...}"
// groups from a directory segment (see trailingBracketGroupRe).
func stripTrailingBracketNoise(s string) string {
	for {
		next := trailingBracketGroupRe.ReplaceAllString(s, "")
		if next == s {
			break
		}
		s = next
	}
	return s
}

// dirCheck compares one directory segment's non-noise tokens against the
// requested album's title (required AND explaining) and artist (explaining
// only, never required - many peers name the release folder without the
// artist at all, under a per-artist parent directory, and requiring it would
// reject good candidates wholesale).
func dirCheck(segment string, titleTokens, artistTokens map[string]bool) (bool, string) {
	dir := nonNoise(stripTrailingBracketNoise(segment))
	if len(dir) == 0 {
		return false, fmt.Sprintf("directory %q carries no usable tokens", segment)
	}
	var recallHits, precisionHits int
	for t := range titleTokens {
		if dir[t] {
			recallHits++
		}
	}
	for t := range dir {
		if titleTokens[t] || artistTokens[t] {
			precisionHits++
		}
	}
	recall := float64(recallHits) / float64(len(titleTokens))
	precision := float64(precisionHits) / float64(len(dir))
	ok := recall >= minTitleRecall && precision >= minDirPrecision
	reason := fmt.Sprintf("directory %q explains %d/%d tokens (min %.2f), title recall %d/%d (min %.2f)",
		segment, precisionHits, len(dir), minDirPrecision, recallHits, len(titleTokens), minTitleRecall)
	return ok, reason
}

// trackBaseName returns filename's base name with its path, extension and
// leading track number stripped, for both trackFilenameTokens and
// interpretable to classify.
func trackBaseName(filename string) string {
	normalized := strings.ReplaceAll(filename, `\`, "/")
	base := strings.TrimSuffix(path.Base(normalized), path.Ext(normalized))
	if m := leadingTrackNumber.FindStringSubmatch(base); m != nil {
		base = strings.TrimSpace(strings.TrimLeft(base[len(m[0]):], " -._"))
	}
	return base
}

// trackFilenameTokens returns the non-noise tokens of a filename's base name,
// for comparison against an expected track title's tokens.
func trackFilenameTokens(filename string) map[string]bool {
	return nonNoise(trackBaseName(filename))
}

// matchesSomeTrack reports whether filename's tokens sufficiently cover any
// one of the expected track titles' tokens.
func matchesSomeTrack(filename string, expected []map[string]bool) bool {
	f := trackFilenameTokens(filename)
	if len(f) == 0 {
		return false
	}
	for _, t := range expected {
		if len(t) == 0 {
			continue
		}
		hits := 0
		for tok := range t {
			if f[tok] {
				hits++
			}
		}
		if float64(hits)/float64(len(t)) >= minTrackTitleRecall {
			return true
		}
	}
	return false
}

// interpretableExclusions are generic track-file words that carry no title
// information even though they are not release-naming noise (isNoise) - a
// file named "Track 03.flac" or "01.mp3" is not evidence either way.
var interpretableExclusions = map[string]bool{
	"track": true, "tracks": true, "untitled": true, "audiotrack": true, "unknown": true,
}

// interpretable reports whether filename's base name carries at least one
// token usable as track-title evidence: not release-naming noise, not a
// generic placeholder word, and not purely numeric (a bare track number).
func interpretable(filename string) bool {
	base := trackBaseName(filename)
	for _, t := range tokens(base) {
		if isNoise(t) || interpretableExclusions[t] {
			continue
		}
		if isAllDigits(t) {
			continue
		}
		return true
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
