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
			v:    core.JobView{Job: core.AlbumJob{State: core.StateWanted}},
			want: "queued",
		},
		{
			// SELECTING is the pipeline's waiting room: candidates are cached but
			// the job is waiting for a MaxActive slot. Showing it as "active" made
			// the dashboard's Aktiv count grow past max_active (which only caps
			// DOWNLOADING+IMPORTING), reading like a broken cap.
			name: "selecting is queued (waiting for a MaxActive slot)",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateSelecting}},
			want: "queued",
		},
		{
			name: "importing is active",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateImporting}},
			want: "active",
		},
		{
			name: "downloading with no transfer yet is queued",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateDownloading}},
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
			name: "job done is done",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateDone}},
			want: "done",
		},
		{
			name: "job failed is failed",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateFailed}},
			want: "failed",
		},
		{
			name: "job parked is parked",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateParked}},
			want: "parked",
		},
		{
			name: "legacy orphaned job is canonically parked",
			v:    core.JobView{Job: core.AlbumJob{State: core.StateOrphaned}},
			want: "parked",
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
