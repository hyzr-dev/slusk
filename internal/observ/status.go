// Package observ: status.go maps internal job/transfer states to the small
// display vocabulary the dashboard's Queue view uses (queued/active/
// stalled/done/failed), decoupling the UI from the pipeline's state machine
// (internal/core.AlbumJobState has 7 states; the dashboard needs 5).
package observ

import "github.com/samuelenocsson/slskdarr/internal/core"

// dashboardStatus derives the dashboard's coarse status label for a job view.
func dashboardStatus(v core.JobView) string {
	switch v.Job.State {
	case core.StateDone:
		return "done"
	case core.StateFailed:
		return "failed"
	case core.StateWanted:
		return "queued"
	case core.StateSelecting, core.StateImporting:
		return "active"
	}
	if v.Transfer != nil {
		switch v.Transfer.State {
		case core.TransferStalled:
			return "stalled"
		case core.TransferInProgress:
			return "active"
		case core.TransferErrored, core.TransferCancelled:
			return "failed"
		}
	}
	return "queued"
}
