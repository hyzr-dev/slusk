package matcher

import "strings"

// DropTokenQuery derives a query for a Soulseek search that has repeatedly
// returned zero raw results by dropping exactly one whitespace-separated
// token from the artist+album query. This exists because the network
// silently filters some multi-token queries in a way that is not derivable
// from the server's disclosed exclusion list: the query "Bob Dylan Desire"
// returns 0 responses, while "Dylan Desire" returns 176 and "Bob Desire"
// returns 824 - dropping a single token can be the difference between zero
// and hundreds of hits. Because the cause is unobservable ahead of time, the
// trigger for calling this is empirical (repeated zero raw results, see
// discovery.go) and the remedy is the minimal one: try dropping one token at
// a time rather than reconstructing why the network rejected the original.
//
// Tokens are taken from artist then album, in that order, preserving their
// original casing. attempt (>=0, or otherwise reduced modulo the number of
// droppable positions) selects which single token to drop, so repeated calls
// with 0, 1, 2, ... try different single-token drops deterministically.
// Artist tokens are dropped before album tokens: the album title carries
// more of the discriminating power for matching, so it is the artist's
// tokens that are sacrificed first.
//
// Returns "" when fewer than 3 tokens are present in total - dropping one
// token must still leave at least two, or the query becomes too generic to
// be a meaningful search (a lone artist or album word would flood the result
// set with unrelated matches). Returns "" also for an unparseable attempt,
// which cannot happen for the documented input range but is guarded so a
// negative or huge attempt never panics.
func DropTokenQuery(artist, album string, attempt int) string {
	artistTokens := strings.Fields(artist)
	albumTokens := strings.Fields(album)
	total := len(artistTokens) + len(albumTokens)
	if total < 3 {
		return ""
	}

	drop := attempt % total
	if drop < 0 {
		drop += total
	}

	out := make([]string, 0, total-1)
	idx := 0
	for _, t := range artistTokens {
		if idx != drop {
			out = append(out, t)
		}
		idx++
	}
	for _, t := range albumTokens {
		if idx != drop {
			out = append(out, t)
		}
		idx++
	}
	return strings.Join(out, " ")
}
