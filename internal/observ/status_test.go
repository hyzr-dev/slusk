package observ

import (
	"testing"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

func TestDashboardStatus(t *testing.T) {
	cases := []struct {
		name string
		v    core.JobView
		want string
	}{
		{
			name: "no transfer yet is queued",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateDiscovered}},
			want: "queued",
		},
		{
			name: "searching with no transfer is queued",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateSearching}},
			want: "queued",
		},
		{
			name: "transfer in progress is active",
			v: core.JobView{
				Job:      core.AlbumJob{State: core.StateDownloading},
				Transfer: &core.Transfer{State: core.TransferInProgress},
			},
			want: "active",
		},
		{
			name: "transfer stalled is stalled",
			v: core.JobView{
				Job:      core.AlbumJob{State: core.StateDownloading},
				Transfer: &core.Transfer{State: core.TransferStalled},
			},
			want: "stalled",
		},
		{
			name: "job completed is done",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateCompleted}},
			want: "done",
		},
		{
			name: "job failed is failed",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateFailed}},
			want: "failed",
		},
		{
			name: "job in cooldown is queued",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateCooldown}},
			want: "queued",
		},
		{
			name: "transfer errored is failed",
			v: core.JobView{
				Job:      core.AlbumJob{State: core.StateDownloading},
				Transfer: &core.Transfer{State: core.TransferErrored},
			},
			want: "failed",
		},
		{
			name: "transfer cancelled is failed",
			v: core.JobView{
				Job:      core.AlbumJob{State: core.StateDownloading},
				Transfer: &core.Transfer{State: core.TransferCancelled},
			},
			want: "failed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dashboardStatus(c.v)
			if got != c.want {
				t.Errorf("dashboardStatus() = %q, want %q", got, c.want)
			}
		})
	}
}
