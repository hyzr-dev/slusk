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
// idx.matchFile (see liveTransferIndex in jobdetail.go), never on username
// alone, since a peer serving files for two different albums at once would
// otherwise have both counted on both album rows. Only transfers in a state
// that can still make progress (core.TransferQueued, core.TransferInProgress)
// are counted: a terminal-but-not-yet-reconciled transfer (errored,
// cancelled, completed) lingers in ListDownloads until the pipeline's next
// reconcile pass, and its speed would misrepresent current throughput.
// Returns the summed instantaneous speed and EWMA-smoothed average speed
// (for ETA), the minimum queue position among matched transfers that report
// one (the album's download effectively starts once its first file starts),
// liveBytesDone: the summed live BytesDone across those same matched,
// non-terminal transfers — used by toJobDTO to overlay fresh in-memory bytes
// on top of the persisted, up-to-15s-stale AlbumBytesDone (issue #161) — and
// matched: whether at least one file matched a non-terminal live transfer at
// all, independent of whether that transfer happened to report zero
// speed/bytes. The SSE stream (buildStreamJobs, stream.go) uses matched to
// decide whether this job belongs in the live set at all; toJobDTO
// (observ.go) ignores it, since REST always reports every job regardless of
// live data. candidate == nil (a job with no candidate yet) yields all
// zeros / false.
//
// Remaining bytes are not computed here: callers get them from
// core.JobView.AlbumBytesRemaining (store-computed — see
// internal/store/dashboard.go's jobViewSelect and the field comment on
// AlbumBytesRemaining).
func aggregateLiveAlbum(candidate *core.Candidate, idx liveTransferIndex) (speed, speedAvg int64, queuePosition uint32, hasQueuePosition bool, liveBytesDone int64, matched bool) {
	if candidate == nil {
		return 0, 0, 0, false, 0, false
	}
	for _, f := range candidate.Files {
		lt, ok := idx.matchFile(candidate.Username, f.Filename)
		if !ok {
			continue
		}
		if lt.State != core.TransferQueued && lt.State != core.TransferInProgress {
			continue
		}
		matched = true
		speed += lt.Speed
		speedAvg += lt.SpeedAverage
		liveBytesDone += lt.BytesDone
		if lt.QueuePosition > 0 && (!hasQueuePosition || lt.QueuePosition < queuePosition) {
			queuePosition = lt.QueuePosition
			hasQueuePosition = true
		}
	}
	return speed, speedAvg, queuePosition, hasQueuePosition, liveBytesDone, matched
}

// clampBytesDone bounds an overlaid bytes-done value to at most bytesTotal,
// so a stale or overlapping live sample can never render a progress bar past
// 100% — e.g. a transfer persisted COMPLETED at bytesTotal that briefly
// reappears live (a re-queue) at a smaller in-progress value would otherwise
// sum past its own total via overlayBytesDone's max(). bytesTotal == 0 means
// "unknown", not "empty" (a candidate file's size can be legitimately
// unreported), so it disables the clamp rather than forcing done to 0.
func clampBytesDone(done, bytesTotal int64) int64 {
	if bytesTotal > 0 && done > bytesTotal {
		return bytesTotal
	}
	return done
}

// overlayBytesDone applies the monotone-below, clamped-above live-bytes
// overlay described on jobDTO.BytesDone (issue #161): persistedDone's
// contribution from still-in-flight transfers (persistedNonTerminalDone,
// itself a part of persistedDone) is replaced by their live in-memory
// counterpart (liveDone), then bounded to bytesTotal (see clampBytesDone).
// max() with persistedDone keeps the *lower* bound from ever regressing when
// a non-terminal transfer has no live match (the backend just restarted, or
// the transfer hasn't been enqueued to the peer yet) — the persisted figure
// is always at least as fresh as what the overlay could compute in that
// case. That guarantee is one-sided, though: the live term itself is free to
// drop between ticks (a transfer that reached 500 B can error out of
// ListDownloads and vanish from the live set before persistedNonTerminalDone
// catches up), so the overlay as a whole is not monotone tick-to-tick, only
// bounded below by persistedDone. Shared by toJobDTO (observ.go) and the SSE
// stream (stream.go) so the two transports can never disagree about a given
// job's BytesDone.
func overlayBytesDone(persistedDone, persistedNonTerminalDone, liveDone, bytesTotal int64) int64 {
	overlaid := persistedDone - persistedNonTerminalDone + liveDone
	result := persistedDone
	if overlaid > persistedDone {
		result = overlaid
	}
	return clampBytesDone(result, bytesTotal)
}
