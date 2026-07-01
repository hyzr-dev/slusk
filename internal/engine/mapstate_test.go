package engine

import (
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestMapSlskdState(t *testing.T) {
	cases := []struct {
		in   string
		want core.TransferState
	}{
		{"Completed, Succeeded", core.TransferCompleted},
		{"Completed, Cancelled", core.TransferCancelled},
		{"Completed, TimedOut", core.TransferErrored},
		{"Completed, Errored", core.TransferErrored},
		{"Completed, Rejected", core.TransferErrored},
		{"Completed, Aborted", core.TransferErrored},
		{"InProgress", core.TransferInProgress},
		{"Queued, Remotely", core.TransferQueued},
		{"Initializing", core.TransferQueued},
	}
	for _, c := range cases {
		if got := mapSlskdState(c.in); got != c.want {
			t.Errorf("mapSlskdState(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
