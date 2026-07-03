package engine

import (
	"regexp"
	"strings"
)

var (
	// bracketGroupRe matches a parenthesized or bracketed group, along with any
	// leading whitespace, e.g. " (Deluxe Edition)" or " [2011 Remaster]".
	bracketGroupRe = regexp.MustCompile(`\s*[(\[][^)\]]*[)\]]`)
	// punctuationRe matches characters that rarely appear in a peer's shared
	// folder/file names, so they only hurt Soulseek's token matching.
	punctuationRe = regexp.MustCompile(`['":.,!?]`)
	whitespaceRe  = regexp.MustCompile(`\s+`)
)

// normalizeQuery derives a looser search query from a primary query that
// returned zero raw results. Soulseek search matches on tokens in shared
// folder/file paths and is sensitive to characters that rarely occur in
// peers' folder names, so a query built straight from Lidarr metadata often
// misses shares that would otherwise match. normalizeQuery:
//   - strips parenthesized/bracketed groups (e.g. "Album (Deluxe Edition)" -> "Album")
//   - replaces "&" with "and"
//   - removes punctuation ( ' " : . , ! ? )
//   - collapses repeated whitespace
//
// It is a pure, idempotent function: normalizing an already-normalized (or
// already-clean) query returns it unchanged.
func normalizeQuery(query string) string {
	q := bracketGroupRe.ReplaceAllString(query, "")
	q = strings.ReplaceAll(q, "&", "and")
	q = punctuationRe.ReplaceAllString(q, "")
	q = whitespaceRe.ReplaceAllString(q, " ")
	return strings.TrimSpace(q)
}
