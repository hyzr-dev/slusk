package matcher

import "strings"

// DropTokenQuery derives a query for a Soulseek search that has repeatedly
// returned zero raw results by dropping exactly one artist token from the
// artist+album query. This exists because the network silently filters some
// multi-token queries in a way that is not derivable from the server's
// disclosed exclusion list: the query "Bob Dylan Desire" returns 0 responses,
// while "Dylan Desire" returns 176 and "Bob Desire" returns 824 - dropping a
// single token can be the difference between zero and hundreds of hits.
// Because the cause is unobservable ahead of time, the trigger for calling
// this is empirical (repeated zero raw results, see discovery.go) and the
// remedy is the minimal one: try dropping one artist token at a time rather
// than reconstructing why the network rejected the original.
//
// Only artist tokens are ever dropped, never album tokens: the album title
// carries more of the discriminating power for matching, so it is the
// artist's tokens that are sacrificed. attempt (>=0) selects which artist
// token to drop, rotating modulo the number of artist tokens, so repeated
// calls with 0, 1, 2, ... try different single-token drops deterministically
// and the rotation never reaches album tokens. A single-token artist
// therefore has only one variant and returns the same rewrite for every
// attempt - the caller re-issues an identical extra search once per backoff
// interval in that case, which is the accepted cost of never dropping the
// album title.
//
// Returns "" when there are no artist tokens to drop, when there are no
// album tokens either (a titleless album would leave an artist-only query,
// which is exactly what the two-token floor exists to prevent - the combined
// count alone does not rule it out, since a three-word artist satisfies it on
// its own), when attempt < 0
// (precondition: the sole caller only reaches this once
// job.EmptySearches >= emptySearchRewriteThreshold, so attempt is always
// >= 0), or when fewer than 3 tokens are present in total - dropping one
// token must still leave at least two, or the query becomes too generic to
// be a meaningful search (a lone artist or album word would flood the result
// set with unrelated matches).
func DropTokenQuery(artist, album string, attempt int) string {
	if attempt < 0 {
		return ""
	}

	artistTokens := strings.Fields(artist)
	albumTokens := strings.Fields(album)
	total := len(artistTokens) + len(albumTokens)
	if total < 3 || len(artistTokens) == 0 || len(albumTokens) == 0 {
		return ""
	}

	drop := attempt % len(artistTokens)

	out := make([]string, 0, total-1)
	for i, t := range artistTokens {
		if i != drop {
			out = append(out, t)
		}
	}
	out = append(out, albumTokens...)
	return strings.Join(out, " ")
}
