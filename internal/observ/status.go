// Package observ: status.go maps internal job/transfer states to the small
// display vocabulary the dashboard's Queue view uses (queued/active/
// stalled/done/failed), decoupling the UI from the engine's richer state
// machine (internal/core.AlbumJobState has 10 states; the dashboard needs 5).
package observ

import "github.com/samuelenocsson/slskdarr/internal/core"

// dashboardStatus derives the dashboard's coarse status label for a job view.
func dashboardStatus(v core.JobView) string {
	switch v.Job.State {
	case core.StateCompleted:
		return "done"
	case core.StateFailed:
		return "failed"
	}
	if v.Transfer != nil {
		switch v.Transfer.State {
		case core.TransferStalled:
			return "stalled"
		case core.TransferInProgress:
			return "active"
		}
	}
	return "queued"
}
