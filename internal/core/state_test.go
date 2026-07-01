package core

import "testing"

func TestAlbumJobStateTerminal(t *testing.T) {
	cases := []struct {
		state AlbumJobState
		want  bool
	}{
		{StateDiscovered, false},
		{StateDownloading, false},
		{StateCompleted, true},
		{StateFailed, true},
		{StateCooldown, false},
	}
	for _, c := range cases {
		if got := c.state.Terminal(); got != c.want {
			t.Errorf("%s.Terminal() = %v, want %v", c.state, got, c.want)
		}
	}
}
