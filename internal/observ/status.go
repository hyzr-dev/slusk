// Package observ: status.go maps internal job/transfer states to the small
// display vocabulary the dashboard's Queue view uses (queued/active/
// stalled/done/failed/parked), decoupling the UI from the pipeline's state
// machine.
package observ

import "github.com/samuelenocsson/slskdarr/internal/core"

// dashboardStatus derives the dashboard's coarse status label for a job view.
func dashboardStatus(v core.JobView) string {
	switch v.Job.State {
	case core.StateDone:
		return "done"
	case core.StateFailed:
		return "failed"
	case core.StateParked, core.StateOrphaned:
		return "parked"
	// SELECTING is deliberately "queued", not "active": it is the pipeline's
	// waiting room — candidates are cached but the job is waiting for a
	// MaxActive slot (the cap only counts DOWNLOADING+IMPORTING). Counting it
	// as active made the dashboard's Aktiv figure grow far past max_active,
	// which read like a broken cap.
	case core.StateWanted, core.StateSelecting:
		return "queued"
	case core.StateImporting:
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
