package matcher

import (
	"slices"
	"testing"
)

func TestNormalizeQuery(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "strips trailing parenthesized group",
			input: "X Album (Deluxe Edition)",
			want:  "X Album",
		},
		{
			name:  "strips trailing bracketed group",
			input: "X Album [2011 Remaster]",
			want:  "X Album",
		},
		{
			name:  "strips multiple groups",
			input: "X Album (Deluxe Edition) [2011 Remaster]",
			want:  "X Album",
		},
		{
			name:  "strips nested groups fully",
			input: "Album (Deluxe (Bonus Disc))",
			want:  "Album",
		},
		{
			name:  "dangling unmatched open paren leaves no stray brackets",
			input: "X (A) (B",
			want:  "X B",
		},
		{
			name:  "lone closing bracket is removed",
			input: "X] Album",
			want:  "X Album",
		},
		{
			name:  "query of only bracketed groups normalizes to empty",
			input: "(!!!) [Untitled]",
			want:  "",
		},
		{
			name:  "replaces ampersand with and",
			input: "Simon & Garfunkel Bridge",
			want:  "Simon and Garfunkel Bridge",
		},
		{
			name:  "removes apostrophes",
			input: "Guns N' Roses Don't Cry",
			want:  "Guns N Roses Dont Cry",
		},
		{
			name:  "removes other punctuation and collapses whitespace",
			input: "Artist: Album, Vol. 1!  Really?",
			want:  "Artist Album Vol 1 Really",
		},
		{
			name:  "already-clean query passes through unchanged",
			input: "Radiohead OK Computer",
			want:  "Radiohead OK Computer",
		},
		{
			name:  "empty string stays empty",
			input: "",
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeQuery(tc.input)
			if got != tc.want {
				t.Errorf("NormalizeQuery(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNormalizeQueryIsIdempotent(t *testing.T) {
	inputs := []string{
		"X Album (Deluxe Edition)",
		"Album (Deluxe (Bonus Disc))",
		"X (A) (B",
		"Simon & Garfunkel Bridge",
		"Guns N' Roses Don't Cry",
		"Artist: Album, Vol. 1!  Really?",
		"Radiohead OK Computer",
		"",
	}
	for _, in := range inputs {
		first := NormalizeQuery(in)
		second := NormalizeQuery(first)
		if first != second {
			t.Errorf("NormalizeQuery not idempotent for %q: first=%q second=%q", in, first, second)
		}
	}
}

func TestTokens(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "backslash path separators split into tokens",
			input: `Kansas\The Absence Of Presence (2020)`,
			want:  []string{"kansas", "the", "absence", "of", "presence", "2020"},
		},
		{
			name:  "lowercased",
			input: "THE ABSENCE",
			want:  []string{"the", "absence"},
		},
		{
			name:  "diacritics folded",
			input: "Motörhead",
			want:  []string{"motorhead"},
		},
		{
			name:  "ampersand becomes and",
			input: "Simon & Garfunkel",
			want:  []string{"simon", "and", "garfunkel"},
		},
		{
			name:  "punctuation splits without inventing empty tokens",
			input: "X (A) (B",
			want:  []string{"x", "a", "b"},
		},
		{
			name:  "empty string yields no tokens",
			input: "",
			want:  nil,
		},
		{
			name:  "apostrophe is elided, not split on",
			input: "Don't Cry",
			want:  []string{"dont", "cry"},
		},
		{
			name:  "curly apostrophe is elided too",
			input: "Don’t Cry",
			want:  []string{"dont", "cry"},
		},
		{
			name:  "grave accent standing in for an apostrophe is elided too",
			input: "Don`t Cry",
			want:  []string{"dont", "cry"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokens(tc.input)
			if !slices.Equal(got, tc.want) {
				t.Errorf("tokens(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestIsNoise(t *testing.T) {
	noise := []string{
		"2020", "1999", // year
		"12345",                      // long digits
		"mb1234", "abc123", "xyz45a", // catalogue-ish
		"flac", "mp3", "m4a", // codec
		"v0", "v2", "vbr", "320kbps", "16bit", "lossless", // quality
		"web", "cd", "vinyl", "promo", "rip", // source
		"remaster", "deluxe", "edition", "limited", // edition
		"cd1", "disc2", "disk", "sidea", "pt1", // structure
		"eac", "log", "cue", "nfo", "tracks", // scene/extras
	}
	for _, tok := range noise {
		if !isNoise(tok) {
			t.Errorf("isNoise(%q) = false, want true", tok)
		}
	}

	notNoise := []string{
		"the", "of", "a", "and", "in", // stopwords - CRITICAL, must never be noise
		"absence", "presence", "kansas", "wartorn", "motorhead",
	}
	for _, tok := range notNoise {
		if isNoise(tok) {
			t.Errorf("isNoise(%q) = true, want false (stopwords/content must never be noise)", tok)
		}
	}
}
