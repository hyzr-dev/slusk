package pipeline

import "testing"

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
			got := normalizeQuery(tc.input)
			if got != tc.want {
				t.Errorf("normalizeQuery(%q) = %q, want %q", tc.input, got, tc.want)
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
		first := normalizeQuery(in)
		second := normalizeQuery(first)
		if first != second {
			t.Errorf("normalizeQuery not idempotent for %q: first=%q second=%q", in, first, second)
		}
	}
}
