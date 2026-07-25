// Package observ: albumlive.go aggregates live (in-memory, non-persisted)
// transfer data from the peer backend's ListDownloads up to album level for
// the jobs list (GET /api/jobs, issue #157): per-file speed/queue-position is
// summed/mined across every live transfer belonging to a job's current
// candidate, so the list shows one album-level speed and ETA rather than
// requiring the frontend to open the per-job detail panel to see progress.
package observ

import (
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// maxETASeconds clamps etaSeconds so a momentary 1 B/s blip cannot render a
// multi-year ETA; 30 days is generous for a stalled-but-recovering transfer
// while still being an obviously-bogus number the frontend could choose to
// treat specially.
const maxETASeconds = 30 * 24 * 3600

// etaSeconds estimates remaining transfer time from remaining bytes and an
// average speed, or 0 (meaning: omit the field, see jobDTO.ETASeconds) when
// either input makes the estimate meaningless: avgSpeed <= 0 (no live
// throughput to divide by) or remaining <= 0 (nothing left, or unknown).
func etaSeconds(remaining, avgSpeed int64) int64 {
	if avgSpeed <= 0 || remaining <= 0 {
		return 0
	}
	eta := remaining / avgSpeed
	if eta > maxETASeconds {
		return maxETASeconds
	}
	return eta
}

// aggregateLiveAlbum sums the live transfers belonging to candidate's own
// files — matched exactly on (username, filename ∈ candidate.Files), via
// idx.byFallback (see liveTransferIndex in jobdetail.go), never on username
// alone, since a peer serving files for two different albums at once would
// otherwise have both counted on both album rows. Only transfers in a state
// that can still make progress (core.TransferQueued, core.TransferInProgress)
// are counted: a terminal-but-not-yet-reconciled transfer (errored,
// cancelled, completed) lingers in ListDownloads until the pipeline's next
// reconcile pass, and its speed would misrepresent current throughput.
// Returns the summed instantaneous speed and EWMA-smoothed average speed
// (for ETA), and the minimum queue position among matched transfers that
// report one (the album's download effectively starts once its first file
// starts). candidate == nil (a job with no candidate yet) yields all zeros /
// hasQueuePosition false.
//
// Remaining bytes are not computed here: callers get them from
// core.JobView.AlbumBytesRemaining (store-computed — see
// internal/store/dashboard.go's jobViewSelect and the field comment on
// AlbumBytesRemaining).
func aggregateLiveAlbum(candidate *core.Candidate, idx liveTransferIndex) (speed, speedAvg int64, queuePosition uint32, hasQueuePosition bool) {
	if candidate == nil {
		return 0, 0, 0, false
	}
	for _, f := range candidate.Files {
		lt, ok := idx.byFallback[candidate.Username+"\x00"+f.Filename]
		if !ok {
			continue
		}
		if lt.State != core.TransferQueued && lt.State != core.TransferInProgress {
			continue
		}
		speed += lt.Speed
		speedAvg += lt.SpeedAverage
		if lt.QueuePosition > 0 && (!hasQueuePosition || lt.QueuePosition < queuePosition) {
			queuePosition = lt.QueuePosition
			hasQueuePosition = true
		}
	}
	return speed, speedAvg, queuePosition, hasQueuePosition
}
