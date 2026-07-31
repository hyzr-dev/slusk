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
	q := query
	// Remove innermost groups until a fixpoint so nested groups vanish fully.
	for {
		next := innermostGroupRe.ReplaceAllString(q, "")
		if next == q {
			break
		}
		q = next
	}
	q = strings.ReplaceAll(q, "&", "and")
	q = punctuationRe.ReplaceAllString(q, "")
	q = whitespaceRe.ReplaceAllString(q, " ")
	return strings.TrimSpace(q)
}

// diacriticFold is intentionally NOT built on top of innermostGroupRe: this
// tokenizer feeds the relevance gate (see relevance.go), which must not
// forgive parenthesized content the way NormalizeQuery's search-fallback use
// case does - a peer's folder named "(Of Presence)" must still count as
// tokens, not disappear.
var diacriticFold = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// tokens splits s into lowercase, diacritic-folded, alphanumeric tokens for
// the relevance gate. Unlike NormalizeQuery it never strips bracketed groups:
// every substantive word must be seen, since the gate's whole job is to
// notice unexplained tokens.
func tokens(s string) []string {
	s = strings.ReplaceAll(s, `\`, "/")
	s = strings.ToLower(s)
	if folded, _, err := transform.String(diacriticFold, s); err == nil {
		s = folded
	}
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
	// quality
	"vbr": true, "cbr": true, "kbps": true, "kbs": true, "lossless": true,
	"16bit": true, "24bit": true, "96khz": true, "192khz": true,
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
// SAFETY PROPERTY: noise only ever forgives an unexplained token; it can
// never satisfy recall, since recall is computed purely from the requested
// album/artist's own tokens (see relevance.go), and noise classification is
// never applied to those. An over-broad noise list therefore only ever
// weakens the gate (lets more candidates through) - it can never cause a
// false rejection of a real match. Err on the side of adding to this list.
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
