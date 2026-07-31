package matcher

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var (
	// innermostGroupRe matches a parenthesized or bracketed group containing no
	// nested brackets, along with any leading whitespace, e.g. " (Deluxe
	// Edition)" or " [2011 Remaster]". Applied repeatedly until no match, so
	// nested groups like "(Deluxe (Bonus Disc))" are removed inside-out.
	innermostGroupRe = regexp.MustCompile(`\s*[(\[][^()\[\]]*[)\]]`)
	// punctuationRe matches characters that rarely appear in a peer's shared
	// folder/file names, so they only hurt Soulseek's token matching. Includes
	// lone brackets left behind by unbalanced groups (e.g. "X (A) (B").
	punctuationRe = regexp.MustCompile(`['":.,!?()\[\]]`)
	whitespaceRe  = regexp.MustCompile(`\s+`)
)

// stripBracketedGroups removes parenthesized/bracketed groups from s,
// including nested ones (e.g. "Album (Deluxe (Bonus Disc))" -> "Album"),
// applying innermostGroupRe repeatedly until a fixpoint. Shared by
// NormalizeQuery (search-fallback queries) and CheckRelevance's expected
// track titles (relevance.go) - the two legitimate uses of "this bracketed
// group is metadata, not album identity". Directory segments in the
// relevance gate deliberately do NOT go through this; see dirCheck.
func stripBracketedGroups(s string) string {
	for {
		next := innermostGroupRe.ReplaceAllString(s, "")
		if next == s {
			break
		}
		s = next
	}
	return s
}

// NormalizeQuery derives a looser search query from a primary query that
// returned zero raw results. Soulseek search matches on tokens in shared
// folder/file paths and is sensitive to characters that rarely occur in
// peers' folder names, so a query built straight from Lidarr metadata often
// misses shares that would otherwise match. NormalizeQuery:
//   - strips parenthesized/bracketed groups, including nested ones
//     (e.g. "Album (Deluxe Edition)" -> "Album")
//   - replaces "&" with "and"
//   - removes punctuation ( ' " : . , ! ? ) and any lone brackets left by
//     unbalanced groups
//   - collapses repeated whitespace
//
// It is a pure, idempotent function: normalizing an already-normalized (or
// already-clean) query returns it unchanged. It can return the empty string
// (e.g. a query that is nothing but bracketed groups); callers must not
// search with that.
func NormalizeQuery(query string) string {
	q := stripBracketedGroups(query)
	q = strings.ReplaceAll(q, "&", "and")
	q = punctuationRe.ReplaceAllString(q, "")
	q = whitespaceRe.ReplaceAllString(q, " ")
	return strings.TrimSpace(q)
}

var diacriticFold = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// elisionReplacer deletes (rather than splits on) elision characters -
// straight and curly apostrophes and the grave accent occasionally used in
// their place - before tokens splits on punctuation. Peer folder naming
// routinely drops apostrophes ("Dont Cry" for "Don't Cry"), and splitting on
// them instead of deleting them would tokenize "Don't" as {don, t}, which
// then fails to recall against "Dont" -> {dont}.
var elisionReplacer = strings.NewReplacer("'", "", "’", "", "`", "")

// tokens splits s into lowercase, diacritic-folded, alphanumeric tokens for
// the relevance gate. Unlike NormalizeQuery (stripBracketedGroups) it never
// strips bracketed groups: every substantive word must be seen, since the
// gate's whole job is to notice unexplained tokens. It is intentionally NOT
// built on top of NormalizeQuery/stripBracketedGroups for that reason - a
// peer's folder named "(Of Presence)" must still count as tokens, not
// disappear.
func tokens(s string) []string {
	s = strings.ReplaceAll(s, `\`, "/")
	s = strings.ToLower(s)
	if folded, _, err := transform.String(diacriticFold, s); err == nil {
		s = folded
	}
	s = elisionReplacer.Replace(s)
	s = strings.ReplaceAll(s, "&", " and ")
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// noise classifiers. Each one recognizes a class of token that Soulseek
// release naming conventions add but that carries no information about which
// album a file belongs to (catalogue numbers, codecs, edition marketing,
// scene/rip metadata, disc/side structure).
var (
	yearRe       = regexp.MustCompile(`^(19|20)\d{2}$`)
	longDigitsRe = regexp.MustCompile(`^\d{3,}$`)
	catalogueRe  = regexp.MustCompile(`^[a-z]{1,4}\d{2,6}[a-z]?$`)
	vQualityRe   = regexp.MustCompile(`^v[0-2]$`)
	kbpsRe       = regexp.MustCompile(`^\d{2,3}kbps?$`)
	bitDepthRe   = regexp.MustCompile(`^\d{2}bits?$`)
	cdNumRe      = regexp.MustCompile(`^cd\d+$`)
	discRe       = regexp.MustCompile(`^disc\d*$`)
	diskRe       = regexp.MustCompile(`^disk\d*$`)
	sideRe       = regexp.MustCompile(`^side[a-d]$`)
	ptRe         = regexp.MustCompile(`^pt\d*$`)
)

// noiseWords are noise tokens matched by an exact set membership rather than
// a regex, grouped by why they carry no album-identifying information.
var noiseWords = map[string]bool{
	// codec/container
	"flac": true, "mp3": true, "m4a": true, "aac": true, "ogg": true,
	"opus": true, "wav": true, "alac": true, "ape": true, "wv": true,
	"aiff": true, "dsf": true,
	// quality (bit depth is matched by bitDepthRe below, not listed here, so
	// there is one unambiguous place to extend it)
	"vbr": true, "cbr": true, "kbps": true, "kbs": true, "lossless": true,
	"96khz": true, "192khz": true,
	// source
	"web": true, "webrip": true, "cd": true, "cdrip": true, "cdda": true,
	"vinyl": true, "lp": true, "ep": true, "single": true, "promo": true,
	"retail": true, "tape": true, "cassette": true, "sacd": true, "dvd": true,
	"bluray": true, "digital": true, "itunes": true, "qobuz": true,
	"tidal": true, "bandcamp": true, "hdtracks": true, "spotify": true,
	"rip": true,
	// edition
	"remaster": true, "remastered": true, "reissue": true, "deluxe": true,
	"expanded": true, "edition": true, "anniversary": true, "bonus": true,
	"special": true, "limited": true, "collectors": true, "collector": true,
	"japanese": true, "japan": true, "version": true, "pressing": true,
	// scene/extras
	"eac": true, "log": true, "cue": true, "covers": true, "cover": true,
	"artwork": true, "scans": true, "booklet": true, "nfo": true, "sbd": true,
	"tracks": true, "folder": true,
}

// isNoise reports whether tok is release-naming noise: a token that
// legitimately appears in real Soulseek shares of the requested album but
// carries no information about which album it is (years, catalogue numbers,
// codec/quality/source/edition marketing, disc/side structure, scene rip
// artifacts).
//
// This IS applied to the requested album/artist's own tokens, not just to
// candidate directory/track tokens (see relevance.go's nonNoise(in.AlbumTitle)
// and nonNoise(in.ArtistName)) - despite how tempting it is to assume
// otherwise. An over-broad entry therefore has a real cost in both
// directions: it can swallow a real title/artist token as well as a
// candidate's, shrinking the set of tokens the gate has to work with. Taken
// far enough, an entry that swallows an entire short album title (e.g. a
// purely numeric one) empties titleTokens and falls into SourceNoData, which
// accepts every candidate unconditionally - "err on the side of adding" is
// not free. Verify against real Lidarr/Soulseek data before adding, not just
// plausibility.
//
// English stopwords ("the", "of", "a", "and", "in", ...) are DELIBERATELY NOT
// noise. Forgiving "of" is exactly what would let "The Absence" match "The
// Absence Of Presence" - the false positive issue #316 exists to fix. Do not
// "clean this up" by adding them.
func isNoise(tok string) bool {
	if noiseWords[tok] {
		return true
	}
	switch {
	case yearRe.MatchString(tok):
		return true
	case longDigitsRe.MatchString(tok):
		return true
	case catalogueRe.MatchString(tok):
		return true
	case vQualityRe.MatchString(tok):
		return true
	case kbpsRe.MatchString(tok):
		return true
	case bitDepthRe.MatchString(tok):
		return true
	case cdNumRe.MatchString(tok):
		return true
	case discRe.MatchString(tok):
		return true
	case diskRe.MatchString(tok):
		return true
	case sideRe.MatchString(tok):
		return true
	case ptRe.MatchString(tok):
		return true
	}
	return false
}

// nonNoise returns s's tokens with noise removed, as a set (for
// membership/size comparisons in the relevance gate).
func nonNoise(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tokens(s) {
		if !isNoise(t) {
			out[t] = true
		}
	}
	return out
}
