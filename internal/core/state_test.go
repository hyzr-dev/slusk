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

func TestPipelineTerminalStates(t *testing.T) {
	for _, s := range []AlbumJobState{StateDone, StateCancelled, StateFailed} {
		if !s.PipelineTerminal() {
			t.Errorf("%s should be pipeline-terminal", s)
		}
	}
	for _, s := range []AlbumJobState{StateWanted, StateSelecting, StateDownloading, StateImporting} {
		if s.PipelineTerminal() {
			t.Errorf("%s should not be pipeline-terminal", s)
		}
	}
}
