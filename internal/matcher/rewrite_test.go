package matcher

import "testing"

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
			name:    "attempt 2 drops the only album token, artist tokens rotate through first",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: 2,
			want:    "Bob Dylan",
		},
		{
			name:    "attempt wraps around modulo total droppable positions",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: 3,
			want:    "Dylan Desire", // same as attempt 0
		},
		{
			name:    "large attempt wraps around",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: 100,
			want:    "Bob Desire", // 100 % 3 == 1, same as attempt 1
		},
		{
			name:    "negative attempt does not panic and wraps around",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: -1,
			want:    "Bob Dylan", // -1 % 3 == -1 -> +3 == 2, same as attempt 2
		},
		{
			name:    "large negative attempt wraps around",
			artist:  "Bob Dylan",
			album:   "Desire",
			attempt: -4,
			want:    "Bob Dylan", // -4 % 3 == -1 -> +3 == 2, same as attempt 2
		},
		{
			name:    "album tokens carry more weight, artist dropped first across multi-word albums",
			artist:  "Bob Dylan",
			album:   "Blood on the Tracks",
			attempt: 0,
			want:    "Dylan Blood on the Tracks",
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

func TestDropTokenQueryNeverPanics(t *testing.T) {
	attempts := []int{-1000, -3, -2, -1, 0, 1, 2, 3, 1000}
	for _, a := range attempts {
		DropTokenQuery("Bob Dylan", "Desire", a)
	}
}
