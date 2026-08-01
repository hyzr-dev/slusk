package matcher

import (
	"math"
	"testing"
)

func TestDropTokenQuery(t *testing.T) {
	cases := []struct {
		name    string
		artist  string
		album   string
		attempt int
		want    string
	}{
		{
			name:    "attempt 0 drops first artist token",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: 0,
			want:    "Dylan Desire",
		},
		{
			name:    "attempt 1 drops second artist token",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: 1,
			want:    "Bob Desire",
		},
		{
			name:    "attempt wraps around modulo the artist-token count, never reaching the album token",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: 2,
			want:    "Dylan Desire", // 2 % 2 artist tokens == 0, same as attempt 0
		},
		{
			name:    "attempt 3 continues the artist-token rotation",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: 3,
			want:    "Bob Desire", // 3 % 2 == 1, same as attempt 1
		},
		{
			name:    "large attempt wraps around within the artist tokens",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: 100,
			want:    "Dylan Desire", // 100 % 2 == 0, same as attempt 0
		},
		{
			name:    "negative attempt violates the documented precondition and returns empty",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: -1,
			want:    "",
		},
		{
			name:    "large negative attempt also returns empty",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: -4,
			want:    "",
		},
		{
			name:    "album tokens carry more weight, artist dropped first across multi-word albums",
			artist:  "Bob Dylan",
			album:   "Blood on the Tracks",
			attempt: 0,
			want:    "Dylan Blood on the Tracks",
		},
		{
			name:    "single-token artist, attempt 0 drops the only artist token",
			artist:  "Prince",
			album:   "Purple Rain",
			attempt: 0,
			want:    "Purple Rain",
		},
		{
			name:    "single-token artist wraps to the same result on every attempt",
			artist:  "Prince",
			album:   "Purple Rain",
			attempt: 1,
			want:    "Purple Rain", // 1 % 1 artist token == 0, same as attempt 0
		},
		{
			name:    "two total tokens is below the guardrail, no rewrite possible",
			artist:  "Bob",
			album:   "Dylan",
			attempt: 0,
			want:    "",
		},
		{
			name:    "single artist token, no album, well below the guardrail",
			artist:  "Bob",
			album:   "",
			attempt: 0,
			want:    "",
		},
		{
			name:    "empty artist and album",
			artist:  "",
			album:   "",
			attempt: 0,
			want:    "",
		},
		{
			name:    "no artist tokens at all, only album tokens - nothing droppable",
			artist:  "",
			album:   "Bob Dylan Desire",
			attempt: 0,
			want:    "",
		},
		{
			name:    "titleless album would leave an artist-only query, so no rewrite",
			artist:  "Emerson Lake Palmer",
			album:   "",
			attempt: 0,
			want:    "",
		},
		{
			name:    "whitespace-only album title is treated as titleless",
			artist:  "Emerson Lake Palmer",
			album:   "   ",
			attempt: 1,
			want:    "",
		},
		// The extremes of int are pinned because the modulo is the one place
		// this function could panic or wrap; the caller never passes them, so
		// nothing but this table protects that.
		{
			name:    "math.MaxInt attempt still lands inside the artist tokens",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: math.MaxInt,
			want:    "Bob Desire", // MaxInt is odd, so MaxInt % 2 == 1
		},
		{
			name:    "math.MinInt attempt is rejected by the non-negative precondition",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: math.MinInt,
			want:    "",
		},
		{
			name:    "exactly three tokens, only one droppable-position rotation length",
			artist:  "A",
			album:   "B C",
			attempt: 0,
			want:    "B C", // drops the single artist token
		},
		{
			name:    "original casing is preserved",
			artist:  "ABBA",
			album:   "Arrival Deluxe",
			attempt: 0,
			want:    "Arrival Deluxe",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DropTokenQuery(c.artist, c.album, c.attempt)
			if got != c.want {
				t.Errorf("DropTokenQuery(%q, %q, %d) = %q, want %q", c.artist, c.album, c.attempt, got, c.want)
			}
		})
	}
}
