package engine

import (
	"regexp"
	"strings"
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

// normalizeQuery derives a looser search query from a primary query that
// returned zero raw results. Soulseek search matches on tokens in shared
// folder/file paths and is sensitive to characters that rarely occur in
// peers' folder names, so a query built straight from Lidarr metadata often
// misses shares that would otherwise match. normalizeQuery:
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
func normalizeQuery(query string) string {
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
